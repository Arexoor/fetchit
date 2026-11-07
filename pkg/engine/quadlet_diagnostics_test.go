package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the exact command builder used by deploy, not a hand-built wrapper.
func TestQuadletDiagnosticCommand(t *testing.T) {
	parent := t.TempDir()
	p := quadletPlan{parent: parent, namespace: "fetchit-test", desiredRevision: "test-revision"}
	for _, operation := range []string{"apply", "inspect"} {
		command := []string{"-ceu", "echo PRIVATE_TRANSPORT_FAILURE >&2; echo 'FETCHIT_RECEIPT absent'; exit 19"}
		cmd := exec.Command("/bin/sh", quadletDiagnosticCommand(p, operation, command)...)
		cmd.Env = append(os.Environ(), "FETCHIT_QUADLET_HOST_ROOT=/")
		out, err := cmd.CombinedOutput()
		if err == nil || cmd.ProcessState.ExitCode() != 19 {
			t.Fatalf("lost exit status: %v", err)
		}
		if strings.Contains(string(out), "PRIVATE_TRANSPORT_FAILURE") {
			t.Fatal("private diagnostics escaped transport")
		}
		if operation == "inspect" && string(out) != "FETCHIT_RECEIPT absent\n" {
			t.Fatalf("receipt changed: %q", out)
		}
		log, err := os.ReadFile(filepath.Join(parent, "fetchit", "quadlet.log"))
		if err != nil || !strings.Contains(string(log), "PRIVATE_TRANSPORT_FAILURE") || !strings.Contains(string(log), "exit=19") {
			t.Fatalf("missing durable diagnostic: %v", err)
		}
	}
}
