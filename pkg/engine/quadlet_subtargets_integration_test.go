//go:build quadlet_subtargets_integration

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
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// Opt-in synthetic workloads only. No ports, no credentials, no volume deletion.
func TestQuadletSubtargetsIntegration(t *testing.T) {
	image := os.Getenv("QUADLET_HELPER_IMAGE")
	if image == "" {
		t.Skip("requires QUADLET_HELPER_IMAGE")
	}
	if os.Getuid() != 1000 {
		t.Fatal("requires isolated approved core rootless user")
	}
	r, q := quadletTestRepo(t)
	prefix := fmt.Sprintf("fetchit-subtargets-proof-%d-%d", os.Getpid(), time.Now().Unix())
	q.Name = prefix
	q.ScanDepth = 1
	q.Root = false
	q.Start = true
	q.Restart = true
	q.HostHome = os.Getenv("HOME")
	q.HostConfigHome = filepath.Join(q.HostHome, ".config")
	q.HostRuntimeDir = os.Getenv("XDG_RUNTIME_DIR")
	q.HelperImage = image
	ctx := context.Background()
	conn, err := bindings.NewConnection(ctx, "unix://"+q.HostRuntimeDir+"/podman/podman.sock")
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
	abs, _ := filepath.Abs("repo")
	if _, err := r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{abs}}); err != nil {
		t.Fatal(err)
	}
	a1, a2, b := prefix+"-a1", prefix+"-a2", prefix+"-b"
	source := func(name string) string {
		return "[Container]\nImage=docker.io/library/alpine:3.23\nPull=never\nContainerName=" + name + "\nNetwork=none\nReadOnly=true\nExec=sleep infinity\n[Service]\nTimeoutStartSec=30\n"
	}
	files := map[string]string{"README": "initial empty parent"}
	poll := func(wantFailure bool) {
		t.Helper()
		h := quadletCommit(t, r, files)
		if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), h)); err != nil {
			t.Fatal(err)
		}
		err := q.reconcile(ctx, conn)
		if (err != nil) != wantFailure {
			t.Fatalf("poll failure=%t: %v", wantFailure, err)
		}
		if !wantFailure {
			current, err := getCurrent(q.target, q.GetKind(), q.GetName())
			if err != nil || current != h {
				t.Fatalf("parent tag: %s %v", current, err)
			}
		}
	}
	t.Cleanup(func() {
		plan, err := q.subtargetRemovalPlan()
		if err == nil {
			err = q.deploy(ctx, conn, plan)
		}
		if err != nil {
			t.Errorf("synthetic cleanup: %v", err)
		}
		t.Logf("Preserved synthetic data volume: %s-data", prefix)
	})
	poll(false)
	files["a/"+a1+".container"] = source(a1)
	files["a/"+a2+".container"] = source(a2)
	files["b/"+b+".container"] = source(b)
	files["a/data.volume"] = "[Volume]\nVolumeName=" + prefix + "-data\n"
	// Service names must remain globally unique even with independent ownership.
	files["a/"+prefix+"-data.volume"] = files["a/data.volume"]
	delete(files, "a/data.volume")
	poll(false)
	ids := func() []string {
		return []string{command("podman", "inspect", "--format={{.Id}}", a1), command("podman", "inspect", "--format={{.Id}}", a2), command("podman", "inspect", "--format={{.Id}}", b)}
	}
	before := ids()
	t.Logf("initial IDs=%v", before)
	mount := command("podman", "volume", "inspect", "--format={{.Mountpoint}}", prefix+"-data")
	marker := filepath.Join(mount, "fetchit-public-marker")
	if err := os.WriteFile(marker, []byte("retain synthetic data"), 0600); err != nil {
		t.Fatal(err)
	}
	verify := func(label string, changeA bool) {
		t.Helper()
		after := ids()
		for i := range after {
			if (after[i] != before[i]) != (changeA && i < 2) {
				t.Fatalf("%s group isolation: before=%v after=%v", label, before, after)
			}
		}
		before = after
		t.Logf("%s IDs=%v", label, after)
	}
	files["README"] = "outside groups"
	poll(false)
	verify("outside README", false)
	for _, name := range []string{"a/nested/config", "a/credentials/public-marker.cred"} {
		files[name] = "synthetic change"
		poll(false)
		verify(name, true)
	}
	fresh := *q
	q = &fresh
	if err := q.reconcile(ctx, conn); err != nil {
		t.Fatal(err)
	}
	verify("controller restart", false)
	// A valid removal plus a host-invalid new candidate must not stop old groups.
	original := files["a/"+a1+".container"]
	delete(files, "a/"+a1+".container")
	delete(files, "a/"+a2+".container")
	files["bad/"+prefix+"-bad.container"] = "[Container]\nImage=docker.io/library/alpine:3.23\nNotARealQuadletKey=yes\n"
	poll(true)
	verify("invalid generator candidate", false)
	delete(files, "bad/"+prefix+"-bad.container")
	files["a/"+a1+".container"] = original
	files["a/"+a2+".container"] = source(a2)
	files["README"] = "restore"
	poll(false)
	verify("valid retry", false)
	files["renamed/"+b+".container"] = files["b/"+b+".container"]
	delete(files, "b/"+b+".container")
	poll(false)
	after := ids()
	if after[0] != before[0] || after[1] != before[1] || after[2] == before[2] {
		t.Fatalf("rename scope: %v %v", before, after)
	}
	delete(files, "a/"+a1+".container")
	delete(files, "a/"+a2+".container")
	poll(false)
	for _, name := range []string{a1, a2} {
		out, err := exec.Command("podman", "container", "exists", name).CombinedOutput()
		if err == nil {
			t.Fatalf("retired container still exists: %s %s", name, out)
		}
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "retain synthetic data" {
		t.Fatalf("data marker lost: %q %v", got, err)
	}
	if id := command("podman", "inspect", "--format={{.Id}}", b); id != after[2] {
		t.Fatal("removing A restarted B")
	}
	files = map[string]string{"README": "all groups removed"}
	poll(false)
	out, err := exec.Command("podman", "container", "exists", b).CombinedOutput()
	if err == nil {
		t.Fatalf("deleted group still exists: %s", out)
	}
	t.Log("PASS: dynamic empty/add, isolation, recursive config/credentials, restart, invalid preservation, rename, empty-container cleanup, deletion and data retention")
}
