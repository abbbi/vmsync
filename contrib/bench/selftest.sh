#!/bin/bash
# selftest.sh -- the bench's own verdict arithmetic, checked without hardware.
#
# Everything else in this directory needs two hypervisors. This does not: the
# functions that decide what a stage REPORTED are extracted from bench.sh
# verbatim and driven with synthetic CSVs, so the accounting can be checked on
# any machine, including before a bench run rather than after one.
#
# WHY IT EXISTS. A stage's verdict is computed by awk over the results CSV, so
# a check that returns without leaving a row does not read as skipped -- it
# simply moves the denominator. The verify stage reported "14 checks passed"
# where fifteen were intended, with the fifteenth present only as a log line no
# tooling reads, and no amount of diffing two runs could show that a check had
# stopped running. Both halves are asserted below: that an ungraded check is
# counted and named in the verdict, and that the functions which grade their
# own checks never return without recording.
#
# Usage: ./selftest.sh [path/to/bench.sh [path/to/lib/common.sh]]
# Exits non-zero if anything fails, so it can go in front of a bench run.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
BENCH="${1:-$HERE/bench.sh}"
COMMON="${2:-$HERE/lib/common.sh}"
for f in "$BENCH" "$COMMON"; do
	[ -r "$f" ] || { echo "cannot read $f" >&2; exit 2; }
done
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- the real functions, extracted by name ---------------------------------
extract() {
	sed -n "/^$2() {/,/^}/p" "$1"
}
{
	echo 'log()  { :; }'
	echo 'warn() { :; }'
	extract "$COMMON" results_row
	extract "$BENCH" stage_pattern
	extract "$BENCH" stage_verdict
	extract "$BENCH" ungraded_row
	extract "$BENCH" fo_check
} > "$WORK/lib.sh"
for fn in results_row stage_pattern stage_verdict fo_check; do
	grep -q "^$fn() {" "$WORK/lib.sh" || { echo "EXTRACT FAILED: $fn"; exit 2; }
done
# Optional, so this harness can be pointed at a bench.sh from before the fix
# and show which assertions the fix is carrying.
HAVE_UNGRADED=yes
grep -q '^ungraded_row() {' "$WORK/lib.sh" || HAVE_UNGRADED=no
# shellcheck disable=SC1090
. "$WORK/lib.sh"

DRY_RUN=no
FAILOVER_FAILURES=0
PASS=0
FAIL=0

fresh_csv() {
	CSV="$WORK/r.csv"
	printf 'scenario,phase,exit_code,wall_seconds,transferred_bytes,compressed_bytes,disk_bytes,mode,notes\n' > "$CSV"
}

# row SCENARIO PHASE NOTE
row() { results_row "$CSV" "$1" "$2" "" "" "" "" "" "" "$3"; }

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

verdict() { stage_verdict failover | tr '\t' '|'; }

echo "=== the arithmetic ==="

# THE FINDING. Fourteen graded, one that did not apply: before this the
# fifteenth left no row and the verdict read exactly like a stage with only
# fourteen checks in it.
fresh_csv
for i in $(seq 1 14); do row failover "check_$i" PASS; done
row failover "check_15" "N/A this comparator has no plan to prune"
check "14 passed + 1 N/A accounts for all fifteen" \
	"PASS|14 checks passed, 1 not applicable" "$(verdict)"

# The control. Same fourteen, nothing inapplicable: the suffix must not appear,
# or every stage in the suite grows a meaningless clause.
fresh_csv
for i in $(seq 1 14); do row failover "check_$i" PASS; done
check "14 passed and nothing else says just that" \
	"PASS|14 checks passed" "$(verdict)"

# N/A is not a skip. A skip is something to chase; this is not.
fresh_csv
row failover one PASS
row failover two "SKIP the sentinel ownership could not be set"
row failover three "N/A no group was available"
check "skips and N/A are counted apart" \
	"PASS|1 checks passed, 1 skipped, 1 not applicable" "$(verdict)"

# N/A must stay out of the failure denominator: "1 of 2 failed" is about
# checks that were graded.
fresh_csv
row failover one "FAIL it did not"
row failover two PASS
row failover three "N/A not here"
check "N/A is not in the failed-of-total denominator" \
	"FAIL|1 of 2 checks failed, 1 not applicable" "$(verdict)"

# A stage where nothing applied is not a stage where nothing was recorded, and
# the difference is the whole point: one is a configuration, the other is a
# harness that lost its rows.
fresh_csv
row failover one "N/A nope"
row failover two "N/A nope"
check "all-N/A reads as not applicable, not as nothing recorded" \
	"SKIPPED|nothing graded here, 2 checks not applicable" "$(verdict)"

fresh_csv
check "an empty stage still reads nothing recorded" \
	"SKIPPED|nothing recorded" "$(verdict)"

# Standing down outranks everything, including the new clause: a stage that
# gave up has not got as far as having an opinion about applicability.
fresh_csv
row failover one PASS
row failover "stood-down" "SKIP no clean starting point"
row failover two "N/A not here"
check "standing down still outranks the rest" \
	"SKIPPED|stood down after 1 check(s) passed" "$(verdict)"

if [ "$HAVE_UNGRADED" = no ]; then
	echo
	echo "(ungraded_row is absent from this bench.sh, so the row-level assertions cannot run)"
	printf '%d passed, %d failed
' "$PASS" "$FAIL"
	[ "$FAIL" -eq 0 ]
	exit $?
fi

echo "=== the row itself ==="

# DIFFABILITY, which is the reason these rows exist at all. An ungraded row has
# to land in the same phase column as the graded one for the same check, or two
# runs cannot be compared on that check by name.
fresh_csv
label="every replica disk is readable by the qemu account"
sc=failover
fo_check failover "$label" 0
graded_phase="$(awk -F, 'NR==2{print $2}' "$CSV")"
fresh_csv
ungraded_row failover "$label" "N/A" "a reason"
ungraded_phase="$(awk -F, 'NR==2{print $2}' "$CSV")"
check "an ungraded row uses the same phase as fo_check's" "$graded_phase" "$ungraded_phase"
check "and that phase is the label with spaces replaced" \
	"every_replica_disk_is_readable_by_the_qemu_account" "$ungraded_phase"

# The note has to begin with the kind, because that prefix is the only thing
# stage_verdict classifies on.
fresh_csv
ungraded_row failover "some check" "N/A" "because reasons"
note="$(awk -F, 'NR==2{print $9}' "$CSV")"
check "the note begins with the kind" "N/A because reasons" "$note"

# Commas are the other way a row vanishes from the verdict, already fixed
# centrally in results_row. Asserted here because ungraded_row's reasons are
# free prose written at call sites.
fresh_csv
ungraded_row failover "a, label with commas" "N/A" "a reason, with commas"
line="$(sed -n 2p "$CSV")"
check "commas cannot shift the columns" "9" "$(awk -F, 'NR==2{print NF}' "$CSV")"
case "$line" in
*,*,*) : ;;
esac

echo "=== no silent return in a graded function ==="

# The arithmetic above cannot see whether a check function actually RECORDS
# before it gives up -- that is a property of the source, and it is the half of
# 4.15 that put the finding in the report. So it is asserted here, for the
# functions where it was broken.
#
# Deliberately scoped to a list rather than the whole file: most `return` in
# bench.sh is a helper handing a precondition failure back to a caller that
# records on its behalf (probe_reset -> "stood-down", and lib/common.sh's
# target-state guards), and failing those would be noise. Extend the list when
# a function joins the set that grades its own checks.
RECORDERS='ungraded_row|fo_check|results_row|owner_checks_ungraded'
for fn in verify_skip_checks stage_failover_disk_owner; do
	body="$(sed -n "/^$fn() {/,/^}/p" "$BENCH")"
	if [ -z "$body" ]; then
		FAIL=$((FAIL + 1))
		printf '  FAIL  %s is not in bench.sh any more; this assertion has to move with it\n' "$fn"
		continue
	fi
	# The search stops at the enclosing block's opener rather than running a
	# fixed number of lines back, because a window does not respect control
	# flow: with a plain five-line lookback, deleting the recorder from inside
	# an `if` left the check satisfied by an unrelated fo_check further up the
	# function, and two mutations survived. What has to hold is that the
	# recorder is on the path the return takes.
	bad="$(printf '%s\n' "$body" | awk -v re="$RECORDERS" '
		/^[[:space:]]*(#|$)/ { next }
		{ keep[++n] = $0; ln[n] = NR }
		/^[[:space:]]*return([[:space:]]+[0-9]+)?[[:space:]]*$/ {
			found = 0
			for (i = n - 1; i >= 1; i--) {
				if (keep[i] ~ /^[[:space:]]*(if|then|else|elif|fi|do|done|case|esac|while|for|\{|\})/) break
				if (keep[i] ~ /;[[:space:]]*(then|do)[[:space:]]*$/) break
				if (keep[i] ~ re) { found = 1; break }
			}
			if (!found) printf "line %d: %s\n", ln[n], $0
		}')"
	if [ -n "$bad" ]; then
		FAIL=$((FAIL + 1))
		printf '  FAIL  %s gives up without recording anything:\n' "$fn"
		printf '%s\n' "$bad" | sed 's/^/          /'
	else
		PASS=$((PASS + 1))
		printf '  ok    %s records on every path out\n' "$fn"
	fi
done

echo
printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
