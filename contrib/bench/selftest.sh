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
	# The fixture builder the two planting stages share, and the one-line rule
	# that stops an unanswerable existence question reading as good news. Both
	# are driven below against stubbed virsh calls.
	extract "$BENCH" plant_orphan
	extract "$BENCH" probe_gone
	# The pipeline guard every existence check in bench.sh now goes through.
	# Tested directly rather than through a caller: what it has to get right
	# is an exit status, and the whole defect was that the status was wrong.
	extract "$BENCH" grep_q
} > "$WORK/lib.sh"
for fn in results_row stage_pattern stage_verdict fo_check; do
	grep -q "^$fn() {" "$WORK/lib.sh" || { echo "EXTRACT FAILED: $fn"; exit 2; }
done
# Optional, so this harness can be pointed at a bench.sh from before the fix
# and show which assertions the fix is carrying.
HAVE_UNGRADED=yes
grep -q '^ungraded_row() {' "$WORK/lib.sh" || HAVE_UNGRADED=no
HAVE_PLANT=yes
for fn in plant_orphan probe_gone; do
	grep -q "^$fn() {" "$WORK/lib.sh" || HAVE_PLANT=no
done
HAVE_GREPQ=yes
grep -q '^grep_q() {' "$WORK/lib.sh" || HAVE_GREPQ=no
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

echo "=== the harness says what it is ==="

# A results set used to carry no record of which bench.sh produced it. The
# harness version-checks the engine on both hosts and warns on mere
# uncertainty -- "cannot tell whether the failover stages are testing the build
# you just made" -- and was the one version nothing recorded. That is not a
# cosmetic gap: commit 782a7b8 replaced stage 4e's assertion with its opposite,
# so a deployed copy from before it reports stage 4 FAIL on a healthy host, and
# nothing in the report would say which harness said so.

# Behavioural, and the only assertion here that runs the real script. --version
# exits before any config is read, so this is safe anywhere.
ident="$(bash "$BENCH" --version 2>&1)"
rc=$?
check "--version exits 0" "0" "$rc"
want_sum="$(sha256sum "$BENCH" | cut -d' ' -f1)"
want_lines="$(wc -l < "$BENCH" | tr -d ' ')"
check "--version reports this file's own sha256 and line count" \
	"bench.sh sha256:$want_sum ($want_lines lines)" "$ident"

# Source-level, because the report and the run directory cannot be produced
# without a bench run. Both of these are a single line that is easy to drop.
if grep -A1 'echo "- Run: \$RUN_ID"' "$BENCH" | grep -q 'Harness'; then
	PASS=$((PASS + 1))
	printf '  ok    report.md names the harness beside the run\n'
else
	FAIL=$((FAIL + 1))
	printf '  FAIL  report.md does not name the harness, so a pasted report cannot be attributed to a bench.sh\n'
fi

if grep -q 'bench-ident\.txt' "$BENCH"; then
	PASS=$((PASS + 1))
	printf '  ok    the run directory keeps the harness identity in a file of its own\n'
else
	FAIL=$((FAIL + 1))
	printf '  FAIL  nothing writes bench-ident.txt, so only the report carries the identity and a later script cannot read it\n'
fi

# Ordering: the report is generated long after setup, but an edit that moved
# the assignment below its use would leave every report saying "- Harness:"
# and nothing else, which reads like a harness that declined to answer.
assign="$(grep -n '^BENCH_IDENT=' "$BENCH" | head -1 | cut -d: -f1)"
use="$(grep -n 'echo "- Harness: \$BENCH_IDENT"' "$BENCH" | head -1 | cut -d: -f1)"
if [ -n "$assign" ] && [ -n "$use" ] && [ "$assign" -lt "$use" ]; then
	PASS=$((PASS + 1))
	printf '  ok    the identity is computed before the report prints it\n'
else
	FAIL=$((FAIL + 1))
	printf '  FAIL  BENCH_IDENT is used at line %s but assigned at line %s\n' "${use:-none}" "${assign:-none}"
fi

echo
echo "=== the orphan-export sweep ==="

# WHY THIS IS HERE. kill_orphaned_target_exports aims a kill -9 at the target
# using a number read out of a pidfile, and kill -9 succeeds against any live
# pid, recycled or not. The pidfile it reads is the worst case for that: it is
# left behind by a -test=die-writing-base run that called os.Exit with nothing
# unwinding, so nobody removed it, and a later invocation against the same
# target sweeps it as root. The sweep has to prove the pid is still that export
# before it signals.
#
# The source rules run anywhere, and are written as what the sweep MUST say
# rather than as a list of wrong spellings -- a denylist only forbids the
# mistakes somebody already thought of. The behavioural half needs /proc, and
# where there is none the cases are skipped and SAID to be skipped: a green run
# that quietly omitted them is the same failure this file exists to catch.
SKIPPED=0
sweep_src="$(extract "$BENCH" kill_orphaned_target_exports)"
[ -n "$sweep_src" ] || { echo "EXTRACT FAILED: kill_orphaned_target_exports"; exit 2; }

has() { case "$2" in *"$1"*) echo yes ;; *) echo no ;; esac; }

check "the sweep matches the pid against /proc before it signals" \
	yes "$(has 'grep -qxF' "$sweep_src")"
check "the kill is aimed at a validated variable, not at a substitution" \
	yes "$(has 'kill -9 \"\$q\"' "$sweep_src")"
# Redirections apply left to right: with the input redirect first, a pid that
# has already gone makes the shell announce "No such file or directory" on the
# commonest path there is.
check "stderr is silenced before the /proc read, not after" \
	yes "$(has '2>/dev/null < ' "$sweep_src")"

if [ -r "/proc/$$/cmdline" ]; then
	SWEEPDIR="$WORK/sweep"
	REFUSED="$WORK/refused"
	REC="$WORK/rec"
	mkdir -p "$SWEEPDIR"

	# STUB_KILL, when set, is prepended to the command so a kill is RECORDED
	# instead of sent. Several pidfile contents below would otherwise be
	# catastrophic -- "-1" is every process this user owns, "0" is this
	# shell's own process group -- and the case that proves the guard refuses
	# them must not be the case that takes out the machine when it does not.
	STUB_KILL=""
	ssh_host_cmd() { shift; sh -c "${STUB_KILL}$*"; }
	warn() { printf '%s\n' "$*" >> "$REFUSED"; }
	eval "$sweep_src"

	sweep() {
		: > "$REFUSED"
		rm -f "$REC"
		TARGET_HOST=stub TARGET_DOMAIN=web01 TARGET_RUNTIME_DIR="$SWEEPDIR" \
			kill_orphaned_target_exports > /dev/null 2>&1
	}

	# MARKER as its own argv word, which is how qemu-nbd carries the operand
	# of --pid-file. Output to /dev/null because this runs inside $(...) and a
	# background child holding that pipe open never lets it return.
	spawn() {
		sh -c 'while :; do sleep 0.2; done' "$1" > /dev/null 2>&1 &
		local p=$!
		local n=0
		while [ ! -r "/proc/$p/cmdline" ] && [ "$n" -lt 60 ]; do
			n=$((n + 1)); sleep 0.05
		done
		echo "$p"
	}
	alive() { if kill -0 "$1" 2>/dev/null; then echo alive; else echo dead; fi; }

	P="$SWEEPDIR/vmsync-qemu-nbd-web01-vda.pid"
	real="$(spawn "$P")"
	printf '%s\n' "$real" > "$P"
	sweep
	sleep 0.4
	check "an export whose argv carries its pidfile path is killed" dead "$(alive "$real")"
	check "and nothing is reported when the kill went ahead" "" "$(cat "$REFUSED")"

	# THE FINDING: a live process holding the recorded number, which is what a
	# recycled pid looks like.
	P="$SWEEPDIR/vmsync-qemu-nbd-web01-vdb.pid"
	stranger="$(spawn an-unrelated-process)"
	printf '%s\n' "$stranger" > "$P"
	sweep
	sleep 0.4
	check "a stranger holding the recorded pid is left alone" alive "$(alive "$stranger")"
	check "the refusal is reported, so a sweep that did nothing cannot read as a clean one" \
		yes "$(has 'does not carry it in argv' "$(cat "$REFUSED")")"
	kill -9 "$stranger" 2>/dev/null

	STUB_KILL="kill() { printf '%s\n' \"kill \$*\" >> $REC; }; "
	for junk in "" "notanumber" "-1" "0" "1" "123 456" "4194303"; do
		P="$SWEEPDIR/vmsync-qemu-nbd-web01-junk.pid"
		printf '%s\n' "$junk" > "$P"
		sweep
		check "a pidfile holding '$junk' attempts no signal" "" "$(cat "$REC" 2>/dev/null)"
	done
	STUB_KILL=""
else
	SKIPPED=1
	printf '  SKIP  this machine has no /proc, so the sweep cannot be exercised\n'
	printf '        against real processes here; the source rules above still ran\n'
fi

echo
echo "=== the orphan planter ==="
#
# plant_orphan builds the fixture BOTH planting stages depend on (23 and 26),
# and it is the one part of them that can be exercised without hardware: it
# makes three calls and reads what comes back, so stubbing those three calls
# drives every outcome. What this cannot check is whether libvirt and qemu
# actually behave as the stubs pretend -- that is what the stages themselves
# are for, and why each failure below carries a verdict an operator can act
# on rather than a bare SKIP.
#
# WHY IT IS HERE. The six ways planting fails used to be recorded as one, "no
# orphan was produced", which names a host behaviour. A run that stood down
# because libvirt refused the create, or because an earlier run's plant was
# still there, said the same words -- so the reader went to the host with a
# question the host had not answered.
if [ "$HAVE_PLANT" = no ]; then
	printf '  SKIP  this bench.sh has no plant_orphan, so the planter cannot be driven\n'
else
	SOURCE_URI=qemu+ssh://stub/system
	SOURCE_DOMAIN=stubvm
	PROBE_NAME=vmsync-cpt-2509010000-f00d-098001
	PROBE_XML="$WORK/probe.xml"
	TRACE="$WORK/trace"

	# cp_listed consumes CP_SEQ, one exit code per call, because the real
	# answer CHANGES either side of the create and the metadata delete and a
	# single stubbed value could not express "absent, then present, then
	# absent".
	CP_SEQ=""
	CREATE_RC=0
	CREATE_OUT=""
	HOLD_RC=0

	cp_listed() {
		local next="${CP_SEQ%% *}"
		case "$CP_SEQ" in
		*" "*) CP_SEQ="${CP_SEQ#* }" ;;
		*) CP_SEQ="" ;;
		esac
		printf ' listed:%s' "${next:-1}" >> "$TRACE"
		return "${next:-1}"
	}
	# Traced to a FILE, not a variable: the create runs inside a command
	# substitution, so a shell variable written in there never comes back.
	# The two deletes are distinguished because the difference is the whole
	# fixture -- a full delete takes the bitmap with it and plants nothing.
	virsh_uri() {
		case "$*" in
		*checkpoint-create*)
			printf ' create' >> "$TRACE"
			[ -n "$CREATE_OUT" ] && printf '%s' "$CREATE_OUT"
			return "$CREATE_RC"
			;;
		*checkpoint-delete*--metadata*)
			printf ' delete-meta' >> "$TRACE"
			return 0
			;;
		*checkpoint-delete*)
			printf ' delete-full' >> "$TRACE"
			return 0
			;;
		esac
		return 0
	}
	qemu_holds_probe() {
		printf ' hold:%s' "$HOLD_RC" >> "$TRACE"
		return "$HOLD_RC"
	}

	# plant CP_SEQ CREATE_RC CREATE_OUT HOLD_RC -> sets RC, PLANT_VERDICT
	plant() {
		CP_SEQ="$1"
		CREATE_RC="$2"
		CREATE_OUT="$3"
		HOLD_RC="$4"
		: > "$TRACE"
		plant_orphan && RC=0 || RC=$?
	}
	has_text() { case "$2" in *"$1"*) echo yes ;; *) echo no ;; esac; }

	plant "0" 0 "" 0
	check "a name that is already a checkpoint is refused" 1 "$RC"
	check "and named as such" "name was already a checkpoint" "$PLANT_VERDICT"
	check "and nothing is created or deleted first" " listed:0" "$(cat "$TRACE")"

	plant "1" 1 "error: Bitmap already exists: $PROBE_NAME" 0
	check "a create refused for the bitmap is an earlier run's orphan" \
		"name was already an orphan" "$PLANT_VERDICT"
	check "and the reason says whose it is" yes \
		"$(has_text "an earlier run left behind" "$PLANT_REASON")"

	plant "1" 1 "error: internal error: unable to execute QEMU command" 0
	check "a create refused for anything else is libvirt's refusal" \
		"plant refused by libvirt" "$PLANT_VERDICT"
	check "and libvirt's own words are quoted" yes \
		"$(has_text "unable to execute QEMU command" "$PLANT_REASON")"
	check "and the command is given to run by hand" yes \
		"$(has_text "virsh -c $SOURCE_URI checkpoint-create" "$PLANT_REASON")"

	# A create that exits 0 and lists nothing is still a refused plant: the
	# exit code is not the fixture, the checkpoint is.
	plant "1 1" 0 "" 0
	check "a create that exits 0 and lists nothing is still refused" \
		"plant refused by libvirt" "$PLANT_VERDICT"

	plant "1 0 0" 0 "" 0
	check "a metadata delete that does not take is named" \
		"metadata delete did not take" "$PLANT_VERDICT"

	plant "1 0 1" 0 "" 1
	check "a host whose metadata delete removes the bitmap is named" \
		"host produces no orphan" "$PLANT_VERDICT"

	# THE DISTINCTION THIS SECTION EXISTS FOR. An oracle that could not
	# answer is not a host that produces no orphans, and reporting it as one
	# attributes a behaviour to the host that this run never observed.
	plant "1 0 1" 0 "" 2
	check "an unanswered oracle is NOT reported as no-orphan" \
		"existence probe inconclusive" "$PLANT_VERDICT"
	check "and says that a question is not an answer" yes \
		"$(has_text "not an answer" "$PLANT_REASON")"

	plant "1 0 1" 0 "" 0
	check "a planted orphan reports success" 0 "$RC"
	check "with no verdict to record" "" "$PLANT_VERDICT"
	# ONE create, and a metadata-only delete. Both stages used to ask the
	# existence oracle BEFORE creating -- and that oracle answers "the name is
	# free" by creating a checkpoint and deleting it again, so on a host whose
	# normal delete leaves the bitmap the question manufactured the very thing
	# it was asking about and the real create was then refused.
	check "planting is one create and a metadata-only delete" \
		" listed:1 create listed:0 delete-meta listed:1 hold:0" "$(cat "$TRACE")"

	# probe_gone exists to stop `! qemu_holds_probe` reading an unanswerable
	# question as good news, which four call sites did.
	for rc in 0 1 2; do
		HOLD_RC="$rc"
		probe_gone && g=gone || g=not-gone
		case "$rc" in
		1) check "probe_gone reads 1 as gone" gone "$g" ;;
		*) check "probe_gone does not read $rc as gone" not-gone "$g" ;;
		esac
	done
fi

if [ "$HAVE_GREPQ" = no ]; then
	echo "=== grep_q: NOT PRESENT in this bench.sh, pipeline cases skipped ==="
else
	echo
	echo "=== grep_q: a match must never read as a non-match ==="

	# What decides this is not how much the writer prints but whether it still
	# has a write LEFT once grep has exited. The real case had 70 bytes of
	# output in it: `virsh checkpoint-list --name` printed two checkpoints,
	# grep -q matched the first and exited, and virsh died on the second.
	#
	# So the fixture puts the match first and leaves well over a pipe buffer
	# still to come. 200000 lines is more than a megabyte against a 64 KiB
	# buffer, so the writer cannot possibly have finished and the SIGPIPE is
	# guaranteed rather than raced for. `seq 1 5000` was the first attempt and
	# was wrong for exactly this reason -- about 24 KB, it fits the buffer
	# whole, seq exits happily before grep matches, and the control below
	# reported a healthy 0 for a form that is broken in the field.
	early() { printf 'MATCH\n'; seq 1 200000; }
	late() { seq 1 200000; printf 'MATCH\n'; }

	# THE CONTROL, and it comes first deliberately. A test for a cure is
	# worthless unless the disease is present on the machine running it. If
	# this reports 0, pipelines are not dying here and every assertion below
	# would pass just as well against the broken form, proving nothing.
	#
	# pipefail is turned on inside the subshell because bench.sh sets it
	# globally (bench.sh:30) and this harness deliberately does not. Without
	# it the pipeline reports grep's own status and the bug is invisible --
	# which is why it survived in a file this one has been testing all along.
	old_rc=0; ( set -o pipefail; early | grep -q '^MATCH$' ) || old_rc=$?
	check "the bare grep -q form does fail here, so these cases mean something" 141 "$old_rc"

	# The bug itself: the match is EARLY, which is the shape that bit
	# cp_listed on 2026-11-03 -- two checkpoints, the wanted one listed first,
	# reported as absent, and stage 23 standing down over it.
	new_rc=0; ( set -o pipefail; early | grep_q '^MATCH$' ) || new_rc=$?
	check "grep_q reports an early match as a match" 0 "$new_rc"

	# The case that always worked, kept so a fix that broke it would show.
	last_rc=0; ( set -o pipefail; late | grep_q '^MATCH$' ) || last_rc=$?
	check "grep_q reports a last-line match as a match" 0 "$last_rc"

	# And absence must still be absence. A cure that answered 0 for everything
	# would pass every case above and turn every existence check in the file
	# into a tautology.
	none_rc=0; ( set -o pipefail; early | grep_q '^nope$' ) || none_rc=$?
	check "grep_q still reports a genuine absence as absence" 1 "$none_rc"

	# Flags have to survive the wrapper: the converted call sites pass -x, -E
	# and -F, and "$@" is the only reason they can.
	x_rc=0; ( set -o pipefail; early | grep_q -x 'MATCH' ) || x_rc=$?
	check "grep_q passes -x through" 0 "$x_rc"
	# The discriminating half, and the case above cannot do without it: a
	# wrapper that swallowed -x altogether would still find MATCH inside
	# XMATCHX, so "passes -x through" would pass on a wrapper that does not.
	padded() { printf 'XMATCHX\n'; seq 1 200000; }
	xs_rc=0; ( set -o pipefail; padded | grep_q -x 'MATCH' ) || xs_rc=$?
	check "grep_q -x means whole line, not substring" 1 "$xs_rc"
	e_rc=0; ( set -o pipefail; early | grep_q -E '^(MATCH|nope)$' ) || e_rc=$?
	check "grep_q passes -E through" 0 "$e_rc"
	# -F with a string that is a live regex if it is not taken literally.
	f_rc=0; ( set -o pipefail; printf '%s\n' 'a.c' 'zzz' | grep_q -F 'a.c' ) || f_rc=$?
	check "grep_q passes -F through" 0 "$f_rc"
	f2_rc=0; ( set -o pipefail; printf '%s\n' 'abc' 'zzz' | grep_q -F 'a.c' ) || f2_rc=$?
	check "grep_q -F is not quietly a regex" 1 "$f2_rc"

	# No local caller may go back to the trap. The three that remain are
	# inside quoted strings that run on the TARGET host under its own shell,
	# which has no pipefail, so they are safe where they are -- and they are
	# recognised by the escaped quoting that makes them remote.
	stray=0
	while IFS= read -r line; do
		case "$line" in
		*'\"'*|*'\$'*) continue ;;
		*'#'*'grep -q'*) continue ;;
		esac
		stray=$((stray + 1))
	done < <(grep -E '\|[[:space:]]*grep[[:space:]]+-[a-zA-Z]*q' "$BENCH" || true)
	check "no local pipeline in bench.sh still ends in a bare grep -q" 0 "$stray"
fi

echo
echo "=== bench_sync_hint belongs INSIDE a message ==="

# The hint is built with printf and returns its text; it is not a logger. Four
# stages called it as a bare statement, so its text went to stdout raw -- no
# timestamp, no "WARNING:", and glued to whatever the next log line started
# with. Observed on 2026-11-03, where stage 26's stand-down printed
#
#     exit=1 wall=3.038s mode=full transferred=0B
#      -- if that run ended in under a second ...installed there[2026-11-03 ...
#
# The other twenty-three call sites interpolate it, which is what puts it in
# the message the reader is already reading. This counts the exceptions.
bare=0
while IFS= read -r line; do
	bare=$((bare + 1))
done < <(grep -nE '^[[:space:]]*bench_sync_hint$|\{[[:space:]]*bench_sync_hint;' "$BENCH" || true)
check "no stage calls bench_sync_hint as a bare statement" 0 "$bare"

echo
if [ "$SKIPPED" -ne 0 ]; then
	printf '%d passed, %d failed -- the sweep cases were SKIPPED on this machine, so a clean run here does not mean the sweep is safe\n' "$PASS" "$FAIL"
else
	printf '%d passed, %d failed\n' "$PASS" "$FAIL"
fi
[ "$FAIL" -eq 0 ]
