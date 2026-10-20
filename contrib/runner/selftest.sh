#!/bin/bash
# selftest.sh -- the launcher's mutual exclusion, checked without a hypervisor.
#
# vmsync-parallel.sh drives real replication, so it cannot be run to test it.
# acquire_lock, release_lock and their helpers are extracted from it verbatim
# and driven as separate processes against a temporary lock path, which is
# enough to answer the only question that matters here: can two instances ever
# hold this lock at once?
#
# WHY IT EXISTS. The lock used to be a directory made with mkdir, with the
# holder's pid written inside it, and the decision to break it came from reading
# that pid back -- so "no record" was indistinguishable from "holder gone", and
# any ps failure read as "stale". Either way the script removed a live holder's
# lock and ran alongside it, on a hypervisor whose port accounting assumes a
# single instance. It is an flock now, and these assertions are what keep it one.
#
# Usage: ./selftest.sh [path/to/vmsync-parallel.sh]
# Exits non-zero if anything fails.
#
# Needs flock and setsid for the locking cases, so it is a Linux check: on a
# host without them it runs the source-level rules, says the rest were skipped,
# and says a clean run there proves less.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="${1:-$HERE/vmsync-parallel.sh}"
[ -r "$SCRIPT" ] || { echo "cannot read $SCRIPT" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
PASS=0
FAIL=0
SKIPPED=0

# The behavioural half needs the tools the lock itself needs. On a host without
# them -- a Windows workstation under Git Bash, for instance -- those cases
# cannot run, and reporting them as failures would be noise that hides a real
# one. The source-level rules at the end need neither and always run.
CAN_LOCK=yes
for tool in flock setsid; do
	command -v "$tool" > /dev/null 2>&1 || CAN_LOCK=no
done

check() {
	local what="$1" want="$2" got="$3"
	if [ "$got" = "$want" ]; then
		PASS=$((PASS + 1))
		printf '  ok    %s\n' "$what"
	else
		FAIL=$((FAIL + 1))
		printf '  FAIL  %s\n        want: %s\n        got:  %s\n' "$what" "$want" "$got"
	fi
}

# --- the real functions, extracted by name --------------------------------
extract() { sed -n "/^$1() {/,/^}/p" "$SCRIPT"; }

LIB="$WORK/lib.sh"
{
	echo 'set -u'
	echo "LOCK_FILE=\"$WORK/lock\""
	echo "PID_FILE=\"$WORK/pid\""
	echo "LEGACY_LOCK_DIR=\"$WORK/legacy\""
	# The directory-based lock this replaced. Defined so this harness can be
	# pointed at a pre-fix vmsync-parallel.sh and show what it fails.
	echo "LOCK_DIR=\"$WORK/lockdir\""
	echo "LOG_FILE=\"$WORK/log\""
	echo "RUN_DIR=\"$WORK\""
	echo 'PROGRAM=selftest'
	echo 'TSTAMP=0'
	echo 'DEBUG=false'
	echo 'SCRIPT_PID=$$'
	extract log
	extract log_quit
	extract holder_note
	extract acquire_lock
	extract release_lock
} > "$LIB"
for fn in log log_quit acquire_lock release_lock; do
	grep -q "^$fn() {" "$LIB" || { echo "EXTRACT FAILED: $fn"; exit 2; }
done
# Optional: holder_note arrived with the fix, so a pre-fix script has none.
grep -q '^holder_note() {' "$LIB" || echo 'holder_note() { :; }' >> "$LIB"

# Each instance is its own PROCESS, because an flock belongs to the open file
# description: a subshell that inherited an already-locked descriptor would
# share the lock rather than contend for it, and every assertion below would
# pass for the wrong reason.
instance() {
	local action="$1"
	bash -c ". '$LIB'; $action" 9>&-
}

reset() { rm -rf "$WORK/lock" "$WORK/pid" "$WORK/legacy" "$WORK/log" "$WORK/lockdir" "$WORK/held" "$WORK/held2" "$WORK/held3"; }

if [ "$CAN_LOCK" = no ]; then
	echo "=== two instances cannot both hold it ==="
	echo "  SKIP  this host has no flock and/or no setsid, so the locking cases cannot run here"
	echo "        (they are what the Linux hosts this script runs on exercise; the rules below still apply)"
	SKIPPED=1
fi

if [ "$CAN_LOCK" = yes ]; then
echo "=== two instances cannot both hold it ==="

reset
# A holder that keeps the lock for the length of the test.
instance 'acquire_lock; echo HELD > '"$WORK/held"'; sleep 30' &
holder=$!
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
	[ -f "$WORK/held" ] && break
	sleep 0.1
done
check "the first instance acquires it" "yes" "$([ -f "$WORK/held" ] && echo yes || echo no)"

out="$(instance 'acquire_lock; echo TOOK_IT' 2>&1)"
rc=$?
check "the second instance backs off rather than running" "0" "$rc"
check "  and does not proceed" "no" "$(echo "$out" | grep -q TOOK_IT && echo yes || echo no)"
check "  and says somebody else is running" "yes" "$(echo "$out" | grep -qi "already running" && echo yes || echo no)"

# THE ORIGINAL DEFECT, both halves. An empty or absent pid file used to be
# indistinguishable from "the holder is gone" -- which is exactly the state
# between taking the lock and recording the pid, and the permanent state if
# that write ever failed.
: > "$WORK/pid"
out="$(instance 'acquire_lock; echo TOOK_IT' 2>&1)"
check "an EMPTY pid file does not authorise a takeover" "no" "$(echo "$out" | grep -q TOOK_IT && echo yes || echo no)"

rm -f "$WORK/pid"
out="$(instance 'acquire_lock; echo TOOK_IT' 2>&1)"
check "a MISSING pid file does not authorise a takeover" "no" "$(echo "$out" | grep -q TOOK_IT && echo yes || echo no)"

# And the lock file itself is still the holder's: nothing removed it.
check "the holder's lock file is untouched" "yes" "$([ -f "$WORK/lock" ] && echo yes || echo no)"

kill "$holder" 2>/dev/null
wait "$holder" 2>/dev/null

echo "=== and the lock is not permanent ==="

# The control: without this, "never acquire" would pass everything above.
reset
out="$(instance 'acquire_lock; echo TOOK_IT' 2>&1)"
check "a lock nobody holds is acquired" "yes" "$(echo "$out" | grep -q TOOK_IT && echo yes || echo no)"

# SIGKILL is the case a pid-file scheme exists to survive and an flock does not
# need to: the kernel drops it, so there is no stale lock and nothing to break.
#
# The whole PROCESS GROUP is killed, and the first version of this test did not
# do that -- it killed the shell only, and the `sleep` it had started kept the
# lock alive. That is not a test artefact to route around but the behaviour
# itself: `exec 9>>` leaves fd 9 open in every child the launcher spawns, so a
# launcher killed while its vmsync children are still running keeps the lock
# until they finish. That is the right answer -- the work the lock protects is
# still in flight -- and it is asserted separately below.
reset
setsid bash -c ". '$LIB'; acquire_lock; echo HELD > '$WORK/held2'; sleep 30" 9>&- &
victim=$!
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
	[ -f "$WORK/held2" ] && break
	sleep 0.1
done
vpgid="$(ps -o pgid= -p "$victim" 2>/dev/null | tr -d ' ')"
[ -n "$vpgid" ] && kill -9 -- "-$vpgid" 2>/dev/null
wait "$victim" 2>/dev/null
sleep 0.2
out="$(instance 'acquire_lock; echo TOOK_IT' 2>&1)"
check "a SIGKILLed holder and its children leave an acquirable lock" "yes" "$(echo "$out" | grep -q TOOK_IT && echo yes || echo no)"

# The other half, asserted because it is a property and not an accident: while
# a child of the dead launcher is still running, the lock is still held.
reset
setsid bash -c ". '$LIB'; acquire_lock; echo HELD > '$WORK/held3'; sleep 30" 9>&- &
victim2=$!
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
	[ -f "$WORK/held3" ] && break
	sleep 0.1
done
vpgid2="$(ps -o pgid= -p "$victim2" 2>/dev/null | tr -d ' ')"
kill -9 "$victim2" 2>/dev/null
wait "$victim2" 2>/dev/null
out="$(instance 'acquire_lock; echo TOOK_IT' 2>&1)"
check "a surviving child of a dead launcher still holds the lock" "no" "$(echo "$out" | grep -q TOOK_IT && echo yes || echo no)"
[ -n "$vpgid2" ] && kill -9 -- "-$vpgid2" 2>/dev/null

# Releasing must not unlink the lock: unlinking a lock file is its own hazard,
# and the next acquirer would get an inode nobody else can see.
reset
instance 'acquire_lock; release_lock' >/dev/null 2>&1
check "release leaves the lock file in place" "yes" "$([ -f "$WORK/lock" ] && echo yes || echo no)"
check "release removes the pid record" "no" "$([ -f "$WORK/pid" ] && echo yes || echo no)"

fi  # CAN_LOCK

echo "=== nothing decides from a record ==="

# Source-level, because the absence of a code path cannot be observed by
# running it. The pid file may be READ for a log line; it must not be fed to
# ps, and nothing may remove a lock.
# The WHOLE lock path, not just acquire_lock. The first version of this check
# looked at acquire_lock alone, and a mutation that fed the pid record to ps
# inside holder_note survived it -- benign there, since holder_note only decides
# whether to name the holder in a log line, but it is the fail-open pattern this
# fix removed and the next edit could promote it. The rule is that nothing in
# the lock path decides anything from a record.
body="$(for __fn in acquire_lock holder_note; do sed -n "/^${__fn}() {/,/^}/p" "$SCRIPT"; done)"
# Word-bounded, and the first version was not: it matched 'ps ' and fired on
# "stops two launchers" inside the function's own error message.
check "nothing in the lock path consults ps" "no" \
	"$(printf '%s\n' "$body" | grep -vE '^[[:space:]]*#' | grep -qE '(^|[^[:alnum:]_])ps([^[:alnum:]_]|$)' && echo yes || echo no)"
check "nothing in the lock path removes a lock" "no" \
	"$(printf '%s\n' "$body" | grep -vE '^\s*#' | grep -qE 'rm -rf|rmdir' && echo yes || echo no)"
check "the lock path asks flock" "yes" \
	"$(printf '%s\n' "$body" | grep -vE '^[[:space:]]*#' | grep -q 'flock' && echo yes || echo no)"
# Non-blocking. A blocking flock would turn "somebody else is running, exit"
# into "wait for them", and a launcher driven from cron would then pile up one
# waiting process per tick instead of standing down.
check "  and does not wait for the holder" "yes" \
	"$(printf '%s\n' "$body" | grep -vE '^[[:space:]]*#' | grep -qE 'flock[[:space:]]+-[a-z]*n' && echo yes || echo no)"
rel="$(sed -n '/^release_lock() {/,/^}/p' "$SCRIPT")"
check "release_lock never removes a lock" "no" \
	"$(printf '%s\n' "$rel" | grep -vE '^\s*#' | grep -qE 'rm -rf|rmdir' && echo yes || echo no)"

echo
if [ "$SKIPPED" -ne 0 ]; then
	printf '%d passed, %d failed -- the locking cases were SKIPPED on this host, so a clean run here does not mean the lock works\n' "$PASS" "$FAIL"
else
	printf '%d passed, %d failed\n' "$PASS" "$FAIL"
fi
[ "$FAIL" -eq 0 ]
