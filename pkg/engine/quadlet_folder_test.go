package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// A Git subtree includes recursive blobs and modes, unlike a source-unit list.
func TestQuadletHostUnchangedTreeAdvancesReceipt(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("requires flock")
	}
	dir := t.TempDir()
	control := filepath.Join(dir, "etc/containers/.fetchit-test")
	if err := os.MkdirAll(control, 0755); err != nil {
		t.Fatal(err)
	}
	first, next := strings.Repeat("1", 40), strings.Repeat("2", 40)
	receipt := filepath.Join(control, "receipt")
	if err := os.WriteFile(receipt, []byte(first+"\nconfig\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-ceu", quadletHostScript, "quadlet", "/etc/containers", "", "fetchit-test", "app.service", "app.service", "true", "app.service", first, next, "config", "false", "true")
	cmd.Env = append(os.Environ(), "FETCHIT_QUADLET_HOST_ROOT="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unchanged subtree must advance receipt without host lifecycle commands: %v %s", err, out)
	}
	got, err := os.ReadFile(receipt)
	if err != nil || string(got) != next+"\nconfig\n" {
		t.Fatalf("receipt not advanced: %q %v", got, err)
	}
}

func TestQuadletHostHelperOnlyConfigChange(t *testing.T) {
	dir := t.TempDir()
	control := filepath.Join(dir, "etc/containers/.fetchit-test")
	if err := os.MkdirAll(control, 0755); err != nil {
		t.Fatal(err)
	}
	first, next, tree := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("a", 40)
	receipt := filepath.Join(control, "receipt")
	if err := os.WriteFile(receipt, []byte(first+"\nold-helper\nlifecycle\n"+tree+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-ceu", quadletHostScript, "quadlet", "/etc/containers", "", "fetchit-test", "app.service", "app.service", "true", "app.service", strings.Repeat("0", 40), next, "new-helper", "false", "false", "lifecycle", tree)
	cmd.Env = append(os.Environ(), "FETCHIT_QUADLET_HOST_ROOT="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("verified helper-only config change restarted bundle: %v %s", err, out)
	}
	got, err := os.ReadFile(receipt)
	if err != nil || string(got) != next+"\nnew-helper\nlifecycle\n"+tree+"\n" {
		t.Fatalf("helper config not persisted: %q %v", got, err)
	}
}

func TestQuadletTreeChangesReconcileWholeBundle(t *testing.T) {
	for _, change := range []string{"nested", "ciphertext", "add", "delete", "mode", "volume", "initial", "removed-directory"} {
		t.Run(change, func(t *testing.T) {
			r, q := quadletTestRepo(t)
			files := map[string]string{"app.container": "[Container]\nImage=example.com/app\n", "peer.container": "[Container]\nImage=example.com/app\n", "data.volume": "[Volume]\n", "nested/config": "one", "credentials/public-marker.cred": "synthetic ciphertext"}
			first := quadletCommit(t, r, files)
			switch change {
			case "nested":
				files["nested/config"] = "two"
			case "ciphertext":
				files["credentials/public-marker.cred"] = "changed synthetic ciphertext"
			case "add":
				files["nested/add"] = "added"
			case "delete":
				delete(files, "nested/config")
			case "volume":
				files["data.volume"] = "[Volume]\nLabel=revision=two\n"
			case "removed-directory":
				files = map[string]string{}
			case "initial":
				first = plumbing.ZeroHash
			}
			var next plumbing.Hash
			if change == "mode" {
				if err := os.Chmod("repo/bundle/nested/config", 0755); err != nil {
					t.Fatal(err)
				}
				w, _ := r.Worktree()
				if _, err := w.Add("bundle/nested/config"); err != nil {
					t.Fatal(err)
				}
				var err error
				next, err = w.Commit("mode", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com"}})
				if err != nil {
					t.Fatal(err)
				}
			} else if change == "initial" {
				ref, err := r.Head()
				if err != nil {
					t.Fatal(err)
				}
				next = ref.Hash()
			} else {
				next = quadletCommit(t, r, files)
			}
			q.runHost = func(_ context.Context, _ context.Context, p quadletPlan) error {
				if p.unchangedTree {
					t.Fatal("changed recursive tree marked unchanged")
				}
				if change != "removed-directory" && strings.Join(p.desired.restartServices, ",") != "app.service,peer.service" {
					t.Fatalf("not whole bundle: %v", p.desired.restartServices)
				}
				return nil
			}
			if err := q.Apply(context.Background(), context.Background(), first, next, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQuadletReceiptNoopGuards(t *testing.T) {
	for _, scenario := range []string{"missing", "pending", "stale", "lifecycle", "tree", "legacy-helper"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			control := filepath.Join(dir, "etc/containers/.fetchit-test")
			if err := os.MkdirAll(control, 0755); err != nil {
				t.Fatal(err)
			}
			first, next, tree := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("a", 40)
			current, config, lifecycle, unchanged := first, "config", "lifecycle", "true"
			receipt := first + "\nconfig\nlifecycle\n" + tree + "\n"
			switch scenario {
			case "pending":
				if err := os.WriteFile(filepath.Join(control, "pending"), []byte("app.service\n"), 0644); err != nil {
					t.Fatal(err)
				}
			case "stale":
				current = strings.Repeat("3", 40)
			case "lifecycle":
				config = "new-settings"
				lifecycle = "new-lifecycle"
				current = strings.Repeat("0", 40)
				unchanged = "false"
			case "tree":
				tree = strings.Repeat("b", 40)
				unchanged = "false"
			case "legacy-helper":
				receipt = first + "\nold-helper\n"
				current = strings.Repeat("0", 40)
				unchanged = "false"
			}
			if scenario != "missing" {
				if err := os.WriteFile(filepath.Join(control, "receipt"), []byte(receipt), 0644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", "-ceu", quadletHostScript, "quadlet", "/etc/containers", "", "fetchit-test", "app.service", "app.service", "true", "app.service", current, next, config, "false", unchanged, lifecycle, tree)
			cmd.Env = append(os.Environ(), "FETCHIT_QUADLET_HOST_ROOT="+dir)
			out, err := cmd.CombinedOutput()
			// There are deliberately no host binaries here: a full reconcile fails,
			// whereas a mistaken no-op would succeed and advance the receipt.
			if err == nil {
				t.Fatalf("unsafe no-op for %s: %s", scenario, out)
			}
			got, _ := os.ReadFile(filepath.Join(control, "receipt"))
			if scenario != "missing" && string(got) != receipt {
				t.Fatal("failed operation advanced receipt")
			}
			if scenario == "stale" && !strings.Contains(string(out), "Stale Quadlet plan") {
				t.Fatalf("CAS not enforced: %s", out)
			}
		})
	}
}

func TestQuadletUnrelatedCommitReceiptPlan(t *testing.T) {
	r, q := quadletTestRepo(t)
	first := quadletCommit(t, r, map[string]string{"app.container": "[Container]\nImage=example.com/app\n", "nested/config": "one"})
	if err := os.WriteFile("repo/README", []byte("outside bundle"), 0644); err != nil {
		t.Fatal(err)
	}
	w, _ := r.Worktree()
	if err := w.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	next, err := w.Commit("docs only", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	q.runHost = func(_ context.Context, _ context.Context, p quadletPlan) error {
		calls++
		if !p.unchangedTree {
			t.Fatal("unrelated commit requested bundle restart instead of receipt-only CAS")
		}
		if p.current != first.String() || p.desiredRevision != next.String() {
			t.Fatal("receipt revisions lost")
		}
		return nil
	}
	if err := q.Apply(context.Background(), context.Background(), first, next, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("host receipt must advance under ownership lock")
	}
}
