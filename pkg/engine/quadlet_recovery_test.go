package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Complements the executable offline shell tests: this checks the controller
// selects B (not stale Git tag A) as the cleanup baseline after a restart.
func TestQuadletRecoverReceiptBeforePlanning(t *testing.T) {
	for _, receiptOnly := range []bool{true, false} {
		for _, missingTag := range []bool{true, false} {
			t.Run(fmt.Sprintf("receiptOnly=%t/missingTag=%t", receiptOnly, missingTag), func(t *testing.T) {
				r, q := quadletTestRepo(t)
				ctx := context.Background()
				files := map[string]string{"app.container": "[Container]\nImage=example.com/app\n"}
				a := quadletCommit(t, r, files)
				var b plumbing.Hash
				if receiptOnly {
					if err := os.WriteFile("repo/README", []byte("docs B"), 0600); err != nil {
						t.Fatal(err)
					}
					w, _ := r.Worktree()
					if _, err := w.Add("README"); err != nil {
						t.Fatal(err)
					}
					var err error
					b, err = w.Commit("B docs", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com"}})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					files["removed-at-c.container"] = "[Container]\nImage=example.com/removed\n"
					b = quadletCommit(t, r, files)
				}
				delete(files, "removed-at-c.container")
				files["app.container"] = "[Container]\nImage=example.com/c\n"
				c := quadletCommit(t, r, files)
				if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), c)); err != nil {
					t.Fatal(err)
				}
				abs, _ := filepath.Abs("repo")
				if _, err := r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{abs}}); err != nil {
					t.Fatal(err)
				}
				if err := updateCurrent(ctx, q.target, a, q.GetKind(), q.GetName()); err != nil {
					t.Fatal(err)
				}
				if missingTag {
					// Simulate the updateCurrent crash window after DeleteTag,
					// before CreateTag, with the host already at B.
					if err := r.DeleteTag("current-" + q.GetKind() + "-" + q.GetName()); err != nil {
						t.Fatal(err)
					}
				}
				bundleB, err := q.bundle(b)
				if err != nil {
					t.Fatal(err)
				}
				token := strings.Repeat("a", 64)
				applies := 0
				q.runHost = func(_ context.Context, _ context.Context, p quadletPlan) error {
					if p.receipt != nil {
						*p.receipt = fmt.Sprintf("FETCHIT_RECEIPT %s %s %s %s %s", token, b, q.GetName(), q.settingsID(""), bundleB.treeID)
						return nil
					}
					applies++
					if p.current != b.String() || p.desiredRevision != c.String() || p.expectedReceipt != token {
						t.Fatal("plan not bound to authoritative B receipt")
					}
					if strings.Join(p.previous.services, ",") != strings.Join(bundleB.services, ",") {
						t.Fatal("cleanup baseline lost host B service names")
					}
					return nil
				}
				if err := q.reconcile(ctx, ctx); err != nil {
					t.Fatal(err)
				}
				got, err := getCurrent(q.target, q.GetKind(), q.GetName())
				if err != nil || got != c || applies != 1 {
					t.Fatalf("did not converge: %s %v applies=%d", got, err, applies)
				}
			})
		}
	}
}

func TestQuadletRejectInvalidRecoveryReceipt(t *testing.T) {
	r, q := quadletTestRepo(t)
	b := quadletCommit(t, r, map[string]string{"app.container": "[Container]\nImage=example.com/app\n"})
	bundle, err := q.bundle(b)
	if err != nil {
		t.Fatal(err)
	}
	valid := fmt.Sprintf("FETCHIT_RECEIPT %s %s %s %s %s", strings.Repeat("a", 64), b, q.GetName(), q.settingsID(""), bundle.treeID)
	for _, receipt := range []string{"", "FETCHIT_RECEIPT absent extra", strings.Replace(valid, q.GetName(), "other-owner", 1), strings.Replace(valid, bundle.treeID.String(), strings.Repeat("f", 40), 1), strings.Replace(valid, b.String(), strings.Repeat("f", 40), 1)} {
		q.runHost = func(_ context.Context, _ context.Context, p quadletPlan) error { *p.receipt = receipt; return nil }
		if _, _, err := q.hostBaseline(context.Background(), context.Background()); err == nil {
			t.Fatal("accepted invalid recovery receipt")
		}
	}
}
