//go:build quadlet_diagnostics_integration

package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containers/podman/v5/pkg/bindings"
)

// Uses the real rootless API/helper transport, but only disposable config paths
// and a deliberately nonexistent systemd unit; never changes a live workload.
func TestQuadletDiagnosticsIntegration(t *testing.T) {
	image := os.Getenv("QUADLET_HELPER_IMAGE")
	if image == "" {
		t.Skip("requires QUADLET_HELPER_IMAGE")
	}
	if os.Getuid() != 1000 {
		t.Fatal("requires approved core rootless user")
	}
	parent, err := os.MkdirTemp(os.Getenv("HOME"), "fetchit-diagnostics-proof-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(parent)
	ctx := context.Background()
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	conn, err := bindings.NewConnection(ctx, "unix://"+runtime+"/podman/podman.sock")
	if err != nil {
		t.Fatal(err)
	}
	before, err := exec.Command("podman", "ps", "-aq").Output()
	if err != nil {
		t.Fatal(err)
	}
	q := &Quadlet{HelperImage: image}
	p := quadletPlan{parent: parent, runtime: runtime, home: os.Getenv("HOME"), namespace: "fetchit-diagnostics-proof", desiredRevision: "synthetic-revision"}
	p.command = []string{"-ceu", "echo PRIVATE_EARLY_PROOF >&2; exit 23"}
	err = q.deploy(ctx, conn, p)
	if err == nil || !strings.Contains(err.Error(), "status 23") || !strings.Contains(err.Error(), parent+"/fetchit/quadlet.log") || strings.Contains(err.Error(), "PRIVATE_") {
		t.Fatalf("failure not private/actionable: %v", err)
	}
	logpath := filepath.Join(parent, "fetchit", "quadlet.log")
	data, err := os.ReadFile(logpath)
	if err != nil || !strings.Contains(string(data), "PRIVATE_EARLY_PROOF") {
		t.Fatalf("lost helper diagnostic: %v", err)
	}
	info, err := os.Stat(logpath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("diagnostics not private")
	}
	p.command = []string{"-ceu", "chroot /host /usr/bin/env XDG_RUNTIME_DIR=" + runtime + " systemctl --user start fetchit-diagnostics-nonexistent-proof.service"}
	if err = q.deploy(ctx, conn, p); err == nil {
		t.Fatal("expected nonexistent unit failure")
	}
	data, err = os.ReadFile(logpath)
	if err != nil || !strings.Contains(string(data), "not found") {
		t.Fatalf("lost real systemctl stderr: %v", err)
	}
	var receipt string
	p.receipt = &receipt
	p.command = []string{"-ceu", "printf 'FETCHIT_RECEIPT absent\\n'"}
	if err = q.deploy(ctx, conn, p); err != nil || receipt != "FETCHIT_RECEIPT absent" {
		t.Fatalf("receipt regression: %q %v", receipt, err)
	}
	data, err = os.ReadFile(logpath)
	if err != nil || !strings.Contains(string(data), "PRIVATE_EARLY_PROOF") {
		t.Fatal("inspection erased previous failure")
	}
	after, err := exec.Command("podman", "ps", "-aq").Output()
	if err != nil || string(before) != string(after) {
		t.Fatal("helper cleanup failed or container set changed")
	}
	t.Log("real rootless API: early exit preserved, systemctl stderr durable, mode 0600, receipt unchanged, helpers removed")
}
