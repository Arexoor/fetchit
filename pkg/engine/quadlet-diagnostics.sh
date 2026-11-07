# Outer transport wrapper: start capture BEFORE inventory/host script execution.
# Arguments: writable host parent, operation, group identity, revision, command...
set -eu
umask 077
host=${FETCHIT_QUADLET_HOST_ROOT:-/host}
dir="$host$1/fetchit"
operation=$2 group=$3 revision=$4
shift 4
ready=false
tmp= output=
tmp_created=false output_created=false
finish() {
 status=$?
 trap - EXIT
 [ "$tmp_created" != true ] || rm -f -- "$tmp"
 [ "$output_created" != true ] || rm -f -- "$output"
 if [ "$ready" != true ]; then
  printf '%s\n' FETCHIT_DIAGNOSTICS_UNAVAILABLE >&2
 fi
 exit "$status"
}
trap finish EXIT
# Never chmod a pre-existing config directory, or follow a log symlink. Open a
# NEW inode exclusively, then rename it over the old log (no target following).
[ ! -L "$dir" ] || exit 73
if [ ! -d "$dir" ]; then mkdir -m 0700 -- "$dir"; fi
exec 7<"$dir"
flock -x 7
[ ! -L "$dir/quadlet.log" ] || exit 73
[ ! -e "$dir/quadlet.log" ] || [ -f "$dir/quadlet.log" ] || exit 73
# The directory lock serializes all helpers sharing this config home. The
# previous tail is bounded; the current operation is kept complete, even large.
tmp="$dir/.quadlet-log.$$"
set -C
exec 6>"$tmp"
tmp_created=true
set +C
if [ -f "$dir/quadlet.log" ]; then tail -c 1048576 -- "$dir/quadlet.log" >&6; fi
mv -fT -- "$tmp" "$dir/quadlet.log"
tmp_created=false
ready=true
export FETCHIT_QUADLET_DIAGNOSTICS=true
printf '\n[%s] operation=%s group=%s revision=%s begin\n' "$(date -u +%FT%TZ)" "$operation" "$group" "$revision" >&6
status=0
if [ "$operation" = inspect ]; then
 output="$dir/.quadlet-inspect.$$"
 set -C
 exec 5>"$output"
 output_created=true
 set +C
 "$@" >&5 2>&6 || status=$?
 cat -- "$output" >&6
 # Only receipt records cross the controller protocol, never private stderr.
 while IFS= read -r line; do
  case "$line" in 'FETCHIT_RECEIPT '*) printf '%s\n' "$line";; esac
 done <"$output"
else
 "$@" >&6 2>&6 || status=$?
fi
printf '[%s] operation=%s group=%s revision=%s exit=%s\n' "$(date -u +%FT%TZ)" "$operation" "$group" "$revision" "$status" >&6
exit "$status"
