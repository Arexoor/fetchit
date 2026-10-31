//go:build quadlet_folder_integration

package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Explicit opt-in; uses NEW names only. Never reads credentials, pulls images,
// publishes ports, touches live apps, or removes/prunes volumes (even its own).
func TestQuadletFolderIntegration(t *testing.T) {
	image := os.Getenv("QUADLET_HELPER_IMAGE")
	if image == "" {
		t.Skip("QUADLET_HELPER_IMAGE required")
	}
	if os.Getuid() != 1000 {
		t.Fatal("requires the approved rootless core UID 1000")
	}
	ctx := context.Background()
	r, a := quadletTestRepo(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("fetchit-folder-proof-%d-%d", os.Getpid(), time.Now().Unix())
	a.Name = prefix + "-a"
	a.Root = false
	a.Start = true
	a.Restart = true
	a.HostHome = home
	a.HostConfigHome = filepath.Join(home, ".config")
	a.HostRuntimeDir = os.Getenv("XDG_RUNTIME_DIR")
	a.HelperImage = image
	bcopy := *a
	b := &bcopy
	b.Name = prefix + "-b"
	b.TargetPath = "bundle-b"
	conn, err := bindings.NewConnection(ctx, "unix://"+a.HostRuntimeDir+"/podman/podman.sock")
	if err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if command("podman", "info", "--format={{.Host.CgroupManager}}") != "systemd" {
		t.Fatal("requires systemd cgroups")
	}
	command("podman", "image", "exists", image)
	command("podman", "image", "exists", "docker.io/library/alpine:3.23")
	names := []string{prefix + "-a1", prefix + "-a2", prefix + "-b1"}
	source := func(name string) string {
		return "[Container]\nImage=docker.io/library/alpine:3.23\nPull=never\nContainerName=" + name + "\nNetwork=none\nReadOnly=true\nExec=sleep infinity\n[Service]\nTimeoutStartSec=30\n"
	}
	files := map[string]string{
		"bundle/" + names[0] + ".container":     source(names[0]),
		"bundle/" + names[1] + ".container":     source(names[1]),
		"bundle-b/" + names[2] + ".container":   source(names[2]),
		"bundle/nested/config":                  "public config one",
		"bundle/credentials/public-marker.cred": "synthetic ciphertext; not a real credential",
		"bundle/" + prefix + ".volume":          "[Volume]\nLabel=proof=one\n",
	}
	write := func(name, value string) {
		t.Helper()
		p := filepath.Join("repo", name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range files {
		write(name, value)
	}
	commit := func(message string) plumbing.Hash {
		t.Helper()
		w, err := r.Worktree()
		if err != nil {
			t.Fatal(err)
		}
		if err := w.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			t.Fatal(err)
		}
		h, err := w.Commit(message, &git.CommitOptions{Author: &object.Signature{Name: "Disposable proof", Email: "test@example.invalid", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), h)); err != nil {
			t.Fatal(err)
		}
		return h
	}
	first := commit("initial two isolated bundles")
	abs, err := filepath.Abs("repo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{abs}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []*Quadlet{a, b} {
			ns, home := q.hostIdentity()
			parent, runtime, err := q.paths()
			if err != nil {
				t.Error(err)
				continue
			}
			// Dedicated removal mode reads only this test namespace's ownership journal.
			err = q.deploy(ctx, conn, quadletPlan{removal: true, parent: parent, runtime: runtime, namespace: ns, home: home, current: strings.Repeat("0", 40), desiredRevision: strings.Repeat("0", 40), configID: q.GetName(), desired: quadletBundle{files: map[string][]byte{}, modes: map[string]int64{}}})
			if err != nil {
				t.Errorf("cleanup %s: %v", ns, err)
				continue
			}
			for _, p := range []string{filepath.Join(parent, "containers/systemd", ns), filepath.Join(parent, "."+ns)} {
				if err := os.RemoveAll(p); err != nil {
					t.Error(err)
				}
			}
		}
		if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			t.Errorf("cleanup reload: %v %s", err, out)
		}
		t.Logf("Preserved disposable volume (never removed): systemd-%s", prefix)
	})
	reconcile := func(q *Quadlet) {
		t.Helper()
		if err := q.reconcile(ctx, conn); err != nil {
			t.Fatal(err)
		}
		q.initialRun = false
	}
	ids := func() []string {
		t.Helper()
		out := []string{}
		for _, name := range names {
			out = append(out, command("podman", "inspect", "--format={{.Id}}", name))
		}
		return out
	}
	verify := func(label string, before []string, changedA bool) []string {
		t.Helper()
		after := ids()
		for i := range after {
			wantChange := changedA && i < 2
			if (before[i] != after[i]) != wantChange {
				t.Fatalf("%s container %s changed=%t want=%t", label, names[i], before[i] != after[i], wantChange)
			}
		}
		t.Logf("%s IDs=%v", label, after)
		return after
	}
	receipt := func(q *Quadlet, want plumbing.Hash) {
		t.Helper()
		ns, _ := q.hostIdentity()
		data, err := os.ReadFile(filepath.Join(q.HostConfigHome, "."+ns, "receipt"))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) < 5 || lines[0] != want.String() || lines[1] != q.GetName() {
			t.Fatalf("receipt mismatch %s: %q", q.Name, data)
		}
		current, err := getCurrent(q.target, q.GetKind(), q.GetName())
		if err != nil || current != want {
			t.Fatalf("Git tag mismatch: %s %v", current, err)
		}
	}
	reconcile(a)
	reconcile(b)
	receipt(a, first)
	receipt(b, first)
	before := ids()
	t.Logf("initial IDs=%v", before)
	write("README", "unrelated docs")
	docs := commit("docs only")
	reconcile(a)
	reconcile(b)
	before = verify("docs-only", before, false)
	receipt(a, docs)
	receipt(b, docs)
	a.initialRun = true
	b.initialRun = true
	reconcile(a)
	reconcile(b)
	before = verify("controller restart", before, false)
	for _, change := range []string{"nested-config", "synthetic-ciphertext", "addition", "mode", "deletion", "volume-definition"} {
		switch change {
		case "nested-config":
			write("bundle/nested/config", "public config two")
		case "synthetic-ciphertext":
			write("bundle/credentials/public-marker.cred", "rotated synthetic ciphertext")
		case "addition":
			write("bundle/nested/added", "public addition")
		case "mode":
			if err := os.Chmod("repo/bundle/nested/added", 0755); err != nil {
				t.Fatal(err)
			}
		case "deletion":
			if err := os.Remove("repo/bundle/nested/added"); err != nil {
				t.Fatal(err)
			}
		case "volume-definition":
			write("bundle/"+prefix+".volume", "[Volume]\nLabel=proof=two\n")
		}
		latest := commit(change)
		reconcile(a)
		reconcile(b)
		before = verify(change, before, true)
		receipt(a, latest)
		receipt(b, latest)
	}
	// Updating a volume definition is NOT a migration of the existing volume.
	if command("podman", "volume", "inspect", "--format={{index .Labels \"proof\"}}", "systemd-"+prefix) != "one" {
		t.Fatal("existing volume was unexpectedly replaced or relabeled")
	}
	// The same embedded script runs in another verified helper reference (image ID).
	a.HelperImage = command("podman", "image", "inspect", "--format={{.Id}}", image)
	reconcile(a)
	before = verify("helper-only config", before, false)
	latest, err := getLatest(a.target)
	if err != nil {
		t.Fatal(err)
	}
	receipt(a, latest)
	// Missing receipt must recover even with unchanged Git on controller startup.
	ns, _ := a.hostIdentity()
	control := filepath.Join(a.HostConfigHome, "."+ns)
	if err := os.Remove(filepath.Join(control, "receipt")); err != nil {
		t.Fatal(err)
	}
	a.initialRun = true
	reconcile(a)
	before = verify("missing receipt repair", before, true)
	receipt(a, latest)
	// Invalid candidates must not stop the old workload or advance bookkeeping.
	write("bundle/"+names[0]+".container", "[Container]\nInvalidOption=true\n")
	commit("invalid candidate")
	if err := a.reconcile(ctx, conn); err == nil {
		t.Fatal("invalid candidate accepted")
	}
	before = verify("invalid candidate rejected", before, false)
	receipt(a, latest)
	// A failed activation leaves pending state; retry must repair, not tree-skip.
	allow := filepath.Join(t.TempDir(), "allow")
	write("bundle/"+names[0]+".container", source(names[0])+"ExecStartPre=/usr/bin/test -f "+allow+"\n")
	failed := commit("start failure")
	if err := a.reconcile(ctx, conn); err == nil {
		t.Fatal("failed activation accepted")
	}
	receipt(a, latest)
	if _, err := os.Stat(filepath.Join(control, "pending")); err != nil {
		t.Fatal("missing pending recovery journal", err)
	}
	if err := os.WriteFile(allow, nil, 0644); err != nil {
		t.Fatal(err)
	}
	a.dirty = false
	a.initialRun = true // simulate losing in-memory state across a controller restart
	reconcile(a)
	reconcile(b)
	before = verify("failed apply retry", before, true)
	receipt(a, failed)
	receipt(b, failed)
	// A stale docs-only plan cannot overwrite the current receipt, even if trees
	// compare equal at its own two historical revisions.
	if err := a.Apply(ctx, conn, first, docs, nil); err == nil {
		t.Fatal("stale CAS plan accepted")
	}
	receipt(a, failed)
	before = verify("stale CAS rejected", before, false)
	// A lifecycle change must not be hidden by matching tree identities.
	a.Start = false // Restart remains true, so activation is still requested.
	reconcile(a)
	before = verify("lifecycle settings change", before, true)
	receipt(a, failed)
	// Source deletion retires only the deleted service, retaining B and volume data.
	if err := os.Remove("repo/bundle/" + names[1] + ".container"); err != nil {
		t.Fatal(err)
	}
	deleted := commit("delete A peer service")
	reconcile(a)
	reconcile(b)
	receipt(a, deleted)
	receipt(b, deleted)
	if err := exec.Command("podman", "container", "exists", names[1]).Run(); err == nil {
		t.Fatal("deleted service container still exists")
	}
	if command("podman", "inspect", "--format={{.Id}}", names[0]) == before[0] {
		t.Fatal("remaining A service not restarted on deletion")
	}
	if command("podman", "inspect", "--format={{.Id}}", names[2]) != before[2] {
		t.Fatal("B restarted on A service deletion")
	}
	t.Log("service deletion verified: A peer absent, remaining A restarted, B unchanged")
}
