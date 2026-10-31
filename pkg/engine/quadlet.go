package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/specgen"
	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/opencontainers/runtime-spec/specs-go"
)

const quadletMethod = "quadlet"
const quadletMaxBytes int64 = 32 << 20
const quadletMaxFiles = 4096

//go:embed quadlet-host.sh
var quadletHostScript string

// Quadlet deploys a dedicated Git directory as a host-managed Quadlet bundle.
// Rootless deployments require explicit host paths; container HOME/UID are not used.
type Quadlet struct {
	CommonMethod    `mapstructure:",squash"`
	Root            bool   `mapstructure:"root"`
	Start           bool   `mapstructure:"start"`
	Restart         bool   `mapstructure:"restart"`
	HostHome        string `mapstructure:"hostHome"`
	HostConfigHome  string `mapstructure:"hostConfigHome"`
	HostRuntimeDir  string `mapstructure:"hostRuntimeDir"`
	HelperImage     string `mapstructure:"helperImage"`
	runHost         func(context.Context, context.Context, quadletPlan) error
	dirty           bool
	forceReconcile  bool
	expectedReceipt string
}

type quadletBundle struct {
	// Git tree identity covers recursive content, paths, and executable modes.
	treeID          plumbing.Hash
	files           map[string][]byte
	modes           map[string]int64
	services        []string
	restartServices []string
}

type quadletPlan struct {
	// receipt selects read-only inspection; expectedReceipt binds apply to it.
	receipt         *string
	expectedReceipt string
	unchangedTree   bool
	removal         bool
	previous        quadletBundle
	desired         quadletBundle
	parent          string
	runtime         string
	namespace       string
	home            string
	current         string
	desiredRevision string
	configID        string
	lifecycleID     string
}

var quadletSafeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:@-]*$`)

func (q *Quadlet) GetKind() string { return quadletMethod }

// GetName includes bundle identity and settings so config reloads have distinct
// applied tags, while host ownership remains stable across activation changes.
func (q *Quadlet) GetName() string {
	return q.settingsID(q.HelperImage)
}

// The helper is only a transport for the controller-embedded script. It is not
// workload configuration. Keep its reference in the applied tag, but allow a
// verified host receipt to adopt a helper-only change without restarting apps.
func (q *Quadlet) settingsID(helper string) string {
	url, branch := "", ""
	if q.target != nil {
		url = q.target.url
		branch = q.target.branch
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%t\x00%s\x00%s\x00%s\x00%t\x00%t\x00%s", url, branch, q.GetTargetPath(), q.Root, q.HostHome, q.HostConfigHome, q.HostRuntimeDir, q.Start, q.Restart, helper)))
	return fmt.Sprintf("%s-%x", q.Name, sum[:6])
}

// hostBaseline reads a locked host receipt before planning. The receipt is
// authoritative only inside this method's ownership namespace, and its commit
// and recorded tree must validate locally. No stale-plan failure is retried
// using a new snapshot within this reconciliation.
func (q *Quadlet) hostBaseline(ctx, conn context.Context) (plumbing.Hash, string, error) {
	parent, runtime, err := q.paths()
	if err != nil {
		return plumbing.ZeroHash, "", err
	}
	namespace, home := q.hostIdentity()
	var receipt string
	run := q.runHost
	if run == nil {
		run = q.deploy
	}
	if err := run(ctx, conn, quadletPlan{parent: parent, runtime: runtime, namespace: namespace, home: home, receipt: &receipt}); err != nil {
		return plumbing.ZeroHash, "", err
	}
	invalid := fmt.Errorf("invalid Quadlet recovery receipt")
	if len(receipt) > 640 {
		return plumbing.ZeroHash, "", invalid
	}
	fields := strings.Fields(receipt)
	if len(fields) == 2 && fields[0] == "FETCHIT_RECEIPT" && fields[1] == "absent" {
		return plumbing.ZeroHash, "absent", nil
	}
	if (len(fields) != 4 && len(fields) != 6) || fields[0] != "FETCHIT_RECEIPT" {
		return plumbing.ZeroHash, "", invalid
	}
	hex40 := regexp.MustCompile(`^[0-9a-f]{40}$`)
	identity := regexp.MustCompile(`^` + regexp.QuoteMeta(q.Name) + `-[0-9a-f]{12}$`)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fields[1]) || !hex40.MatchString(fields[2]) || !identity.MatchString(fields[3]) {
		return plumbing.ZeroHash, "", invalid
	}
	revision := plumbing.NewHash(fields[2])
	bundle, err := q.bundle(revision)
	if err != nil {
		return plumbing.ZeroHash, "", fmt.Errorf("Quadlet recovery commit unavailable or invalid")
	}
	if len(fields) == 6 && (!identity.MatchString(fields[4]) || !hex40.MatchString(fields[5]) || bundle.treeID.String() != fields[5] || (fields[3] == q.GetName() && fields[4] != q.settingsID(""))) {
		return plumbing.ZeroHash, "", invalid
	}
	// A legacy receipt has no tree/lifecycle proof: only the exact config may
	// recover it; activation still goes through the normal locked helper.
	if len(fields) == 4 && fields[3] != q.GetName() {
		return plumbing.ZeroHash, "", invalid
	}
	return revision, fields[1], nil
}

func (q *Quadlet) reconcile(ctx, conn context.Context) error {
	target := q.GetTarget()
	if target.disconnected {
		if target.url != "" {
			if err := extractZip(target.url); err != nil {
				return fmt.Errorf("refreshing disconnected archive: %w", err)
			}
		} else if target.device != "" {
			localDevicePull(getDirectory(target), target.device, "", false)
		}
	}
	// Snapshot before selecting desired: an overlapping old poll must not
	// adopt a newer receipt after it already selected an older desired commit.
	baseline, token, err := q.hostBaseline(ctx, conn)
	if err != nil {
		return err
	}
	latest, err := getLatest(target)
	if err != nil {
		return err
	}
	observeGit(ctx, "unknown", true)
	current, err := getCurrent(target, q.GetKind(), q.GetName())
	if err != nil {
		return err
	}
	observeGit(ctx, "out_of_sync", false)
	appliedTag := current
	if token != "absent" {
		current = baseline
	}
	q.expectedReceipt = token
	// Even equal commits must check the snapshot and pending journal under lock.
	q.forceReconcile = true
	defer func() { q.forceReconcile = false; q.expectedReceipt = "" }()
	err = applyWithRecovery(ctx, conn, q, current, latest, nil)
	if err != nil {
		return err
	}
	if latest != appliedTag {
		if err := updateCurrent(ctx, target, latest, q.GetKind(), q.GetName()); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	observeGit(ctx, "synced", false)
	return nil
}

func (q *Quadlet) Process(ctx, conn context.Context, skew int) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(skew) * time.Millisecond):
	}
	target := q.GetTarget()
	target.mu.Lock()
	defer target.mu.Unlock()
	// Unlike legacy methods, do not replay the saved commit on startup: the host
	// files survive Fetchit restarts, and the saved commit is the cleanup baseline.
	if q.initialRun {
		if err := getRepo(target); err != nil {
			logger.Errorf("Quadlet clone: %v", err)
			return
		}
	}
	if err := q.reconcile(ctx, conn); err != nil {
		logger.Errorf("Quadlet %s: %v", q.Name, err)
		return
	}
	q.initialRun = false
}

// Quadlet operations are deliberately batched, never dispatched per file.
func (q *Quadlet) MethodEngine(context.Context, context.Context, *object.Change, string) error {
	return fmt.Errorf("Quadlet requires bundle reconciliation through Apply")
}

func (q *Quadlet) paths() (string, string, error) {
	if len(q.Name) > 64 || !quadletSafeName.MatchString(q.Name) || strings.Contains(q.Name, "@") || strings.Contains(q.Name, ":") {
		return "", "", fmt.Errorf("invalid Quadlet method name %q", q.Name)
	}
	if q.GetTargetPath() == "" || path.Clean(q.GetTargetPath()) != q.GetTargetPath() || path.IsAbs(q.GetTargetPath()) || strings.HasPrefix(q.GetTargetPath(), "..") {
		return "", "", fmt.Errorf("Quadlet requires a relative targetPath directory")
	}
	if q.Glob != nil {
		return "", "", fmt.Errorf("Quadlet glob is unsupported: targetPath must select a complete bundle directory")
	}
	if q.Root {
		return "/etc/containers", "", nil
	}
	for _, p := range []string{q.HostHome, q.HostConfigHome, q.HostRuntimeDir} {
		if !path.IsAbs(p) || path.Clean(p) != p || p == "/" || strings.ContainsAny(p, "\n\r") {
			return "", "", fmt.Errorf("rootless Quadlet requires absolute hostHome, hostConfigHome and hostRuntimeDir paths")
		}
	}
	return q.HostConfigHome, q.HostRuntimeDir, nil
}

func (q *Quadlet) bundle(hash plumbing.Hash) (quadletBundle, error) {
	b := quadletBundle{files: map[string][]byte{}, modes: map[string]int64{}}
	if hash.IsZero() {
		return b, nil
	}
	repo, err := git.PlainOpen(getDirectory(q.target))
	if err != nil {
		return b, err
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return b, err
	}
	root, err := commit.Tree()
	if err != nil {
		return b, err
	}
	tree := root
	if q.GetTargetPath() != "." {
		tree, err = root.Tree(q.GetTargetPath())
	}
	if errors.Is(err, object.ErrDirectoryNotFound) {
		return b, nil
	}
	if err != nil {
		return b, err
	}

	b.treeID = tree.Hash
	var total int64
	err = tree.Files().ForEach(func(f *object.File) error {
		if f.Mode != filemode.Regular && f.Mode != filemode.Executable {
			return fmt.Errorf("Quadlet bundle requires regular files: %s", f.Name)
		}
		if path.Clean(f.Name) != f.Name || path.IsAbs(f.Name) || strings.HasPrefix(f.Name, "../") || strings.ContainsAny(f.Name, "\n\r") {
			return fmt.Errorf("invalid bundle path %q", f.Name)
		}
		if len(b.files) >= quadletMaxFiles || f.Size > quadletMaxBytes-total {
			return fmt.Errorf("Quadlet bundle exceeds limit: %d files or %d bytes", quadletMaxFiles, quadletMaxBytes)
		}
		total += f.Size
		content, err := f.Contents()
		if err != nil {
			return err
		}
		b.files[f.Name] = []byte(content)
		b.modes[f.Name] = 0644
		if f.Mode == filemode.Executable {
			b.modes[f.Name] = 0755
		}
		return nil
	})
	if err != nil {
		return b, err
	}
	names := map[string]string{}
	for filename, content := range b.files {
		if !quadlet.IsExtSupported(filename) {
			continue
		}
		if path.Dir(filename) != "." {
			return b, fmt.Errorf("Quadlet source units must be at bundle root: %s", filename)
		}
		if !quadletSafeName.MatchString(strings.TrimSuffix(filename, path.Ext(filename))) {
			return b, fmt.Errorf("invalid Quadlet filename %q", filename)
		}
		u := parser.NewUnitFile()
		u.Filename = filename
		if err := u.Parse(string(content)); err != nil {
			return b, fmt.Errorf("parse %s: %w", filename, err)
		}
		// Match upstream drop-in specificity and alphabetical merge order.
		dropins := map[string][]byte{}
		for _, dir := range u.GetUnitDropinPaths() {
			for name, data := range b.files {
				if path.Dir(name) == dir && path.Ext(name) == ".conf" {
					if _, exists := dropins[path.Base(name)]; !exists {
						dropins[path.Base(name)] = data
					}
				}
			}
		}
		keys := make([]string, 0, len(dropins))
		for name := range dropins {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			drop := parser.NewUnitFile()
			if err := drop.Parse(string(dropins[name])); err != nil {
				return b, fmt.Errorf("parse drop-in %s: %w", name, err)
			}
			u.Merge(drop)
		}
		name, err := quadlet.GetUnitServiceName(u)
		if err != nil {
			return b, err
		}
		if !quadletSafeName.MatchString(name) || strings.HasSuffix(name, "@") {
			return b, fmt.Errorf("invalid or uninstantiated Quadlet service %q", name)
		}
		name += ".service"
		if prev, exists := names[name]; exists {
			return b, fmt.Errorf("duplicate service %s in %s and %s", name, prev, filename)
		}
		names[name] = filename
		b.services = append(b.services, name)
		if path.Ext(filename) != ".network" && path.Ext(filename) != ".volume" {
			b.restartServices = append(b.restartServices, name)
		}
	}

	sort.Strings(b.services)
	sort.Strings(b.restartServices)
	return b, err
}

func (q *Quadlet) Apply(ctx, conn context.Context, current, desired plumbing.Hash, _ *[]string) error {
	if desired.IsZero() && current.IsZero() {
		return nil
	}
	parent, runtime, err := q.paths()
	if err != nil {
		return err
	}
	old, err := q.bundle(current)
	if err != nil {
		return err
	}
	next, err := q.bundle(desired)
	if err != nil {
		return err
	}
	if len(next.services) == 0 && current.IsZero() {
		return fmt.Errorf("Quadlet bundle contains no supported source units")
	}
	if current == desired && !q.forceReconcile {
		return nil
	}

	namespace, home := q.hostIdentity()
	plan := quadletPlan{expectedReceipt: q.expectedReceipt, unchangedTree: !current.IsZero() && old.treeID == next.treeID, previous: old, desired: next, parent: parent, runtime: runtime, namespace: namespace, home: home, current: current.String(), desiredRevision: desired.String(), configID: q.GetName(), lifecycleID: q.settingsID("")}
	run := q.runHost
	if run == nil {
		run = q.deploy
	}
	q.dirty = true
	err = run(ctx, conn, plan)
	q.dirty = err != nil
	return err
}

func (q *Quadlet) hostIdentity() (string, string) {
	sum := sha256.Sum256([]byte(q.target.url + "\x00" + q.target.branch + "\x00" + q.Name + "\x00" + q.GetTargetPath()))
	home := q.HostHome
	if q.Root {
		home = "/root"
	}
	return fmt.Sprintf("fetchit-%s-%x", q.Name, sum[:6]), home
}

func quadletArchive(files map[string][]byte, modes map[string]int64) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "quadlet-bundle/", Mode: 0755, Typeflag: tar.TypeDir}); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: "quadlet-bundle/" + name, Mode: modes[name], Size: int64(len(data))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (q *Quadlet) deploy(ctx, conn context.Context, p quadletPlan) (resultErr error) {
	archive, err := quadletArchive(p.desired.files, p.desired.modes)
	if err != nil {
		return err
	}
	image := q.HelperImage
	if image == "" {
		image = fetchitImage
	}
	if err := detectOrFetchImage(conn, image, false); err != nil {
		return err
	}
	s := specgen.NewSpecGenerator(image, false)
	s.CapDrop = []string{"ALL"}
	s.CapAdd = []string{"SYS_CHROOT", "DAC_OVERRIDE", "FOWNER", "CHOWN"}
	s.SelinuxOpts = []string{"disable"}
	s.PidNS = specgen.Namespace{NSMode: "host"}
	// Host binaries/generator and their libraries remain read-only. Only the
	// containers/config directory is writable; systemd uses its private socket.
	s.Mounts = []specs.Mount{
		{Source: "/", Destination: "/host", Type: define.TypeBind, Options: []string{"ro", "rbind", "rslave"}},
		{Source: p.parent, Destination: "/host" + p.parent, Type: define.TypeBind, Options: []string{"rw"}},
	}
	s.Entrypoint = []string{"/bin/sh"}
	restartUnits := ""
	if q.Restart {
		restartUnits = strings.Join(p.desired.restartServices, "\n")
	}
	operation := "apply"
	if p.receipt != nil {
		operation = "inspect"
	}
	s.Command = []string{"-ceu", quadletHostScript, "quadlet", p.parent, p.runtime, p.namespace, strings.Join(p.previous.services, "\n"), strings.Join(p.desired.services, "\n"), fmt.Sprint(q.Start || q.Restart), restartUnits, p.current, p.desiredRevision, p.configID, fmt.Sprint(p.removal), fmt.Sprint(p.unchangedTree), p.lifecycleID, p.desired.treeID.String(), operation, p.expectedReceipt}
	s.Env = map[string]string{"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "HOME": p.home}
	if p.runtime != "" {
		s.Env["XDG_CONFIG_HOME"] = p.parent
		s.Env["XDG_RUNTIME_DIR"] = p.runtime
	}
	created, err := containers.CreateWithSpec(conn, s, nil)
	if err != nil {
		return err
	}
	defer func() {
		reports, err := containers.Remove(conn, created.ID, new(containers.RemoveOptions).WithForce(true))
		resultErr = errors.Join(resultErr, err)
		for _, report := range reports {
			if report != nil {
				resultErr = errors.Join(resultErr, report.Err)
			}
		}
	}()

	copyBundle, err := containers.CopyFromArchive(conn, created.ID, "/tmp", bytes.NewReader(archive))
	if err != nil {
		return err
	}
	if err := copyBundle(); err != nil {
		return err
	}

	if err := containers.Start(conn, created.ID, nil); err != nil {
		return err
	}
	code, err := containers.Wait(conn, created.ID, new(containers.WaitOptions).WithCondition([]define.ContainerStatus{define.ContainerStateExited}))
	if err != nil {
		return err
	}
	// Drain without logging raw host output: generator/systemd diagnostics can
	// contain inline credentials. Inspection accepts only a bounded protocol
	// record, validated by hostBaseline; application emits a generic status.
	logs := make(chan string)
	done := make(chan struct{})
	go func() {
		defer close(done)
		seen := false
		for line := range logs {
			if p.receipt != nil && strings.HasPrefix(line, "FETCHIT_RECEIPT ") {
				if seen || len(line) > 640 {
					*p.receipt = "invalid"
				} else {
					*p.receipt = strings.TrimSpace(line)
				}
				seen = true
			}
		}
	}()
	logErr := containers.Logs(conn, created.ID, new(containers.LogOptions).WithStdout(true).WithStderr(true), logs, logs)
	close(logs)
	<-done
	if code != 0 {
		return fmt.Errorf("Quadlet helper exited with status %d; inspect private host diagnostics", code)
	}
	return logErr
}
