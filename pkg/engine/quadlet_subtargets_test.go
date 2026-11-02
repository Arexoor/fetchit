package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/spf13/viper"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestQuadletSubtargetHostLifecycle(t *testing.T) {
	r, q := quadletTestRepo(t)
	q.ScanDepth = 1
	q.Restart = true
	abs, _ := filepath.Abs("repo")
	r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{abs}})
	host := t.TempDir()
	bin := t.TempDir()
	log := filepath.Join(host, "operations")
	os.MkdirAll(filepath.Join(host, "usr/lib/systemd/system-generators"), 0755)
	os.WriteFile(filepath.Join(host, "usr/lib/systemd/system-generators/podman-system-generator"), nil, 0755)
	stub := `#!/usr/bin/python3
import os,sys,pathlib
args=sys.argv[4:]
env={}
while '=' in args[0]:
 k,v=args.pop(0).split('=',1);env[k]=v
cmd=args.pop(0)
host=pathlib.Path(os.environ['FETCHIT_QUADLET_HOST_ROOT'])
if cmd=='podman': print('podman version 6.0.0')
elif cmd=='stat': print('cgroup2fs')
elif cmd.endswith('podman-system-generator'):
 for p in (host/env['QUADLET_UNIT_DIRS'].lstrip('/')).iterdir():
  if p.suffix in ('.container','.volume','.network'):
   if 'InvalidForTest' in p.read_text(): sys.exit(1)
   suffix={'.container':'','.volume':'-volume','.network':'-network'}[p.suffix]
   print('---'+p.stem+suffix+'.service---')
elif cmd=='systemctl':
 if args[0]=='--user':args.pop(0)
 if args[0]=='show':
  unit=args[1] if len(args)>1 else ''
  paths=list((host/'etc/containers/systemd').glob('*/'+unit.removesuffix('.service')+'.container'))
  if '--property=SourcePath' in args and paths: print('/'+str(paths[0].relative_to(host)))
  if '--property=LoadState' in args: print('loaded' if paths else 'not-found')
 else:
  with open(os.environ['LOG'],'a') as f:f.write(' '.join(args)+'\n')
  if args[0]=='restart' and (host/'fail').exists(): sys.exit(1)
else: sys.exit(90)
`
	os.WriteFile(filepath.Join(bin, "chroot"), []byte(stub), 0755)
	var lastBatch quadletPlan
	q.runHost = func(_ context.Context, _ context.Context, p quadletPlan) error {
		if p.receipt == nil && len(p.command) > 0 {
			lastBatch = p
		}
		bundle := t.TempDir()
		for name, data := range p.desired.files {
			dest := filepath.Join(bundle, name)
			os.MkdirAll(filepath.Dir(dest), 0755)
			os.WriteFile(dest, data, 0644)
		}
		args := p.command
		if len(args) == 0 {
			args = []string{"-ceu", quadletHostScript, "quadlet", p.parent, p.runtime, p.namespace, "", "", "false", "", p.current, p.desiredRevision, p.configID, "false", "false", "", "", "inspect", ""}
		}
		cmd := exec.Command("sh", args...)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FETCHIT_QUADLET_HOST_ROOT="+host, "FETCHIT_QUADLET_BUNDLE="+bundle, "LOG="+log)
		out, err := cmd.CombinedOutput()
		if p.receipt != nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "FETCHIT_RECEIPT ") {
					*p.receipt = line
				}
			}
		}
		if err != nil {
			return fmt.Errorf("helper: %w: %s", err, out)
		}
		return nil
	}
	files := map[string]string{"README": "idle"}
	poll := func(wantFailure bool) {
		t.Helper()
		h := quadletCommit(t, r, files)
		r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), h))
		err := q.reconcile(context.Background(), context.Background())
		if (err != nil) != wantFailure {
			t.Fatalf("poll failure=%t: %v", wantFailure, err)
		}
	}
	readlog := func() string { b, _ := os.ReadFile(log); os.WriteFile(log, nil, 0644); return string(b) }
	poll(false)
	files["a/a.container"] = "[Container]\nImage=example.com/a\n"
	files["a/data.volume"] = "[Volume]\n"
	files["a/lan.network"] = "[Network]\n"
	files["00-resources/shared.volume"] = "[Volume]\n"
	files["00-resources/shared.network"] = "[Network]\n"
	files["b/b.container"] = "[Container]\nImage=example.com/b\n"
	poll(false)
	if got := readlog(); !strings.Contains(got, "restart -- a.service") || !strings.Contains(got, "restart -- b.service") || !strings.Contains(got, "start -- shared-network.service shared-volume.service") || strings.Index(got, "start -- shared-") > strings.Index(got, "restart -- a.service") || strings.Contains(got, "restart -- shared-") {
		t.Fatalf("initial alphabetical activation/start-only resources: %s", got)
	}
	stale := lastBatch
	files["a/credentials/test.cred"] = "synthetic-one"
	poll(false)
	if err := q.runHost(context.Background(), context.Background(), stale); err == nil || !strings.Contains(err.Error(), "Stale Quadlet parent snapshot") {
		t.Fatalf("old desired plan accepted newer parent state: %v", err)
	}
	if got := readlog(); !strings.Contains(got, "restart -- a.service") || strings.Contains(got, "restart -- b.service") {
		t.Fatalf("group isolation: %s", got)
	}
	files["README"] = "outside-change"
	poll(false)
	if got := readlog(); strings.Contains(got, "restart") || strings.Contains(got, "daemon-reload") {
		t.Fatalf("outside files restarted groups: %s", got)
	}
	delete(files, "a/a.container")
	files["bad/bad.container"] = "[Container]\nInvalidForTest=true\n"
	poll(true)
	if got := readlog(); strings.Contains(got, "stop") {
		t.Fatalf("bad candidate removed valid group: %s", got)
	}
	delete(files, "bad/bad.container")
	files["collision/b.container"] = files["b/b.container"]
	poll(true)
	if got := readlog(); strings.Contains(got, "stop") {
		t.Fatalf("duplicate service candidate removed valid group: %s", got)
	}
	delete(files, "collision/b.container")
	external := filepath.Join(host, "etc/containers/systemd/unowned/external.container")
	os.MkdirAll(filepath.Dir(external), 0755)
	os.WriteFile(external, []byte("external"), 0644)
	files["external/external.container"] = "[Container]\nImage=example.com/external\n"
	poll(true)
	if got := readlog(); strings.Contains(got, "stop") {
		t.Fatalf("external ownership conflict removed valid group: %s", got)
	}
	delete(files, "external/external.container")
	delete(files, "bad/bad.container")
	files["README"] = "retry"
	poll(false)
	if got := readlog(); !strings.Contains(got, "stop a.service") || strings.Contains(got, "stop b.service") {
		t.Fatalf("removed-container cleanup isolation: %s", got)
	}
	aChild := q.subtargetAt("bundle/a")
	aNamespace, _ := aChild.hostIdentity()
	liveA := filepath.Join(host, "etc/containers/systemd", aNamespace)
	for _, file := range []string{"data.volume", "lan.network", "credentials/test.cred"} {
		if _, err := os.Stat(filepath.Join(liveA, file)); err != nil {
			t.Fatalf("last container removal retired resource group %s: %v", file, err)
		}
	}
	delete(files, "a/data.volume")
	poll(false)
	if _, err := os.Stat(filepath.Join(liveA, "lan.network")); err != nil {
		t.Fatal("network-only group retired", err)
	}
	delete(files, "a/lan.network")
	poll(false)
	if _, err := os.Stat(filepath.Join(liveA, "credentials/test.cred")); !os.IsNotExist(err) {
		t.Fatal("final recognized unit removal did not retire auxiliary content", err)
	}
	readlog()
	// Different interrupted transactions must never reuse the parent's CAS token.
	os.WriteFile(filepath.Join(host, "fail"), nil, 0644)
	files["c/c.container"] = "[Container]\nImage=example.com/c\n"
	poll(true)
	ns, _ := q.hostIdentity()
	inventoryPath := filepath.Join(host, "etc/containers/."+ns, "groups")
	one, _ := os.ReadFile(inventoryPath)
	files["c/config"] = "changed during retry"
	poll(true)
	two, _ := os.ReadFile(inventoryPath)
	if string(one) == string(two) {
		t.Fatal("interrupted parent transactions reused CAS token")
	}
	os.Remove(filepath.Join(host, "fail"))
	delete(files, "c/c.container")
	files["README"] = "restart-after-partial"
	restarted := *q
	q = &restarted
	poll(false)
	if got := readlog(); !strings.Contains(got, "stop c.service") {
		t.Fatalf("failed new child leaked after controller restart: %s", got)
	}
	files["renamed/b.container"] = files["b/b.container"]
	delete(files, "b/b.container")
	poll(false)
	if got := readlog(); !strings.Contains(got, "stop b.service") || !strings.Contains(got, "restart -- b.service") {
		t.Fatalf("rename must remove/add: %s", got)
	}
	q.ScanDepth = 2
	files["deep/d/d.container"] = "[Container]\nImage=example.com/d\n"
	poll(false)
	readlog()
	q.ScanDepth = 1
	files["README"] = "decrease-depth"
	poll(false)
	if got := readlog(); !strings.Contains(got, "stop d.service") {
		t.Fatalf("depth decrease must retire old group: %s", got)
	}
	q.CleanupOnRemoval = true
	receipt, _, err := methodRemovalReceipt(q)
	if err != nil {
		t.Fatal(err)
	}
	// The scan parent's cleanup intent must identify subgroup inventory ownership.
	if receipt.Quadlet == nil || !strings.HasSuffix(receipt.Quadlet.Namespace, "-scan") {
		t.Fatal("scan cleanup namespace missing")
	}
	cleanup, ok := interface{}(q).(interface{ subtargetRemovalPlan() (quadletPlan, error) })
	if !ok {
		t.Fatal("scan parent has no inventory-scoped cleanup")
	}
	plan, err := cleanup.subtargetRemovalPlan()
	if err != nil {
		t.Fatal(err)
	}
	if err := q.runHost(context.Background(), context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if got := readlog(); !strings.Contains(got, "stop b.service") {
		t.Fatalf("parent removal leaked child: %s", got)
	}
}

func TestQuadletSubtargetEmptyParentKeepsPolling(t *testing.T) {
	r, q := quadletTestRepo(t)
	q.ScanDepth = 1
	abs, _ := filepath.Abs("repo")
	if _, err := r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{abs}}); err != nil {
		t.Fatal(err)
	}
	first := quadletCommit(t, r, map[string]string{"README": "idle"})
	r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), first))
	q.runHost = func(_ context.Context, _ context.Context, p quadletPlan) error {
		if p.receipt != nil {
			*p.receipt = "FETCHIT_RECEIPT absent"
		}
		return nil
	}
	ctx := context.Background()
	if err := q.reconcile(ctx, ctx); err != nil {
		t.Fatalf("empty parent must be idle, not fail: %v", err)
	}
	next := quadletCommit(t, r, map[string]string{"a/a.container": "[Container]\nImage=example.com/a\n"})
	r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), next))
	if err := q.reconcile(ctx, ctx); err != nil {
		t.Fatalf("new direct child must be discovered on next poll: %v", err)
	}
	got, err := getCurrent(q.target, q.GetKind(), q.GetName())
	if err != nil || got != next {
		t.Fatalf("parent did not advance: %v %v", got, err)
	}
}

func TestQuadletSubtargetRejectInventoryTrailer(t *testing.T) {
	_, q := quadletTestRepo(t)
	q.ScanDepth = 1
	data := []byte(`{"version":1,"epoch":"11111111111111111111111111111111","applied":"0000000000000000000000000000000000000000","paths":[]} {}`)
	sum := sha256.Sum256(data)
	q.runHost = func(_ context.Context, _ context.Context, p quadletPlan) error {
		if p.receipt != nil {
			*p.receipt = fmt.Sprintf("FETCHIT_RECEIPT %x %s", sum, base64.StdEncoding.EncodeToString(data))
		}
		return nil
	}
	if err := q.reconcile(context.Background(), context.Background()); err == nil || !strings.Contains(err.Error(), "invalid subgroup inventory") {
		t.Fatalf("malformed inventory accepted: %v", err)
	}
}

func TestQuadletSubtargetRejectUnsafeDiscoveredPath(t *testing.T) {
	r, q := quadletTestRepo(t)
	q.ScanDepth = 1
	h := quadletCommit(t, r, map[string]string{"badpath/a.container": "[Container]\nImage=example.test/a\n"})
	commit, err := r.CommitObject(h)
	if err != nil {
		t.Fatal(err)
	}
	root, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	tree, err := root.Tree("bundle")
	if err != nil {
		t.Fatal(err)
	}
	tree.Entries[0].Name = "bad\npath"
	store := func(value interface {
		Encode(plumbing.EncodedObject) error
	}) plumbing.Hash {
		obj := r.Storer.NewEncodedObject()
		if err := value.Encode(obj); err != nil {
			t.Fatal(err)
		}
		hash, err := r.Storer.SetEncodedObject(obj)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	hash := store(tree)
	for i := range root.Entries {
		if root.Entries[i].Name == "bundle" {
			root.Entries[i].Hash = hash
		}
	}
	commit.TreeHash = store(root)
	h = store(commit)
	if _, err := q.discoverSubtargets(h.String()); err == nil {
		t.Fatal("unsafe discovered path would poison persisted ownership")
	}
}

func TestQuadletSubtargetOwnedPathValidation(t *testing.T) {
	for _, tc := range []struct {
		parent, child string
		want          bool
	}{{".", "a", true}, {".", "..hidden", true}, {"./services", "services/a", true}, {"services", "services/a", true}, {"services", "services/../outside", false}, {"services", "services", false}, {".", "../outside", false}} {
		_, q := quadletTestRepo(t)
		q.TargetPath = tc.parent
		checker, ok := interface{}(q).(interface{ ownsSubtargetPath(string) bool })
		if !ok {
			t.Fatal("missing scoped inventory path validator")
		}
		if got := checker.ownsSubtargetPath(tc.child); got != tc.want {
			t.Fatalf("%+v: %t", tc, got)
		}
	}
}

func TestQuadletSubtargetStrictDepthConfig(t *testing.T) {
	for _, value := range []string{"-1", "1.5", "true", "'1'"} {
		data := []byte("targetConfigs:\n- url: example.com/repo\n  quadlet:\n  - name: app\n    targetPath: services\n    root: true\n    scanDepth: " + value + "\n")
		if err := decodeConfigExactly(data); err == nil {
			t.Errorf("unsupported scanDepth %s accepted", value)
		}
	}
}

func TestQuadletSubtargetConfigValidation(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader("targetConfigs:\n- url: example.com/repo\n  quadlet:\n  - name: app\n    schedule: '* * * * *'\n    targetPath: service\n    root: true\n    scanDepth: -1\n")); err != nil {
		t.Fatal(err)
	}
	var cfg FetchitConfig
	if err := v.Unmarshal(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := validateLifecycleConfig(&cfg); err == nil {
		t.Fatal("negative scanDepth accepted")
	}
	q := cfg.TargetConfigs[0].Quadlet[0]
	q.ScanDepth = 0
	baseline := q.GetName()
	q.ScanDepth = 1
	if q.GetName() == baseline {
		t.Fatal("scanDepth shares exact-mode applied state")
	}
	q.target.rollback = true
	if _, _, err := q.paths(); err == nil {
		t.Fatal("scan mode silently ignores rollback policy")
	}
	q.target.rollback = false
	if q.GetTargetPath() != "service" {
		t.Fatal("configured parent path changed")
	}
}

func TestQuadletSubtargetIdentityAndQuoting(t *testing.T) {
	_, q := quadletTestRepo(t)
	q.Name = strings.Repeat("a", 64)
	a, b := q.subtargetAt("bundle/a-b"), q.subtargetAt("bundle/a/b")
	if a.Name == b.Name || len(a.Name) > 64 || a.Name != q.subtargetAt("bundle/a-b").Name {
		t.Fatal("child identity not bounded/stable/collision safe")
	}
	input := "path with ' quote; $(touch DO_NOT_CREATE)"
	out, err := exec.Command("sh", "-ceu", "printf '%s' "+shellData(input)).CombinedOutput()
	if err != nil || string(out) != input {
		t.Fatalf("shell data escaped incorrectly: %q %v", out, err)
	}
	if _, err := os.Stat("DO_NOT_CREATE"); !os.IsNotExist(err) {
		t.Fatal("shell interpolation executed")
	}
}

func TestQuadletSubtargetDiscoveryDepth(t *testing.T) {
	r, q := quadletTestRepo(t)
	revision := quadletCommit(t, r, map[string]string{
		"root.container":                    "[Container]\nImage=example.com/root\n",
		"a/a.container":                     "[Container]\nImage=example.com/a\n",
		"a/nested/hidden.container":         "[Container]\nImage=example.com/hidden\n",
		"branch/b/b.container":              "[Container]\nImage=example.com/b\n",
		"volume-only/data.volume":           "[Volume]\n",
		"network-only/lan.network":          "[Network]\n",
		"mixed/app.container":               "[Container]\nImage=example.com/app\n",
		"mixed/data.volume":                 "[Volume]\n",
		"mixed/lan.network":                 "[Network]\n",
		"branch/resources/data.volume":      "[Volume]\n",
		"config-only/app.conf":              "auxiliary",
		"dropins/app.container.d/10.conf":   "auxiliary",
		"other/app.pod":                     "[Pod]\n",
		"volume-only/nested/hidden.network": "[Network]\n",
	})
	if _, err := q.subtargetAt("bundle/a").bundle(revision); err == nil {
		t.Fatal("nested source units inside selected group must still fail validation")
	}
	for _, tc := range []struct {
		depth int
		want  []string
	}{
		{0, []string{"bundle"}},
		{1, []string{"bundle/a", "bundle/mixed", "bundle/network-only", "bundle/volume-only"}},
		{2, []string{"bundle/a", "bundle/branch/b", "bundle/branch/resources", "bundle/mixed", "bundle/network-only", "bundle/volume-only"}},
	} {
		// Reflection keeps RED a behavioral failure on the unextended config type.
		field := reflect.ValueOf(q).Elem().FieldByName("ScanDepth")
		if !field.IsValid() {
			t.Fatal("Quadlet lacks configurable scanDepth")
		}
		field.SetInt(int64(tc.depth))
		scanner, ok := interface{}(q).(interface {
			discoverSubtargets(string) ([]string, error)
		})
		if !ok {
			t.Fatal("Quadlet lacks Git-tree subgroup discovery")
		}
		got, err := scanner.discoverSubtargets(revision.String())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("depth %d: got %v want %v", tc.depth, got, tc.want)
		}
	}
}
