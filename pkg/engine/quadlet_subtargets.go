package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/spf13/viper"
)

// Validate only this field before mapstructure's legacy weak type coercions.
func validateQuadletScanDepth(v *viper.Viper) error {
	targets, _ := v.Get("targetConfigs").([]interface{})
	for _, rawTarget := range targets {
		target, _ := rawTarget.(map[string]interface{})
		methods, _ := target["quadlet"].([]interface{})
		for _, rawMethod := range methods {
			method, _ := rawMethod.(map[string]interface{})
			value, present := method["scandepth"]
			if !present {
				continue
			}
			depth, ok := value.(int)
			if !ok || depth < 0 {
				return fmt.Errorf("Quadlet scanDepth must be a nonnegative YAML integer")
			}
		}
	}
	return nil
}

// Parent inventory is a write-ahead ownership index, not workload configuration.
// Pending paths survive partial application even when the next Git revision no
// longer contains them. Child receipts remain authoritative for actual state.
type quadletInventory struct {
	Version int      `json:"version"`
	Epoch   string   `json:"epoch"`
	Applied string   `json:"applied"`
	Paths   []string `json:"paths"`
}

func shellData(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func (q *Quadlet) ownsSubtargetPath(p string) bool {
	if p == "" || p == "." || p == ".." || path.IsAbs(p) || path.Clean(p) != p || strings.HasPrefix(p, "../") || strings.ContainsAny(p, "\x00\r\n") {
		return false
	}
	parent := path.Clean(q.GetTargetPath())
	return parent == "." || (p != parent && strings.HasPrefix(p, parent+"/"))
}

func (q *Quadlet) subtargetAt(targetPath string) *Quadlet {
	child := *q
	sum := sha256.Sum256([]byte(q.Name + "\x00" + q.GetTargetPath() + "\x00" + targetPath))
	prefix := q.Name
	if len(prefix) > 24 {
		prefix = prefix[:24]
	}
	child.Name = fmt.Sprintf("%s-sub-%x", prefix, sum[:16])
	child.TargetPath = targetPath
	child.ScanDepth = 0
	child.subtarget = true
	child.dirty = false
	child.forceReconcile = true
	child.expectedReceipt = ""
	return &child
}

// Every parent transaction uses this same host lock, including inspection.
// Snapshot it BEFORE fetching desired Git; later child receipt reads cannot
// authorize an old poll after any competing parent has begun applying.
func inventoryLock(parent, namespace string) string {
	return "host=${FETCHIT_QUADLET_HOST_ROOT:-/host}\ncontrol=" + shellData(parent+"/."+namespace) + "\n" + `[ ! -L "$host$control" ] || exit 1
mkdir -p "$host$control"
chmod 0700 "$host$control"
exec 8>"$host$control/groups.lock"
flock -x 8
token=absent
if [ -f "$host$control/groups" ]; then
 token=$(sha256sum "$host$control/groups"); token=${token%% *}
fi
`
}

func (q *Quadlet) subtargetRemovalPlan() (quadletPlan, error) {
	parent, runtime, err := q.paths()
	if err != nil {
		return quadletPlan{}, err
	}
	namespace, home := q.hostIdentity()
	return subgroupRemovalPlan(parent, runtime, namespace, home), nil
}

func subgroupRemovalPlan(parent, runtime, namespace, home string) quadletPlan {
	script := inventoryLock(parent, namespace) + "managed_parent=" + shellData(parent) + "\n" + `[ -f "$host$control/groups-owned" ] || exit 0
while IFS= read -r child; do
 case "$child" in fetchit-*-sub-*) ;; *) exit 1;; esac
 case "$child" in *[!a-zA-Z0-9_.-]*|'') exit 1;; esac
 FETCHIT_QUADLET_BUNDLE="${FETCHIT_QUADLET_BUNDLE:-/tmp/quadlet-bundle}/empty" sh -eu "${FETCHIT_QUADLET_BUNDLE:-/tmp/quadlet-bundle}/host.sh" ` + shellData(parent) + " " + shellData(runtime) + ` "$child" '' '' false '' 0000000000000000000000000000000000000000 0000000000000000000000000000000000000000 removal true false
 rm -f "$host$managed_parent/.$child/receipt"
done <"$host$control/groups-owned"
rm -f "$host$control/groups" "$host$control/groups-owned"
`
	return quadletPlan{parent: parent, runtime: runtime, namespace: namespace, home: home, desired: quadletBundle{files: map[string][]byte{"host.sh": []byte(quadletHostScript), "parent.sh": []byte(script), "empty/.fetchit-empty": {}}, modes: map[string]int64{"host.sh": 0600, "parent.sh": 0600, "empty/.fetchit-empty": 0600}}, command: []string{"-ceu", "exec sh -eu \"${FETCHIT_QUADLET_BUNDLE:-/tmp/quadlet-bundle}/parent.sh\""}}
}

func (q *Quadlet) reconcileSubtargets(ctx, conn context.Context) error {
	parent, runtime, err := q.paths()
	if err != nil {
		return err
	}
	namespace, home := q.hostIdentity()
	run := q.runHost
	if run == nil {
		run = q.deploy
	}
	lock := inventoryLock(parent, namespace)
	var receipt string
	inspect := lock + `printf 'FETCHIT_RECEIPT %s' "$token"
if [ "$token" != absent ]; then
 [ "$(wc -c <"$host$control/groups")" -le 524288 ] || exit 1
 printf ' '; base64 -w0 "$host$control/groups"
fi
printf '\n'
`
	if err := run(ctx, conn, quadletPlan{receipt: &receipt, parent: parent, runtime: runtime, namespace: namespace, home: home, command: []string{"-ceu", inspect}}); err != nil {
		return err
	}
	fields := strings.Fields(receipt)
	inv := quadletInventory{Version: 1, Applied: plumbing.ZeroHash.String(), Paths: []string{}}
	if len(fields) < 2 || fields[0] != "FETCHIT_RECEIPT" {
		return fmt.Errorf("invalid Quadlet subgroup inventory")
	}
	token := fields[1]
	if token == "absent" {
		if len(fields) != 2 {
			return fmt.Errorf("invalid absent subgroup inventory")
		}
	} else {
		if len(fields) != 3 || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(token) {
			return fmt.Errorf("invalid subgroup inventory token")
		}
		data, e := base64.StdEncoding.DecodeString(fields[2])
		if e != nil || len(data) > 524288 {
			return fmt.Errorf("invalid subgroup inventory encoding")
		}
		sum := sha256.Sum256(data)
		if fmt.Sprintf("%x", sum) != token {
			return fmt.Errorf("subgroup inventory digest mismatch")
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if e := decoder.Decode(&inv); e != nil || inv.Version != 1 || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(inv.Applied) {
			return fmt.Errorf("invalid subgroup inventory record")
		}
		var extra interface{}
		if decoder.Decode(&extra) != io.EOF {
			return fmt.Errorf("invalid subgroup inventory trailer")
		}
	}
	paths := map[string]bool{}
	for _, p := range inv.Paths {
		if !q.ownsSubtargetPath(p) {
			return fmt.Errorf("invalid owned subgroup path")
		}
		paths[p] = true
	}
	// Verify the old applied tree as well as write-ahead paths; neither arbitrary
	// host directory names nor the controller's in-memory cache confer ownership.
	if inv.Applied != plumbing.ZeroHash.String() {
		old, err := q.discoverSubtargets(inv.Applied)
		if err != nil {
			return err
		}
		for _, p := range old {
			paths[p] = true
		}
	}
	latest, err := getLatest(q.target)
	if err != nil {
		return err
	}
	desired, err := q.discoverSubtargets(latest.String())
	if err != nil {
		return err
	}
	desiredSet := map[string]bool{}
	for _, p := range desired {
		paths[p] = true
		desiredSet[p] = true
	}
	ordered := make([]string, 0, len(paths))
	for p := range paths {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	type entry struct {
		child *Quadlet
		plan  quadletPlan
		index int
	}
	entries := []entry{}
	serviceOwners := map[string]string{}
	// Complete all local validation and host receipt reads before mutating state.
	for i, p := range ordered {
		child := q.subtargetAt(p)
		desiredRevision := latest
		if !desiredSet[p] {
			desiredRevision = plumbing.ZeroHash
		}
		if _, err := child.bundle(desiredRevision); err != nil {
			return err
		}
		baseline, childToken, err := child.hostBaseline(ctx, conn)
		if err != nil {
			return err
		}
		child.expectedReceipt = childToken
		var plan quadletPlan
		child.runHost = func(_ context.Context, _ context.Context, p quadletPlan) error { plan = p; return nil }
		if err := child.Apply(ctx, conn, baseline, desiredRevision, nil); err != nil {
			return err
		}
		for _, service := range plan.desired.services {
			if owner, exists := serviceOwners[service]; exists {
				return fmt.Errorf("duplicate service %s across Quadlet groups %s and %s", service, owner, p)
			}
			serviceOwners[service] = p
		}
		entries = append(entries, entry{child, plan, i})
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	epoch := fmt.Sprintf("%x", nonce)
	pending := quadletInventory{Version: 1, Epoch: epoch, Applied: inv.Applied, Paths: ordered}
	final := quadletInventory{Version: 1, Epoch: epoch, Applied: latest.String(), Paths: desired}
	pendingJSON, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	finalJSON, err := json.Marshal(final)
	if err != nil {
		return err
	}
	if len(pendingJSON) > 524288 || len(finalJSON) > 524288 {
		return fmt.Errorf("subgroup inventory exceeds 512 KiB")
	}
	files := map[string][]byte{"host.sh": []byte(quadletHostScript)}
	modes := map[string]int64{"host.sh": 0600}
	retiring := []string{}
	for _, e := range entries {
		if !desiredSet[e.child.GetTargetPath()] {
			retiring = append(retiring, e.plan.namespace)
		}
	}
	script := "export FETCHIT_QUADLET_RETIRING_NAMESPACES=" + shellData(strings.Join(retiring, "\n")) + "\n" + lock + "[ \"$token\" = " + shellData(token) + " ] || { echo 'Stale Quadlet parent snapshot' >&2; exit 1; }\n"
	call := func(e entry, operation string) string {
		p := e.plan
		restart := ""
		if e.child.Restart {
			restart = strings.Join(p.desired.restartServices, "\n")
		}
		args := []string{p.parent, p.runtime, p.namespace, strings.Join(p.previous.services, "\n"), strings.Join(p.desired.services, "\n"), fmt.Sprint(e.child.Start || e.child.Restart), restart, p.current, p.desiredRevision, p.configID, "false", fmt.Sprint(p.unchangedTree), p.lifecycleID, p.desired.treeID.String(), operation, p.expectedReceipt}
		cmd := "FETCHIT_QUADLET_BUNDLE=\"${FETCHIT_QUADLET_BUNDLE:-/tmp/quadlet-bundle}/groups/" + fmt.Sprint(e.index) + "\" sh -eu \"${FETCHIT_QUADLET_BUNDLE:-/tmp/quadlet-bundle}/host.sh\""
		for _, arg := range args {
			cmd += " " + shellData(arg)
		}
		return cmd + "\n"
	}
	for _, e := range entries {
		// An empty archive directory still needs to exist for removal staging.
		prefix := fmt.Sprintf("groups/%d/", e.index)
		files[prefix+".fetchit-empty"] = []byte{}
		modes[prefix+".fetchit-empty"] = 0600
		for name, data := range e.plan.desired.files {
			files[prefix+name] = data
			modes[prefix+name] = e.plan.desired.modes[name]
		}
		script += call(e, "validate")
	}
	persist := func(data []byte) string {
		return "printf '%s' " + shellData(string(data)) + " >\"$host$control/groups.tmp\"\nmv \"$host$control/groups.tmp\" \"$host$control/groups\"\n"
	}
	owned := []string{}
	for _, e := range entries {
		owned = append(owned, e.plan.namespace)
	}
	script += "printf '%s' " + shellData(strings.Join(owned, "\n")+map[bool]string{true: "\n"}[len(owned) > 0]) + " >\"$host$control/groups-owned.tmp\"\nmv \"$host$control/groups-owned.tmp\" \"$host$control/groups-owned\"\n"
	script += persist(pendingJSON)
	// Remove retired identities before applying replacements (including renames).
	// All candidates already passed generator preflight; child ownership checks
	// still run immediately before each individual transaction.
	for _, remove := range []bool{true, false} {
		for _, e := range entries {
			if (!desiredSet[e.child.GetTargetPath()]) == remove {
				script += call(e, "apply")
			}
		}
	}
	script += persist(finalJSON)
	files["parent.sh"] = []byte(script)
	modes["parent.sh"] = 0600
	if err := ctx.Err(); err != nil {
		return err
	}
	err = run(ctx, conn, quadletPlan{parent: parent, runtime: runtime, namespace: namespace, home: home, desired: quadletBundle{files: files, modes: modes}, command: []string{"-ceu", "exec sh -eu \"${FETCHIT_QUADLET_BUNDLE:-/tmp/quadlet-bundle}/parent.sh\""}})
	if err != nil {
		return err
	}
	// Host inventory commits only after every group succeeds. Git tags are caches:
	// a crash here resumes using that host record and per-child receipt proofs.
	for _, e := range entries {
		if err := updateCurrent(ctx, q.target, latest, q.GetKind(), e.child.GetName()); err != nil {
			return err
		}
	}
	return updateCurrent(ctx, q.target, latest, q.GetKind(), q.GetName())
}

// Depth counts directory edges below targetPath, excluding the parent itself.
// A selected root owns its complete subtree: never select overlapping groups.
func (q *Quadlet) discoverSubtargets(revision string) ([]string, error) {
	if q.ScanDepth < 0 {
		return nil, fmt.Errorf("Quadlet scanDepth must be nonnegative")
	}
	if q.ScanDepth == 0 {
		return []string{q.GetTargetPath()}, nil
	}
	roots := []string{}
	hash := plumbing.NewHash(revision)
	if hash.IsZero() {
		return roots, nil
	}
	tree, err := getSubTreeFromHash(getDirectory(q.target), hash, q.GetTargetPath())
	if errors.Is(err, object.ErrDirectoryNotFound) {
		return roots, nil
	}
	if err != nil {
		return nil, err
	}
	type pending struct {
		tree  *object.Tree
		dir   string
		depth int
	}
	queue := []pending{{tree, q.GetTargetPath(), 0}}
	for len(queue) > 0 {
		item := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if item.depth > 0 {
			selected := false
			for _, entry := range item.tree.Entries {
				ext := path.Ext(entry.Name)
				if entry.Mode != filemode.Dir && (ext == ".container" || ext == ".volume" || ext == ".network") {
					selected = true
					break
				}
			}
			if selected {
				roots = append(roots, item.dir)
				continue
			}
		}
		if item.depth >= q.ScanDepth {
			continue
		}
		for _, entry := range item.tree.Entries {
			if entry.Mode != filemode.Dir {
				continue
			}
			child, err := item.tree.Tree(entry.Name)
			if err != nil {
				return nil, err
			}
			queue = append(queue, pending{child, path.Join(item.dir, entry.Name), item.depth + 1})
		}
	}
	sort.Strings(roots)
	return roots, nil
}
