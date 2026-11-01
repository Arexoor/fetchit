# Executed by Fetchit inside a host-administration helper. Arguments are data, never shell code.
# Arguments: parent, runtime (empty for system manager), namespace, old service
# list, desired service list, start flag, restart service list, expected current
# revision, desired revision, configuration ID, optional removal, unchanged-tree,
# lifecycle ID, tree ID, operation (apply/inspect) and expected receipt digest.
# Service lists use newlines. New fields are optional for legacy callers.
[ "$#" -ge 10 ] && [ "$#" -le 16 ] || { echo "Expected ten to sixteen Quadlet helper arguments" >&2; exit 1; }
operation=${15:-apply} expected_receipt=${16:-}
case "$operation" in apply|inspect) ;; *) echo 'Invalid Quadlet operation' >&2; exit 1;; esac
parent=$1 runtime=$2 namespace=$3 old_services=$4 new_services=$5 start=$6 restart_units=$7 current=$8 desired_revision=$9 config_id=${10}
removal=${11:-false}
unchanged_tree=${12:-false}
lifecycle_id=${13:-} tree_id=${14:-}
case "$unchanged_tree" in true|false) ;; *) echo "Invalid unchanged-tree mode" >&2; exit 1;; esac
case "$removal" in true|false) ;; *) echo "Invalid removal mode" >&2; exit 1;; esac
host=${FETCHIT_QUADLET_HOST_ROOT:-/host}
control="$parent/.${namespace}"
live="$parent/systemd/$namespace"
if [ -n "$runtime" ]; then live="$parent/containers/systemd/$namespace"; fi
stage="$control/desired"
fail() { echo "$*" >&2; exit 1; }
hostcmd() { chroot "$host" /usr/bin/env PATH=/usr/sbin:/usr/bin:/sbin:/bin "$@"; }
write_receipt() {
 {
  printf '%s\n%s\n' "$desired_revision" "$config_id"
  [ -z "$lifecycle_id" ] || printf '%s\n%s\n' "$lifecycle_id" "$tree_id"
 } >"$host$control/receipt.tmp"
 mv "$host$control/receipt.tmp" "$host$control/receipt"
}
systemctl_cmd() {
 if [ -n "$runtime" ]; then hostcmd XDG_RUNTIME_DIR="$runtime" systemctl --user "$@";
 else hostcmd systemctl "$@"; fi
}
# Reject symlinked managed directories before accessing persistent control state.
for dir in "$control" "$parent/systemd" "$parent/containers" "$parent/containers/systemd" "$live"; do
 [ ! -L "$host$dir" ] || fail "Managed directory is a symlink: $dir"
done
mkdir -p "$host$control"
chmod 0700 "$host$control"
# Serialize simultaneous runs and preserve incomplete operations for retry.
exec 9>"$host$control/lock"
flock -x 9
# Inspection and application are separate lock acquisitions. Bind the later
# plan to this exact receipt (including config), never retry a stale snapshot.
receipt_token=absent
if [ -f "$host$control/receipt" ]; then
 receipt_token=$(sha256sum "$host$control/receipt")
 receipt_token=${receipt_token%% *}
fi
if [ "$operation" = inspect ]; then
 printf 'FETCHIT_RECEIPT %s' "$receipt_token"
 if [ "$receipt_token" != absent ]; then
  # Only bounded identity fields, never generator output, cross this protocol.
  [ "$(wc -c <"$host$control/receipt")" -le 512 ] || fail 'Invalid Quadlet receipt'
  while IFS= read -r field; do
   case "$field" in ''|*[!a-zA-Z0-9_.-]*) fail 'Invalid Quadlet receipt';; esac
   printf ' %s' "$field"
  done <"$host$control/receipt"
 fi
 printf '\n'
 exit 0
fi
if [ -n "$expected_receipt" ] && [ "$expected_receipt" != "$receipt_token" ]; then
 fail 'Stale Quadlet snapshot; reconciliation must be replanned'
fi
# Compare-and-set the applied receipt under the lock. A stale process must not
# replace a newer commit. A new settings identity may initialize its own Git tag.
if [ "$removal" != true ] && [ -f "$host$control/receipt" ]; then
 installed_revision=$(sed -n '1p' "$host$control/receipt")
 installed_config=$(sed -n '2p' "$host$control/receipt")
 installed_lifecycle=$(sed -n '3p' "$host$control/receipt")
 installed_tree=$(sed -n '4p' "$host$control/receipt")
 if [ "$installed_revision" != "$current" ] && [ "$installed_revision" != "$desired_revision" ]; then
  if [ "$current" != 0000000000000000000000000000000000000000 ] || [ "$installed_config" = "$config_id" ]; then
   fail "Stale Quadlet plan: host revision $installed_revision differs from expected $current"
  fi
 fi
 if [ "$installed_revision" = "$current" ] && [ "$installed_config" = "$config_id" ] && [ "$unchanged_tree" = true ] && [ ! -f "$host$control/pending" ]; then
  write_receipt
  echo 'Quadlet tree unchanged; receipt advanced without lifecycle changes'
  exit 0
 fi
 # Only a recorded matching tree AND lifecycle identity can adopt a new
 # helper reference without replay. Legacy receipts conservatively reconcile.
 if [ -n "$lifecycle_id" ] && [ "$installed_lifecycle" = "$lifecycle_id" ] && [ "$installed_tree" = "$tree_id" ] && [ ! -f "$host$control/pending" ]; then
  write_receipt
  echo 'Quadlet tree and lifecycle unchanged; configuration receipt advanced'
  exit 0
 fi
 if [ "$installed_revision" = "$desired_revision" ] && [ "$installed_config" = "$config_id" ] && [ ! -f "$host$control/pending" ]; then
  echo 'Quadlet receipt is current; no changes required'
  exit 0
 fi
fi

version=$(hostcmd podman --version)
# Experimental local test build only: Podman 6 acceptance is not upstream support.
case "$version" in
 *'version 5.'*)
  minor=$(printf '%s' "$version" | sed -n 's/.*version 5\.\([0-9]*\).*/\1/p')
  [ "$minor" -ge 7 ] || fail "Quadlet requires Podman >=5.7"
  ;;
 *'version 6.'*) echo 'Experimental FetchIt test build: allowing host Podman 6' >&2 ;;
 *) fail "Quadlet test build requires host Podman 5.7+ within major 5, or experimental major 6: $version" ;;
esac
[ "$(hostcmd stat -fc %T /sys/fs/cgroup)" = cgroup2fs ] || fail "Quadlet requires cgroup v2"
generator=/usr/lib/systemd/system-generators/podman-system-generator
[ -x "$host$generator" ] || fail "Host Quadlet generator missing: $generator"
systemctl_cmd show --property=Version >/dev/null
rm -rf "$host$stage"
mkdir -p "$host$stage"
cp -a --no-preserve=context,xattr "${FETCHIT_QUADLET_BUNDLE:-/tmp/quadlet-bundle}/." "$host$stage/"
# Keep full diagnostics for private host-side debugging only. Replace old files
# (including legacy permissive logs) before writing; never dump generated units.
rm -f "$host$control/validation.log"
(umask 077; : >"$host$control/validation.log")
if [ -n "$runtime" ]; then
 hostcmd QUADLET_UNIT_DIRS="$stage" "$generator" --user --dryrun >"$host$control/validation.log" 2>&1 || fail 'Quadlet validation failed; inspect private host validation.log'
else
 hostcmd QUADLET_UNIT_DIRS="$stage" "$generator" --dryrun >"$host$control/validation.log" 2>&1 || fail 'Quadlet validation failed; inspect private host validation.log'
fi
# Podman 5 dry-run output delimits units as ---web.service--- (no spaces).
# The generator can skip invalid sources: require output for every expected unit.
printf '%s\n' "$new_services" | while IFS= read -r unit; do
 [ -n "$unit" ] || continue
 grep -F -- "---$unit---" "$host$control/validation.log" >/dev/null || fail 'Quadlet validation incomplete; inspect private host validation.log'
 # Do not replace units managed outside this bundle.
 source=$(systemctl_cmd show "$unit" --property=SourcePath --value)
 fragment=$(systemctl_cmd show "$unit" --property=FragmentPath --value)
 case "$source" in "$live/"*) ;; '') [ -z "$fragment" ] || fail "Service already exists outside bundle: $unit" ;; *) fail "Service belongs to another bundle: $unit ($source)";; esac
done
# pending contains names from a prior incomplete application, even if Git changed.
{
 printf '%s\n' "$old_services"
 [ ! -f "$host$control/installed" ] || cat "$host$control/installed"
 [ ! -f "$host$control/pending" ] || cat "$host$control/pending"
} | sed '/^$/d' | sort -u >"$host$control/previous"
{ cat "$host$control/previous"; printf '%s\n' "$new_services"; } | sed '/^$/d' | sort -u >"$host$control/pending.tmp"
mv "$host$control/pending.tmp" "$host$control/pending"
while IFS= read -r unit; do
 if ! printf '%s\n' "$new_services" | grep -Fx -- "$unit" >/dev/null; then
  load=$(systemctl_cmd show "$unit" --property=LoadState --value)
  if [ "$load" != not-found ]; then
   source=$(systemctl_cmd show "$unit" --property=SourcePath --value)
   case "$source" in "$live/"*) ;; *) fail "Refusing to stop unit outside bundle: $unit ($source)";; esac
   systemctl_cmd stop "$unit"
  fi
 fi
done <"$host$control/previous"
# Replace only the directory owned by this method, retaining relative paths.
mkdir -p "$host$live"
find "$host$live" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
cp -a --no-preserve=context,xattr "$host$stage/." "$host$live/"
systemctl_cmd daemon-reload
if [ "$start" = true ]; then
 # Queue coupled restarts together, before starting remaining (e.g. volume
 # and network) units. Dependencies pull prerequisites into the restart job;
 # starting them first could also pull consumers in before their restart.
 for action in restart start; do
  set --
  while IFS= read -r unit; do
   [ -n "$unit" ] || continue
   if printf '%s\n' "$restart_units" | grep -Fx -- "$unit" >/dev/null; then
    [ "$action" = restart ] || continue
   else
    [ "$action" = start ] || continue
   fi
   set -- "$@" "$unit"
  done <<EOF
$new_services
EOF
  [ "$#" -eq 0 ] || systemctl_cmd "$action" -- "$@"
 done
fi
printf '%s\n' "$new_services" >"$host$control/installed.tmp"
mv "$host$control/installed.tmp" "$host$control/installed"
write_receipt
rm -f "$host$control/pending"
echo 'Quadlet bundle reconciled successfully'
