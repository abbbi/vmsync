#!/usr/bin/env bash
# vmsync benchmark / integration harness.
#
# Drives a real `vmsync` binary against a real source/target VM pair over
# the full combination of transport settings (bridge on/off, -use-ssh,
# -compress algo/level, -netbuffer), plus full sync, incremental sync,
# -reinit, -reinit-after-failures, all three -verify modes with deliberate
# target-side disk tampering to confirm mismatch detection actually works,
# and the external-snapshot lifecycle (sync while a source-side external
# snapshot exists, then again after it's removed) -- reporting a
# wall-clock sync time (and, where the textfile has it, bytes transferred)
# for every run.
#
# SAFETY: this is a genuinely destructive tool. It repeatedly -reinit's the
# target replica (deletes and recreates it from scratch), and deliberately
# corrupts the target's disk file to test verify detection (always healed
# with a full -reinit resync immediately after, but a script bug or an
# interrupted run could leave the target replica in a bad state). Point
# this at a disposable test VM, never a real, in-use replication pair. See
# README.md before running this anywhere near production.
#
# Usage:
#   cp bench.conf.example bench.conf   # then edit it
#   ./bench.sh --dry-run               # print every command, touch nothing
#   ./bench.sh                         # run for real (needs bench.conf's
#                                       # I_UNDERSTAND_THIS_IS_DESTRUCTIVE=yes)
#
# See ./bench.sh --help for all options.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"

CONF="$SCRIPT_DIR/bench.conf"
SCENARIOS="$SCRIPT_DIR/scenarios.conf"
DRY_RUN=no
ONLY_PATTERN=""
STAGES="matrix,verify,reinit,snapshot,journal,retention"

# INTERRUPTED: vmsync catches SIGINT/SIGTERM itself (cleanup, then a plain
# os.Exit(1) -- see cmd/vmsync/main.go's own signal handling), so a Ctrl+C
# that lands while a vmsync child is running exits *normally* as far as
# bash is concerned; the usual "child died from an uncaught signal"
# propagation never fires, and this script would otherwise just log that
# one combination as failed and move on to the next of however many
# hundred are queued. Trapping the signal here and checking the flag right
# after every run_vmsync call (its one common chokepoint) makes Ctrl+C
# actually stop the whole harness -- after finishing the report for
# whatever already ran, not silently mid-write.
INTERRUPTED=no
trap 'INTERRUPTED=yes' INT TERM

usage() {
        cat <<'EOF'
Usage: bench.sh [options]

Options:
  -c, --config FILE       config file (default: ./bench.conf)
  -s, --scenarios FILE    transport matrix file (default: ./scenarios.conf)
  --only PATTERN          only run Stage 1 scenarios whose name matches
                           PATTERN (a bash glob, e.g. "compress-zstd-*")
  --stages LIST           comma-separated subset of, in stage-number order:
                             1  matrix        8  verify-long
                             2  verify        9  retention
                             3  reinit       10  restore
                             4  snapshot     11  invert
                             5  define       12  wedge
                             6  failover     13  checksum
                             7  fence-agent  14  verify-failure
                                            15  commit-barrier
                                            16  interrupted-reinit
                                            17  journal
                                            18  colocated
                           Runs in whichever order LIST gives them, not a
                           fixed canonical one.
                           (default: matrix,verify,reinit,snapshot,journal,
                           retention; retention is last because it
                           reinitialises the target, and it skips cleanly
                           where the target filesystem cannot reflink. define,
                           failover, fence-agent, verify-long, restore,
                           invert, wedge, checksum, verify-failure,
                           commit-barrier, interrupted-reinit and colocated
                           are opt-in, see below. commit-barrier additionally
                           needs a source domain with two or more qcow2 disks,
                           and skips cleanly without one. colocated needs
                           TARGET_DISK_PATH set and a target filesystem that
                           can reflink, and skips cleanly without either)
  --dry-run               print every vmsync command line; touch nothing
                           (no ssh/qemu-io/vmsync calls actually made)
  -h, --help              this text

Stages 2, 3, 4, 9, 10, 11, 13, 14 and 16 each start with their own baseline
full sync, so none of them actually require Stage 1 (or any prior sync) to have
run first -- each is safe to run standalone via --stages. Stage 17 needs no
baseline at all: its one sync is whatever the pair's state makes it, full or
incremental, and either answers what it asks. Stage 4 additionally
requires the SOURCE domain to be running. (Stage 14's baseline is -force-clean
rather than -reinit, because a re-run after a previous attempt left a
verification failure recorded would otherwise be refused by the very interlock
it tests.)

Stage 5 (define) is NOT included by default -- pass --stages ...,define
explicitly. 5a leaves a throwaway domain on the target briefly to force a
UUID collision; 5b runs one sync with vmsync's own -test=failure-define,
making the target redefine fail so the rollback to the previous definition
can be checked; 5c then checks that the same rollback left vmsync's OWN
metadata intact, which 5b's comparison deliberately excludes (vmsync rewrites
that metadata mid-run, so comparing it raw made 5b a false failure) -- a
restored definition whose checkpoint and replica_source were wiped is a
replica the next sync cannot continue and a promotion cannot describe. All
three are no more destructive to the target VM than -reinit already is, and
none touches host-level networking, but they are deliberate failure injection
rather than measurement. See this file's own Stage 5 comment.

Stage 6 (failover) is also NOT included by default. It promotes the target,
arms and inspects a fence, checks who owns the target's disks, and puts the
target back to `target` with a fresh sync -- it never stops a domain and
never reverses the pair. It is opt-in because it promotes a real replica:
it puts the target back afterwards and an EXIT trap does the same on a die
or a Ctrl+C, but a kill -9, or a way back that itself fails, leaves the
target `promoted` -- and a promoted target refuses EVERY later sync in the
estate, not just this harness's. Both this stage and Stage 7 wipe vmsync's
metadata off the domains they use before starting -- via virsh, not through
vmsync itself, so a broken write path cannot also break the reset -- and skip
with the exact remedy if that does not take. It needs TARGET_VMSYNC_BIN set
in the config (vmsync on the TARGET host): -promote refuses a remote libvirt
URI by design, so it must run where the domain is. Without that setting the
stage skips rather than failing. Note it runs THREE full syncs in total: its
own baseline plus two more the disk-ownership checks need, since each
property they test only happens when a disk file is created from scratch.

Stage 7 (fence-agent) is opt-in too, and is the most intrusive thing here:
it STOPS THE SOURCE VM. It runs real vmsync-agents in --standalone mode and
proves a fence is actually acted on, not merely written. It restores the
source's power state and both roles afterwards (and on a crash, via an EXIT
trap). Needs SOURCE_AGENT_BIN and TARGET_VMSYNC_BIN.

Stage 8 (verify-long) is opt-in and slow: it builds a replica the way a real
deployment does -- twenty incremental syncs carrying real guest writes --
then corrupts it part way along that chain and checks -verify still finds it.

Stage 9 (retention) IS a default. It covers -retention end to end and skips
cleanly, rather than failing, where the target filesystem cannot reflink.

Stage 10 (restore) is opt-in: it rolls the replica back to a restore point
and leaves replication paused mid-stage, clearing that and healing on the
way out.

Stage 11 (invert) is opt-in and is the OTHER stage that stops the source VM.
It has to: -invert refuses while the old source is running, since the
inversion would make it a replication target, and asserting that refusal is
part of what the stage tests. It shuts the source down gracefully, never
destroys it, and starts it again whatever happened.

Stage 12 (wedge) is opt-in: it makes one sync fail on purpose, then proves
the NEXT sync still succeeds -- that a failed run leaves nothing behind
(stale export, uncommitted overlay, half-written metadata) that would refuse
the run after it.

Stage 13 (checksum) is opt-in and covers the pre-commit integrity check --
the digest exchange with vmsync-bridge-helper that refuses to commit an
incremental sync's overlay when the target's bytes do not match what was
sent. It is the one stage written mainly as a NEGATIVE test, because that
check is ON by default and a check that silently never fires looks exactly
like a check that works: every other stage here would keep reporting PASS
either way. Five sub-tests: an ordinary sync must report the check ran and
matched; a helper reporting one falsified digest must fail the run, remove
the overlay and leave the base untouched; a helper too old to send a format
header must be reported as version skew rather than as a corrupt replica;
-no-checksum must genuinely skip the check; and -- the one that makes the
falsified-digest test mean anything -- GENUINELY corrupt bytes must be caught,
with the corruption proven absent from the base afterwards at the exact offset
it was written to. Neither corruption can be applied from outside, since the
check reads the target back inside one vmsync process: the falsified reply
comes from a wrapper script installed under /tmp on the TARGET host that runs
the real helper and edits its answer (removed on the way out whichever way the
stage ends), and the real corruption comes from vmsync's own
-test=corrupt-before-checksum, which writes into the image between the copy
finishing and the check reading it back. The difference matters, because a
vmsync hashing the wrong ranges or a helper hashing the overlay's backing file
would pass the shim test and fail the real one. Opt-in because two sub-tests
deliberately fail a sync, one substitutes the helper binary vmsync is pointed
at, and one corrupts real bytes (healing after itself where that leaves the
base damaged). Stage 1 passes -no-checksum on every cell so its transport
numbers are not carrying this check's cost; this stage is where that cost
belongs.

Stage 14 (verify-failure) is opt-in and tests what happens AFTER -verify finds
a difference -- stage 2 proves the difference is noticed, this proves the
notice outlives the process. It corrupts the replica, then asserts six things
in the order the state moves: the finding is written to the target domain
(verify_state/verify_failed_at); an ordinary sync is refused while it stands,
without being counted toward -reinit-after-failures; a plain -reinit is
refused too (it recopies but never verifies, so allowing it would move the
replica from "known bad" to "assumed good" while erasing the record); and
-verify-failure-reinit repairs it, with only a PASSING verify clearing the
record. That fourth sub-test drives the whole ladder unaided: the run it starts
is an incremental, which does not touch the tampered offset, so its own verify
fails and the recopy-and-re-verify fires for real. A fifth then covers the
branch a tamper cannot reach -- the repair's own verify failing, so vmsync
stops rather than trying a third time -- using -test=corrupt-after-commit, which
corrupts the replica after each copy is committed and so fails both rungs of
the ladder. A sixth runs last, on the record that fifth one deliberately leaves
standing, and asserts what keeping a record is ultimately FOR: a -promote
against a replica recorded as having failed verification is refused, by name in
the log rather than merely by a non-zero exit, and without leaving the domain
promoted -- while -force-promote still gets past it and records exactly what it
overrode. That last one needs TARGET_VMSYNC_BIN set (vmsync on the TARGET
host), since -promote refuses a remote libvirt URI by design, and skips rather
than failing without it. Opt-in because four of the six sub-tests deliberately
fail a sync, one deliberately corrupts the replica, and one promotes it --
putting the role straight back afterwards, since the stage's own heal is a sync
and a promoted domain refuses those.

Stage 15 (commit-barrier) is opt-in and proves the one property that needed a
multi-disk domain to state at all: when ONE disk fails, NO disk commits. Each
disk copies into its own overlay beside the base and checksums it there; only
once EVERY disk has got that far do the commits run. Before that barrier each
disk committed inside its own worker, so a domain whose third disk failed was
left with two disks at the new checkpoint and one at the old -- a replica no
restore point describes, and one that looks perfectly healthy from the
outside. The stage takes a baseline, dirties the guest, then fails a run on
purpose with -test=fail-last-disk, which refuses the last disk with everything
about it correct: its copy and its digest check both passed. The assertion is
made against the target's own base images rather than the log -- mtime, size
and allocated blocks for every base, before and after -- because qemu-img
commit writes the base, so a disk that committed cannot hide it. Not one
fingerprint may move. Two sub-tests guard the ends: the fault must actually
have fired (the injection line in the log AND at least two disks staged, so a
run that died before the barrier could not report a pass for a barrier it
never reached), and a clean incremental afterwards must still commit and move
the bases -- without which the stage would pass just as happily against a
vmsync that had stopped committing altogether. Opt-in because one sub-test
deliberately fails a sync; it also needs a source domain with two or more
qcow2 disks and skips cleanly without one, since on a single disk the barrier
has nothing to hold back.

Stage 16 (interrupted-reinit) is opt-in and covers the one failure a promotion
used to accept. A full copy -- `-reinit`, `-force-clean`, or any sync that
writes bases directly -- renames the good replica disks aside and writes new
ones with no overlay, while the target domain keeps its OLD metadata. A run
killed in that window leaves last_checkpoint, last_sync_timestamp,
replica_source and failure_count all still describing the replica that was
replaced, so every evidence check reads healthy and `-promote` boots a
half-written machine reporting an ordinary data-loss window -- with the
complete copy sitting unused in the `.vmsync-replaced-<unixtime>` files beside
it. The stage reaches that state with vmsync's own `-test=die-writing-base`,
which kills the process (exit 137, nothing unwound) once a base has been
created and written, and then asserts the whole ladder: the run died where the
fault says it did; the target carries `replica_incomplete` with ONE aside
stamp while last_checkpoint is untouched; there is exactly one
`.vmsync-replaced-<stamp>` per disk and they all share that one stamp;
`-promote` is REFUSED naming the interrupted rebuild and that exact suffix;
`-force-promote` gets past it and reports the window as unknown; putting the
aside files back restores the complete replica byte for byte and it can then
be force-promoted, while a plain promote is still refused because a rename on
the target tells vmsync nothing; and finally a rebuild run to completion
clears the record and a plain `-promote` is accepted again with a measured
window. It forces `-replaced-disk-action=rename` for its own runs whatever
REPLACED_DISK_ACTION says, because with `delete` there are no aside files and
three of those assertions would have nothing to look at; the setting is put
back on the way out. Needs TARGET_VMSYNC_BIN (`-promote` refuses a remote
libvirt URI by design) and skips rather than failing without it. Opt-in
because it deliberately leaves the target half-written, promotes it three
times and rebuilds it -- it puts the role back each time and an EXIT trap does
the same on a die or a Ctrl+C, but a kill -9 leaves the target promoted, and a
promoted target refuses EVERY later sync in the estate.

Stage 17 (journal) IS a default, and is cheap: one ordinary sync, no reinit.
It proves the action journal beside the replica's disks
(`<disk dir>/.vmsync-journal/<domain>.jsonl` on the TARGET host) is actually
being written -- one intent record before the verb acts, one outcome when it
stops, joined by (aid, seq) and in that order -- and that a sync which
finishes leaves no `replica_incomplete` behind. It is in the default list
because of what it guards: the journal's whole value is the record of an
action that DIED, where the intent is written and the outcome never is, and a
journal that had quietly stopped being written would look exactly like an
estate where nothing ever crashed. Every other stage here would keep reporting
PASS either way. It reads past the existing file rather than truncating it.

Stage 18 (colocated) is opt-in, and needs TARGET_DISK_PATH set. It proves that
two target domains sharing one replica directory keep independent restore point
histories. Restore points used to be keyed by that DIRECTORY rather than by the
domain, so every policy decision was taken over the union of both machines'
points: one domain's copy satisfied the other's interval floor (a replica could
go for days taking none), a retention count meant for one machine was spread
across several, the sweep of abandoned staging directories removed a
concurrently running sibling's in-flight set, and a -reinit of one replica
rm -rf'd every co-located replica's entire history in a single command. The
co-located domain is planted rather than replicated -- a second live pair would
race the first, and what has to be proved is only that this domain's sync never
touches directories that are not its own.

Stage 19 (leftovers) is opt-in, and needs TARGET_DISK_PATH set. It proves the
two halves of -reclaim-leftovers-after: that every run REPORTS the displaced sets
beside a replica whether or not reclaiming is on, and that reclaiming takes only
what it was asked for. The files are made by the DEFAULT -replaced-disk-action,
one full-size copy per disk per rebuild, sharing extents on the day they are made
and approaching a whole replica afterwards -- and before this existed nothing
removed or even named any of them, so the cost surfaced as an ENOSPC that failed
a commit for every VM on the DR host. Four of the sub-tests are negative, and
they are the ones worth having: a duration under the 24h floor must stop the run
before it acts, a fresh aside must survive a year-long duration, a co-located
domain's aside must be left alone (the sweep reads this domain's disks, not the
directory -- stage 18's lesson in a new reader), and a replica marked
replica_incomplete must have nothing reclaimed at all, because there the aside
set is the only complete copy of the replica there is. That last one is produced
with the same injected fault stage 16 uses, and it also checks that the sweep
standing down is a warning rather than a failed sync -- the run it happens inside
is the one repairing the replica. Ages are faked by rewriting the unix stamp in
each name rather than by moving a clock: the stamp is what vmsync reads, and
moving the host's time would invalidate every other timestamp the replica carries.

Stage 20 (reinit-order) is opt-in, and is the ONLY stage that deliberately starts
a target domain. It proves that a reinit refused because the target is running
has destroyed nothing -- on either host. That check used to sit after the source's
checkpoint chain had already been dropped, so forgetting to shut the replica down
cost a full recopy of the whole machine, left replica_incomplete armed on a target
the run never touched, and with -force-clean undefined the target first and only
then noticed it was running (DomainExists uses LookupDomainByName, which still
finds a RUNNING domain after its definition is gone). The replica is started
PAUSED, which is what makes this safe to ship: libvirtsync.DomainActive is
`state != SHUTOFF`, so a paused domain trips the guard exactly like a running one
while the guest never executes an instruction, so it cannot write to the replica
or appear on the network as a second copy of a production machine. qemu does open
the images, so the stage rewrites the replica before it finishes. It skips
cleanly, saying which, when the target domain is absent, when the replica will
not boot on the DR host (a definition inherited from the source can name a bridge
or a CPU model that host does not have), or when the source carries no checkpoint
for the assertions to protect.

Stage 21 (lock-lease) is opt-in and needs TARGET_HOST. It proves the target stops
holding its run lock for a driver that has gone. The driver is SIGSTOPped rather
than killed, and that distinction is the stage: killing a process closes its
sockets, so the target gets a FIN and releases the lock promptly -- the case that
always worked. A stopped process keeps every socket open and sends nothing, which
is exactly what a partitioned or powered-off host looks like from the target, and
it needs no firewall rule and is fully reversible. With vmsync-bridge-helper on
the target the lock is leased and the target releases it once the heartbeats stop;
the stage checks it is still held BEFORE the lease is up, because a lock that goes
early can be handed to a promotion while the driver is still writing. It then
re-runs the same scenario with -bridge-helper-path pointing at nothing, and
asserts the opposite: an unleased lock is still held long afterwards, and
-break-target-lock refuses it because a shell lock records no holder to prove gone.
Both halves matter -- the second is what makes the WARNING every unleased run
prints worth trusting. Finally it resumes the stalled driver and checks it refuses
to commit, since by then the lock may belong to a promotion.

Stage 22 (redefine-probe) is a PROBE, not a test, and it is the only stage here that
grades libvirt rather than vmsync. vmsync implements none of what it exercises; the
stage asks whether it CAN be implemented, so its answer decides a design. The question:
-reinit cannot clear a vmsync-cpt-* bitmap that libvirt holds no checkpoint for while
the source is running, because qemu has the image open and qemu-img cannot write it. The
proposed way out is to ADOPT the orphan -- redefine checkpoint metadata naming the
existing bitmap, then delete that checkpoint so qemu removes the bitmap through its own
path. The stage manufactures a real orphan the way reality does (create a checkpoint,
then delete it --metadata, which is the documented orphan factory), then tries to adopt
it both from libvirt's own dumped xml and from an xml built by hand. The hand-built case
is the one that matters: a real orphan has no dumped xml anywhere, which is what makes it
an orphan. It needs a RUNNING source, and it repeats the sequence while PAUSED because
that is the state -reinit -start leaves a source in. It plants vmsync-cpt-099001, far
outside any real chain, and if it cannot clear it again it says so at WARNING with the
exact qemu-img command and the fact that -reinit will refuse until somebody runs it.
Every step logs what virsh and qemu-img actually said before any verdict is read from
it, and a check whose evidence is unreadable is recorded SKIP rather than passed --
where qemu-img reports no bitmap of a name, "the bitmap is gone" and "it was never
visible here" read identically. It makes its own starting point first, which is
DESTRUCTIVE: it deletes every libvirt checkpoint on the source, so the next sync for
that pair is a FULL one. Removing a leftover BITMAP additionally needs the domain
stopped, so that half needs the environment variable PROBE_MAY_STOP_SOURCE=yes, and
without it the stage stands down and prints the commands to do it by hand. Whether
the probe name is free is then confirmed with qemu rather than with the image, since
a bitmap made during the running qemu's life need not be in the image at all.
EOF
}

while [ $# -gt 0 ]; do
        case "$1" in
        -c | --config)
                CONF="$2"
                shift 2
                ;;
        -s | --scenarios)
                SCENARIOS="$2"
                shift 2
                ;;
        --only)
                ONLY_PATTERN="$2"
                shift 2
                ;;
        --stages)
                STAGES="$2"
                shift 2
                ;;
        --dry-run)
                DRY_RUN=yes
                shift
                ;;
        -h | --help)
                usage
                exit 0
                ;;
        *)
                die "unknown argument: $1 (see --help)"
                ;;
        esac
done

[ -f "$CONF" ] || die "config file not found: $CONF (copy bench.conf.example to bench.conf and edit it)"
# shellcheck source=/dev/null
source "$CONF"
[ -f "$SCENARIOS" ] || die "scenarios file not found: $SCENARIOS"

: "${VMSYNC_BIN:?set in $CONF}"
: "${SOURCE_URI:?set in $CONF}"
: "${TARGET_URI:?set in $CONF}"
: "${SOURCE_DOMAIN:?set in $CONF}"
: "${TARGET_DOMAIN:?set in $CONF}"
: "${SOURCE_HOST:?set in $CONF}"
: "${TARGET_HOST:?set in $CONF}"
: "${TAMPER_DISK_DEV:?set in $CONF}"
: "${TAMPER_OFFSET:?set in $CONF}"
: "${TAMPER_LENGTH:?set in $CONF}"
: "${RESULT_DIR:?set in $CONF}"
SOURCE_LOCAL="${SOURCE_LOCAL:-yes}"

# --- what -reinit does with the disk it replaces -----------------------------

# `delete` here, though vmsync itself defaults to `rename`, and the difference
# is about which risk applies where.
#
# vmsync is right to default to renaming: the target of a -reinit may be a
# former primary whose disks still hold everything written after the last
# successful sync, and that is unrecoverable once deleted. None of that is true
# of this harness's target, which bench.conf.example insists must be a
# disposable, dedicated test VM and which this script deletes and recreates
# dozens of times per run anyway.
#
# What IS true here is the cost. Every -reinit leaves a full-size aside copy
# that, as the flag's own help says, is never reaped automatically -- and this
# harness reinits constantly: once per verify sub-test, once per heal, four
# times in Stage 8 alone. On a multi-GB disk that fills TARGET_DISK_PATH partway
# through a long run, and the failure surfaces as a confusing mid-stage sync
# error rather than as "you are out of disk".
REPLACED_DISK_ACTION="${REPLACED_DISK_ACTION:-delete}"
case "$REPLACED_DISK_ACTION" in
delete | rename) ;;
*) die "$CONF: REPLACED_DISK_ACTION must be 'delete' or 'rename', not '$REPLACED_DISK_ACTION'" ;;
esac

# --- tamper placement --------------------------------------------------------

TAMPER_MODE="${TAMPER_MODE:-random}"
TAMPER_BAND_START="${TAMPER_BAND_START:-$TAMPER_OFFSET}"
TAMPER_BAND_END="${TAMPER_BAND_END:-0}" # 0 = up to the disk's virtual size
# 64 KiB, not 512, and that floor is load-bearing rather than cautious.
# nbdsync reports mismatches at a 4096-byte granularity (mismatchScanGranularity
# in pkg/nbdsync/nbd.go), so anything under 4 KiB is indistinguishable from a
# 4 KiB tamper and buys no coverage at all.
#
# 64 KiB specifically is a realism choice, not a detection threshold. No mode
# reconciles a reported range against a dirty bitmap -- every mode compares
# against the same frozen snapshot the copy read from, so a tamper of any size
# is reported. A mode that did discard a reported range overlapping a region
# the guest had written would take those regions from a dirty bitmap at qemu's
# default 64 KiB granularity, and a smaller tamper could be swallowed whole.
# The floor is kept because a tamper at bitmap granularity is the more
# realistic corruption shape, not because a smaller one would be missed.
TAMPER_LENGTH_MIN="${TAMPER_LENGTH_MIN:-65536}"
TAMPER_LENGTH_MAX="${TAMPER_LENGTH_MAX:-262144}"
TAMPER_ALIGN="${TAMPER_ALIGN:-4096}"
TAMPER_PATTERN="${TAMPER_PATTERN:-0xAA}"
TAMPER_SEED="${TAMPER_SEED:-}"

case "$TAMPER_MODE" in
random | fixed) ;;
*) die "$CONF: TAMPER_MODE must be 'random' or 'fixed', not '$TAMPER_MODE'" ;;
esac

# Plain decimal byte counts only, for everything this script does arithmetic
# on. qemu-io accepts k/M/G suffixes, so they are a tempting thing to write
# here, but $(( 100M )) is a bash syntax error, and under `set -e` that ends
# the run with a bare arithmetic complaint rather than anything actionable.
for _v in TAMPER_OFFSET TAMPER_LENGTH TAMPER_BAND_START TAMPER_BAND_END \
	TAMPER_LENGTH_MIN TAMPER_LENGTH_MAX TAMPER_ALIGN; do
	case "${!_v}" in
	'' | *[!0-9]*)
		die "$CONF: $_v must be a plain decimal byte count, not '${!_v}' -- size suffixes like 100M are not accepted here (write 104857600)"
		;;
	esac
done
unset _v

# A zero fill is a content no-op wherever the source already reads as zero,
# and undetectable by every verify mode by design -- so a run configured that
# way would report "verify missed it" for a corruption that was never there.
case "$TAMPER_PATTERN" in
0x00 | 0x0 | 0) die "$CONF: TAMPER_PATTERN must be non-zero -- a zero fill is undetectable wherever the source already reads as zero" ;;
esac

[ "$TAMPER_LENGTH_MIN" -le "$TAMPER_LENGTH_MAX" ] \
	|| die "$CONF: TAMPER_LENGTH_MIN ($TAMPER_LENGTH_MIN) exceeds TAMPER_LENGTH_MAX ($TAMPER_LENGTH_MAX)"
[ "$TAMPER_ALIGN" -gt 0 ] || die "$CONF: TAMPER_ALIGN must be positive"

# --- guest dirtying ----------------------------------------------------------

GUEST_DIRTY="${GUEST_DIRTY:-yes}"
GUEST_DIRTY_PATH="${GUEST_DIRTY_PATH:-/var/tmp/vmsync-bench-dirty}"
GUEST_DIRTY_MIB="${GUEST_DIRTY_MIB:-64}"
# How long to wait for the source's guest agent to come up before giving up on
# it. Stages that need the agent restart the source (or follow a stage that
# did), and `virsh start` returns while the guest is still booting -- see
# wait_for_guest_agent. Raise it for a guest that boots slowly; the wait costs
# nothing when the agent is already answering.
GUEST_AGENT_TIMEOUT="${GUEST_AGENT_TIMEOUT:-120}"
[ "$GUEST_AGENT_TIMEOUT" -ge 0 ] 2>/dev/null || die "$CONF: GUEST_AGENT_TIMEOUT must be a non-negative integer"

# --- Stage 8 (verify-long) ---------------------------------------------------

# Copies per MODE, not per stage: each mode gets its own chain, because the
# -reinit that heals a tamper also destroys the chain (see stage_verify_long).
VERIFY_LONG_COPIES="${VERIFY_LONG_COPIES:-20}"
VERIFY_LONG_MODES="${VERIFY_LONG_MODES:-fast full qemu-img}"

for _m in $VERIFY_LONG_MODES; do
	case "$_m" in
	fast | full | qemu-img) ;;
	*) die "$CONF: VERIFY_LONG_MODES may only contain fast, full or qemu-img -- got '$_m'" ;;
	esac
done
unset _m
[ "$VERIFY_LONG_COPIES" -ge 1 ] 2>/dev/null || die "$CONF: VERIFY_LONG_COPIES must be a positive integer"

if [ "$DRY_RUN" != yes ]; then
        [ "${I_UNDERSTAND_THIS_IS_DESTRUCTIVE:-no}" = yes ] \
                || die "$CONF: set I_UNDERSTAND_THIS_IS_DESTRUCTIVE=yes to run for real (or pass --dry-run to just print commands)"
fi

# --- setup ---------------------------------------------------------------

mkdir -p "$RESULT_DIR"
RUN_ID="$(date +%Y%m%d-%H%M%S)"
RUN_DIR="$RESULT_DIR/$RUN_ID"
mkdir -p "$RUN_DIR/logs" "$RUN_DIR/prom"
CSV="$RUN_DIR/results.csv"
results_init "$CSV"

# Seeded from the run id when unset, and logged either way. A randomly placed
# corruption is only worth having if the exact sequence can be replayed: rerun
# with TAMPER_SEED=<this value> and every draw below repeats identically.
TAMPER_SEED="${TAMPER_SEED:-$RUN_ID}"

log "vmsync benchmark harness -- run id $RUN_ID"
log "config: $CONF"
log "scenarios: $SCENARIOS"
log "results directory: $RUN_DIR"
if [ "$TAMPER_MODE" = random ]; then
	log "tamper placement: random, seed $TAMPER_SEED -- rerun with TAMPER_SEED=$TAMPER_SEED to reproduce this run's corruptions exactly"
else
	log "tamper placement: fixed, offset $TAMPER_OFFSET length $TAMPER_LENGTH"
fi
[ "$DRY_RUN" = yes ] && log "DRY RUN: no vmsync/ssh/qemu-io commands will actually execute"

# --- preflight -------------------------------------------------------------

preflight() {
        if [ "$DRY_RUN" = yes ]; then
                log "preflight: skipped entirely (--dry-run needs nothing but bash itself)"
                return
        fi

        command -v "$VMSYNC_BIN" >/dev/null 2>&1 || die "vmsync binary not found or not executable: $VMSYNC_BIN"
        command -v virsh >/dev/null 2>&1 || die "virsh not found locally -- needed to introspect domains via the qemu+ssh:// URIs"
        command -v xmllint >/dev/null 2>&1 || die "xmllint not found locally (libxml2-utils/libxml2 package) -- needed to read domain XML reliably"
        command -v awk >/dev/null 2>&1 || die "awk not found"
        command -v ssh >/dev/null 2>&1 || die "ssh not found"
        command -v md5sum >/dev/null 2>&1 || die "md5sum not found -- used to draw reproducible random tamper offsets (see TAMPER_SEED). Set TAMPER_MODE=fixed in $CONF to avoid needing it."
        # -verify=qemu-img shells out to `qemu-img compare` on the host running
        # vmsync, not on either hypervisor (pkg/disk/disk.go's CompareImages).
        # Missing it locally makes that one mode fail for a reason that has
        # nothing to do with the replica.
        command -v qemu-img >/dev/null 2>&1 || die "qemu-img not found locally -- -verify=qemu-img runs it on this host to compare the two NBD exports"

        domain_exists "$SOURCE_URI" "$SOURCE_DOMAIN" || die "source domain '$SOURCE_DOMAIN' not found via $SOURCE_URI${VIRSH_ERR:+: $VIRSH_ERR}"
        if domain_exists "$TARGET_URI" "$TARGET_DOMAIN"; then
                require_dom_shutoff "$TARGET_URI" "$TARGET_DOMAIN" "target"
        elif ! virsh_err_is_not_found; then
                warn "could not query target domain '$TARGET_DOMAIN' via $TARGET_URI: $VIRSH_ERR"
        fi
        [ -n "$TARGET_DISK_PATH" ] || warn "TARGET_DISK_PATH is empty -- target disk path resolution (used for tampering) is only reliable when the source has no active external snapshot. Recommended: always set TARGET_DISK_PATH in $CONF."

        preflight_bridge_helper
        log "preflight OK"
}

# bridge_helper_path -> the ONE vmsync-bridge-helper path this whole harness
# uses: the one it checks in the preflight, the one the checksum shims wrap, and
# the one it passes to every vmsync it launches as -bridge-helper-path.
#
# It is resolved in one place and passed EXPLICITLY rather than left to each
# side's default, because the two defaults do not agree: vmsync's flag defaults
# to /usr/local/bin/vmsync-bridge-helper (cmd/vmsync/main.go) and this harness
# falls back to /usr/bin/vmsync-bridge-helper. Left to those defaults with
# BRIDGE_HELPER_PATH unset, the preflight would version-check a binary vmsync
# never opens, and report "integrity check available" for a helper that was not
# the one in use -- the same shape of false green as testing a stale vmsync, and
# just as invisible. Passing it everywhere means the binary this harness
# verified is provably the binary the runs used.
bridge_helper_path() {
	printf '%s\n' "${BRIDGE_HELPER_PATH:-/usr/bin/vmsync-bridge-helper}"
}

# preflight_bridge_helper reports what vmsync-bridge-helper is on the target
# and whether its version matches the vmsync under test.
#
# Reported rather than fatal, because a missing helper is legitimate: it only
# blocks -compress/-netbuffer, and BENCH_SYNC_ARGS="" is the documented way to
# run without them.
#
# But it is worth saying out loud, because the consequence changed. The
# pre-commit integrity check is now on by default and needs that binary at an
# EXACTLY matching version -- so a stale helper does not fail anything, it
# silently turns the check off for every run in the report. A harness that
# quietly certifies an integrity check which never executed is worse than one
# that never claimed to test it, and stage 13a is the only other place that
# would notice.
preflight_bridge_helper() {
	local helper vmsync_version helper_version
	helper="$(bridge_helper_path)"

	if ! ssh_host_cmd "$TARGET_HOST" "test -x '$helper'" >/dev/null 2>&1; then
		warn "vmsync-bridge-helper is not present (or not executable) at $helper on $TARGET_HOST. -compress/-netbuffer cannot run, and the pre-commit integrity check will be SKIPPED on every sync -- it is on by default but needs that binary. Deploy it, set BRIDGE_HELPER_PATH in $CONF, or set BENCH_SYNC_ARGS=\"\" to stop asking for compression."
		return 0
	fi

	vmsync_version="$("$VMSYNC_BIN" -version 2>/dev/null | tr -d '[:space:]')"
	helper_version="$(ssh_host_cmd "$TARGET_HOST" "'$helper' -version" 2>/dev/null | tr -d '[:space:]')"

	if [ -z "$helper_version" ]; then
		warn "vmsync-bridge-helper at $helper on $TARGET_HOST exists but would not report a version, so the integrity check may be skipped on every sync. Check it runs there by hand."
	elif [ -n "$vmsync_version" ] && [ "$helper_version" != "$vmsync_version" ]; then
		warn "version skew: vmsync is $vmsync_version but vmsync-bridge-helper at $helper on $TARGET_HOST is $helper_version. vmsync refuses to use a mismatched helper, so the pre-commit integrity check will be SKIPPED on every sync in this report (and stage 13 cannot test it). Rebuild and redeploy the helper."
	else
		log "preflight: vmsync-bridge-helper $helper_version at $helper on $TARGET_HOST matches vmsync -- integrity check available"
	fi
	return 0
}

# --- vmsync invocation -----------------------------------------------------

# vmsync_common_args [IODEPTH_OVERRIDE] populates the global array
# VMSYNC_ARGS with the flags every invocation always needs -- including
# exactly ONE -io-depth: IODEPTH_OVERRIDE if given (Stage 1 passes its own
# per-combination value), otherwise the fixed default from $CONF. Every
# call site appends its own scenario-specific flags after this.
vmsync_common_args() {
        local iodepth="${1:-${IO_DEPTH:-8}}"
        VMSYNC_ARGS=(
                -source-uri "$SOURCE_URI"
                -target-uri "$TARGET_URI"
                -source-domain "$SOURCE_DOMAIN"
                -target-domain "$TARGET_DOMAIN"
                -io-depth "$iodepth"
        )
        [ -n "${TARGET_DISK_PATH:-}" ] && VMSYNC_ARGS+=(-target-disk-path "$TARGET_DISK_PATH")
        [ -n "${SSH_USER:-}" ] && VMSYNC_ARGS+=(-ssh-user "$SSH_USER")
        [ -n "${SSH_KEY:-}" ] && VMSYNC_ARGS+=(-ssh-key "$SSH_KEY")
        [ -n "${SSH_PORT:-}" ] && VMSYNC_ARGS+=(-ssh-port "$SSH_PORT")
        [ -n "${SSH_KNOWN_HOSTS:-}" ] && VMSYNC_ARGS+=(-ssh-known-hosts "$SSH_KNOWN_HOSTS")
        # Always, not only when BRIDGE_HELPER_PATH is set: unset, the two sides
        # default to different paths, so the preflight would check one helper and
        # the run would use another. See bridge_helper_path.
        VMSYNC_ARGS+=(-bridge-helper-path "$(bridge_helper_path)")
        [ -n "${REPLACED_DISK_ACTION:-}" ] && VMSYNC_ARGS+=(-replaced-disk-action "$REPLACED_DISK_ACTION")
        return 0
}

# run_vmsync SCENARIO PHASE EXTRA_ARGS... -> runs vmsync with the common
# args plus EXTRA_ARGS, times it end to end, captures stdout+stderr to a
# per-run log, and records one results.csv row. Sets RUN_RC, RUN_LOG,
# RUN_WALL_SECONDS, RUN_PROM for the caller to inspect (e.g. to grep the
# log for "starting full pull backup" vs "starting incremental pull
# backup", to decide whether a non-zero exit was actually the expected
# outcome as the verify+tamper tests do, or to read a specific metric back
# out of RUN_PROM via prom_sum as the external-snapshot test does).
#
# A non-zero vmsync exit is NOT treated as a harness error here -- several
# scenarios (verify+tamper) expect one. Callers decide what a given exit
# code means for their own scenario.
run_vmsync() {
        local scenario="$1" phase="$2"
        shift 2
        local prom_file="$RUN_DIR/prom/${scenario}.${phase}.prom"
        local log_file="$RUN_DIR/logs/${scenario}.${phase}.log"
        RUN_LOG="$log_file"
        RUN_PROM="$prom_file"

        # Pull a "-io-depth VALUE" pair out of EXTRA_ARGS, if present (Stage 1
        # passes its own per-combination value this way) -- vmsync_common_args
        # is given it as an override so exactly ONE -io-depth ever ends up on
        # the command line, instead of the fixed default AND the scenario's
        # own value both landing on it back to back (harmless to vmsync itself,
        # whose flag parser just lets the last one win, but it made every
        # printed command line and results.csv entry needlessly confusing).
        local -a extra=()
        local iodepth_override=""
        while [ $# -gt 0 ]; do
                if [ "$1" = "-io-depth" ] && [ $# -ge 2 ]; then
                        iodepth_override="$2"
                        shift 2
                else
                        extra+=("$1")
                        shift
                fi
        done

vmsync_common_args "$iodepth_override"
        local args=("${VMSYNC_ARGS[@]}" -prometheus-textfile "$prom_file" "${extra[@]}")
        log "-> $scenario/$phase"
        log "   $VMSYNC_BIN ${args[*]}"

        if [ "$DRY_RUN" = yes ]; then
                RUN_RC=0
                RUN_WALL_SECONDS=0
                results_row "$CSV" "$scenario" "$phase" DRYRUN 0 "" "" "" "" "dry run -- not executed"
                return 0
        fi

        local start end
        start="$(now_epoch)"
        set +e
        "$VMSYNC_BIN" "${args[@]}" >"$log_file" 2>&1
        RUN_RC=$?
        set -e
        end="$(now_epoch)"
        RUN_WALL_SECONDS="$(elapsed_seconds "$start" "$end")"


        if [ "$INTERRUPTED" = yes ]; then
                warn "interrupted (Ctrl+C/SIGTERM) during $scenario/$phase -- stopping the whole run now rather than queuing further combinations"
                results_row "$CSV" "$scenario" "$phase" "$RUN_RC" "$RUN_WALL_SECONDS" "" "" "" "" "INTERRUPTED (Ctrl+C/SIGTERM)"
                generate_report
                exit 130
        fi

        local transferred compressed disk_bytes mode
        transferred="$(prom_sum "$prom_file" vmsync_transferred_bytes)"
        compressed="$(prom_sum "$prom_file" vmsync_compressed_transferred_bytes)"
        disk_bytes="$(prom_sum "$prom_file" vmsync_disk_size_bytes)"
        mode="unknown"
        if grep -q 'starting full pull backup' "$log_file" 2>/dev/null; then
                mode="full"
        elif grep -q 'starting incremental pull backup' "$log_file" 2>/dev/null; then
                mode="incremental"
        fi

        log "   exit=$RUN_RC wall=${RUN_WALL_SECONDS}s mode=$mode transferred=${transferred}B"
        results_row "$CSV" "$scenario" "$phase" "$RUN_RC" "$RUN_WALL_SECONDS" "$transferred" "$compressed" "$disk_bytes" "$mode" ""
}

# BENCH_SYNC_EXTRA is the transport flags every stage EXCEPT the matrix adds
# to its syncs, split from BENCH_SYNC_ARGS in bench.conf. Default: -compress.
#
# Stage 1 is the one stage measuring transport, so it must choose its own and
# is deliberately left calling run_vmsync directly. Every other stage copies
# a disk only so there is something real to tamper with, reinit, snapshot,
# promote or fence -- the transport is incidental, and the fastest way across
# the wire is simply the least waiting. On a link that is the bottleneck (a
# saturated 1GbE reads as roughly 110 MB/s in results.csv) that is most of
# their runtime, and Stage 6 alone does three full copies.
#
# Nothing about this changes what those stages test. vmsync's own port
# allocator handles "whichever combination of -compress/-netbuffer/-verify is
# active" by design, reserving 4N target ports when both are on, so the
# verify modes compose with the bridge rather than working around it. Stage 3
# already passed -compress unconditionally before this existed.
#
# Set BENCH_SYNC_ARGS="" to turn it off. -compress needs vmsync-bridge-helper
# on the TARGET host; when it is missing every one of these stages fails at
# its baseline, which is why those failures name this setting.
read -ra BENCH_SYNC_EXTRA <<<"${BENCH_SYNC_ARGS--compress}"

# bench_sync SCENARIO PHASE ARGS... -- run_vmsync plus those shared flags.
#
# A wrapper rather than appending at each call site: the empty case has to be
# handled explicitly, because "${arr[@]}" on an empty array is an unbound
# variable under `set -u` on bash before 4.4.
bench_sync() {
	local scenario="$1" phase="$2"
	shift 2
	if [ ${#BENCH_SYNC_EXTRA[@]} -gt 0 ]; then
		run_vmsync "$scenario" "$phase" "$@" "${BENCH_SYNC_EXTRA[@]}"
	else
		run_vmsync "$scenario" "$phase" "$@"
	fi
}

# bench_sync_hint names the likely cause when one of those syncs fails, since
# the flags it adds are also the one extra thing that has to be installed on
# the target.
# bench_sync_hint -- what to suspect when a stage's baseline sync fails.
#
# Two causes, and the order matters. A run that vmsync REFUSED at preflight
# ends in well under a second having copied nothing, and is not a transport
# problem at all: a recorded verify_state (stage 14's feature -- a plain
# -reinit is refused too, by design), a replication_role, or an out-of-band
# write on the replica will each do it. Blaming BENCH_SYNC_ARGS for those
# sends the reader after -compress and pkg/nbdsync for a vmsync that is
# working exactly as designed, so the refusal is named first and by its
# signature.
bench_sync_hint() {
	printf ' -- if that run ended in under a second having transferred nothing, vmsync REFUSED it at preflight rather than failing: check its log for a recorded verify_state (cleared only by -verify-failure-reinit or -force-clean), a replication_role, or an out-of-band-write refusal'
	if [ ${#BENCH_SYNC_EXTRA[@]} -gt 0 ]; then
		printf '. Otherwise, this stage adds "%s" to every sync (BENCH_SYNC_ARGS in %s); -compress needs vmsync-bridge-helper on %s, so set BENCH_SYNC_ARGS="" if that is not installed there' \
			"${BENCH_SYNC_EXTRA[*]}" "$CONF" "$TARGET_HOST"
	fi
}

# load_axes parses scenarios.conf's [compress]/[netbuffer]/[use_ssh]/
# [iodepth] sections into the matching *_OPTS global arrays -- one value
# per line within each section, blank lines and whole-line "#" comments
# ignored. stage_matrix cross-multiplies these into the full Stage 1
# matrix; see scenarios.conf's own header comment for the exact format.
load_axes() {
        COMPRESS_OPTS=()
        NETBUFFER_OPTS=()
        USE_SSH_OPTS=()
        IODEPTH_OPTS=()
        local section="" line
        while read -r line || [ -n "$line" ]; do
                [[ -z "$line" || "$line" == \#* ]] && continue
                if [[ "$line" =~ ^\[([a-z_]+)\]$ ]]; then
                        section="${BASH_REMATCH[1]}"
                        continue
                fi
                case "$section" in
                compress) COMPRESS_OPTS+=("$line") ;;
                netbuffer) NETBUFFER_OPTS+=("$line") ;;
                use_ssh) USE_SSH_OPTS+=("$line") ;;
                iodepth) IODEPTH_OPTS+=("$line") ;;
                "") die "$SCENARIOS: value '$line' appears before any [section] header" ;;
                *) die "$SCENARIOS: unknown section [$section]" ;;
                esac
        done <"$SCENARIOS"
        [ ${#COMPRESS_OPTS[@]} -gt 0 ] || die "$SCENARIOS: [compress] section is empty"
        [ ${#NETBUFFER_OPTS[@]} -gt 0 ] || die "$SCENARIOS: [netbuffer] section is empty"
        [ ${#USE_SSH_OPTS[@]} -gt 0 ] || die "$SCENARIOS: [use_ssh] section is empty"
        [ ${#IODEPTH_OPTS[@]} -gt 0 ] || die "$SCENARIOS: [iodepth] section is empty"
        return 0
}

# is_noop_combo COMPRESS NETBUFFER USE_SSH -> true when this combination is
# a wasted duplicate of the plain "no bridge" case: -use-ssh is a
# documented no-op unless -compress or -netbuffer is also active, so
# compress=off + netbuffer=off + use_ssh=yes would just needlessly re-run
# the exact same sync bench.sh already runs with use_ssh=no.
is_noop_combo() {
        [ "$1" = off ] && [ "$2" = off ] && [ "$3" = yes ]
}

# build_scenario_name COMPRESS NETBUFFER USE_SSH IODEPTH -> sets the global
# SCEN_NAME to a filesystem/log-safe name identifying this combination.
# Populates a global instead of the more usual "print and capture via
# $(...)" -- that command substitution forks a subshell on every single
# call regardless of what's inside it, and on a platform where process
# creation is heavy that dominates runtime once this runs hundreds of
# times (one call per Stage 1 combination) -- same reasoning as
# build_scenario_args's own SCEN_ARGS global just below.
build_scenario_name() {
        local compress="$1" netbuf="$2" use_ssh="$3" iodepth="$4"
        local c_part nb_part
        if [ "$compress" = off ]; then
                c_part="c-off"
        else
                c_part="c-${compress/:/-}" # "s2:better" -> "s2-better"
        fi
        if [ "$netbuf" = off ]; then
                nb_part="nb-off"
        else
                nb_part="nb-${netbuf//,/-}" # "128k,1G" -> "128k-1G"
        fi
        SCEN_NAME="${c_part}_${nb_part}_ssh-${use_ssh}_iod-${iodepth}"
        return 0
}

# build_scenario_args COMPRESS NETBUFFER USE_SSH IODEPTH -> populates the
# global array SCEN_ARGS with the vmsync flags for this combination.
build_scenario_args() {
        local compress="$1" netbuf="$2" use_ssh="$3" iodepth="$4"
        # -no-checksum on EVERY cell of the transport matrix, deliberately.
        #
        # The pre-commit integrity check is on by default whenever a matching
        # vmsync-bridge-helper is on the target -- which, on any host this
        # matrix can run on, is always, since the compressed cells require
        # that same binary. Left enabled it would add a qemu-nbd start, a
        # local read of every written byte on the target, a hash and a stop to
        # each of this stage's 1170 invocations. That does two unwanted
        # things: it inflates a -reinit cell by roughly a disk-sized target
        # read, and it mixes the cost of an integrity check into numbers whose
        # entire purpose is comparing TRANSPORT settings against each other
        # and against previous runs of this same matrix.
        #
        # The check gets its own stage (--stages checksum) where its cost and
        # its behaviour are what is being measured, rather than noise on top
        # of something else.
        SCEN_ARGS=(-io-depth "$iodepth" -no-checksum)
        if [ "$compress" != off ]; then
                local algo="${compress%%:*}" level="${compress#*:}"
                SCEN_ARGS+=("-compress=$algo" "-compress-level=$level")
        fi
        [ "$netbuf" != off ] && SCEN_ARGS+=("-netbuffer=$netbuf")
        [ "$use_ssh" = yes ] && SCEN_ARGS+=(-use-ssh)
        return 0
}

# --- Stage 1: transport matrix ----------------------------------------------

stage_matrix() {
        load_axes

        # Counted in a first, throwaway pass purely so the log/progress
        # messages below can say "3 of 214" instead of leaving you guessing how
        # much of a genuinely large matrix is left.
        local compress netbuf use_ssh iodepth total=0 skipped=0
        for compress in "${COMPRESS_OPTS[@]}"; do
                for netbuf in "${NETBUFFER_OPTS[@]}"; do
                        for use_ssh in "${USE_SSH_OPTS[@]}"; do
                                for iodepth in "${IODEPTH_OPTS[@]}"; do
                                        if is_noop_combo "$compress" "$netbuf" "$use_ssh"; then
                                                skipped=$((skipped + 1))
                                        else
                                                total=$((total + 1))
                                        fi
                                done
                        done
                done
        done

        log "=== Stage 1: transport matrix (full + incremental sync, timed) ==="
        log "$total combinations queued ($((total * 2)) vmsync invocations total), $skipped skipped as redundant (compress=off + netbuffer=off + use-ssh=yes is a no-op)"

        local n=0 name
        for compress in "${COMPRESS_OPTS[@]}"; do
                for netbuf in "${NETBUFFER_OPTS[@]}"; do
                        for use_ssh in "${USE_SSH_OPTS[@]}"; do
                                for iodepth in "${IODEPTH_OPTS[@]}"; do
                                        is_noop_combo "$compress" "$netbuf" "$use_ssh" && continue

                                        build_scenario_name "$compress" "$netbuf" "$use_ssh" "$iodepth"
                                        name="$SCEN_NAME"
                                        if [ -n "$ONLY_PATTERN" ]; then
                                                # shellcheck disable=SC2053 -- intentional glob match, not literal.
                                                [[ "$name" == $ONLY_PATTERN ]] || continue
                                        fi
                                        n=$((n + 1))

                                        build_scenario_args "$compress" "$netbuf" "$use_ssh" "$iodepth"
                                        log "--- [$n/$total] scenario: $name (compress=$compress netbuffer=$netbuf use_ssh=$use_ssh iodepth=$iodepth) ---"

                                        if [ "$DRY_RUN" != yes ]; then
                                                # Still fatal here, unlike every other stage. This
                                                # runs first and preflight checked the same thing
                                                # moments ago, so reaching it means the target
                                                # changed state mid-run -- and almost nothing has
                                                # been recorded yet, so there is no report worth
                                                # preserving by continuing.
                                                require_dom_shutoff_or_absent "$TARGET_URI" "$TARGET_DOMAIN" "target"
                                        fi

                                        # -reinit establishes a clean, fully-timed full-sync
                                        # baseline for this combination (deletes any prior
                                        # target replica first -- see DefineDomain/reinit's own
                                        # undefine-then-resync behavior in cmd/vmsync/main.go).
                                        run_vmsync "$name" full -reinit "${SCEN_ARGS[@]}"
                                        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                                                warn "full sync failed for scenario '$name' (see $RUN_LOG) -- skipping its incremental step"
                                                continue
                                        fi

                                        # Immediately follow with a plain incremental sync
                                        # under the SAME settings. This deliberately uses
                                        # whatever real drift has accumulated on the source
                                        # since the full sync above rather than fabricating
                                        # synthetic writes -- see README.md's "What this does
                                        # and does not control" section.
                                        run_vmsync "$name" incremental "${SCEN_ARGS[@]}"
                                done
                        done
                done
        done
        return 0
}

# --- tampering ---------------------------------------------------------------

# TAMPER_SEQ counts draws, so each tamper in a run gets a different one while
# the whole SEQUENCE stays a pure function of TAMPER_SEED.
TAMPER_SEQ=0
TAMPER_OFF=""
TAMPER_LEN=""

# rng_below N -> a deterministic integer in [0, N).
#
# Not $RANDOM and not awk's srand(): neither is reproducible ACROSS hosts or
# implementations, and a randomly-placed corruption that cannot be replayed is
# strictly worse than a fixed one -- a FAIL you cannot re-run is a FAIL you
# cannot investigate. Hashing "seed:counter" is deterministic everywhere
# md5sum exists, which is everywhere this harness already runs. 15 hex digits
# is 60 bits, comfortably inside bash's signed 64-bit arithmetic.
rng_below() {
	local n="$1" hex
	hex="$(printf '%s:%s' "$TAMPER_SEED" "$TAMPER_SEQ" | md5sum | cut -c1-15)"
	printf '%s' "$(((16#$hex) % n))"
}

# target_virtual_size PATH -> the target disk's virtual size in bytes.
#
# Read off the image rather than from a previous run's Prometheus textfile:
# vmsync_disk_size_bytes has one series per disk and prom_sum adds them up, so
# on a multi-disk domain that number is not any single disk's size.
target_virtual_size() {
	local path="$1"
	ssh_host_cmd "$TARGET_HOST" qemu-img info --output=json "'$path'" 2>/dev/null \
		| awk -F'[:,]' '/"virtual-size"/ { gsub(/[^0-9]/, "", $2); print $2; exit }'
}

# draw_tamper VIRTUAL_SIZE -- chooses TAMPER_OFF/TAMPER_LEN. Returns non-zero
# when the configured band cannot hold even the smallest tamper, so the caller
# can SKIP rather than write somewhere it did not intend to.
draw_tamper() {
	local vsize="$1" start end span nlen nslot

	if [ "$TAMPER_MODE" = fixed ]; then
		TAMPER_OFF="$TAMPER_OFFSET"
		TAMPER_LEN="$TAMPER_LENGTH"
		[ $((TAMPER_OFF + TAMPER_LEN)) -le "$vsize" ] || return 1
		return 0
	fi

	TAMPER_SEQ=$((TAMPER_SEQ + 1))

	end="$TAMPER_BAND_END"
	if [ "$end" -eq 0 ] || [ "$end" -gt "$vsize" ]; then end="$vsize"; fi
	start=$(((TAMPER_BAND_START + TAMPER_ALIGN - 1) / TAMPER_ALIGN * TAMPER_ALIGN))
	span=$((end - start))
	[ "$span" -ge "$((TAMPER_LENGTH_MIN + TAMPER_ALIGN))" ] || return 1

	# Length FIRST, then a slot that fits it. Drawing the offset first and
	# clamping the length is how a draw near the end of the band collapses to
	# length 0 -- a qemu-io write of nothing, which succeeds, changes nothing,
	# and is then scored as "verify failed to detect it".
	nlen=$(((TAMPER_LENGTH_MAX - TAMPER_LENGTH_MIN) / TAMPER_ALIGN + 1))
	TAMPER_LEN=$((TAMPER_LENGTH_MIN + $(rng_below "$nlen") * TAMPER_ALIGN))
	TAMPER_SEQ=$((TAMPER_SEQ + 1))
	nslot=$(((span - TAMPER_LEN) / TAMPER_ALIGN + 1))
	[ "$nslot" -ge 1 ] || return 1
	TAMPER_OFF=$((start + $(rng_below "$nslot") * TAMPER_ALIGN))

	[ $((TAMPER_OFF + TAMPER_LEN)) -le "$vsize" ] || return 1
	return 0
}

# tamper_target PATH PRESERVE_MTIME -- corrupts the target replica in place.
#
# PRESERVE_MTIME decides which of two DIFFERENT protections is under test, and
# it is not a way of getting around either.
#
# vmsync refuses an incremental sync when the target file's mtime is newer than
# the last recorded sync (cmd/vmsync/main.go, "Target file on system is newer"),
# and that check fires long before any compare -- before CreateCheckpoint, let
# alone -verify. It catches one specific thing: somebody wrote to the replica
# THROUGH THE FILESYSTEM since the last sync.
#
# -verify exists for the threat that check structurally cannot see: contents
# that diverged with nothing visible at the filesystem layer -- a bad sector, a
# silent write error, a scrub miscompare, a bug in vmsync's own copy path. Bit
# rot does not touch mtime. So a tamper that leaves mtime alone tests the mtime
# guard, and ONLY a tamper that restores it can reach, and therefore test, the
# compare. Both are worth testing, which is why this harness does both,
# separately and under their own names.
tamper_target() {
	local path="$1" preserve="$2" mtime=""

	if [ "$preserve" = yes ]; then
		mtime="$(ssh_host_cmd "$TARGET_HOST" stat -c %Y "'$path'")" \
			|| { warn "could not read the mtime of $path on $TARGET_HOST before tampering -- refusing to corrupt a file whose state cannot be restored"; return 1; }
	fi

	ssh_host_cmd "$TARGET_HOST" qemu-io -f qcow2 \
		-c "'write -P ${TAMPER_PATTERN} ${TAMPER_OFF} ${TAMPER_LEN}'" "'${path}'" \
		|| { warn "failed to inject test corruption into $path on $TARGET_HOST -- refusing to continue this sub-test"; return 1; }

	# Read it back. A qemu-io write that reports success but lands somewhere
	# else, or gets swallowed, would otherwise turn into "verify missed it".
	ssh_host_cmd "$TARGET_HOST" qemu-io -r -f qcow2 \
		-c "'read -P ${TAMPER_PATTERN} ${TAMPER_OFF} ${TAMPER_LEN}'" "'${path}'" >/dev/null \
		|| { warn "the test corruption did not read back from $path at offset $TAMPER_OFF length $TAMPER_LEN -- the tamper did not take, so nothing below would be testing what it claims"; return 1; }

	if [ "$preserve" = yes ]; then
		# touch -d @N sets nanoseconds to zero, so the restored stamp is at
		# worst marginally OLDER than the original -- never newer, and the
		# original passed the guard on the previous run by construction.
		ssh_host_cmd "$TARGET_HOST" touch -d "@$mtime" "'$path'" \
			|| { warn "could not restore the mtime of $path on $TARGET_HOST -- the sync below would fail at vmsync's mtime guard instead of reaching -verify, and would be scored as if it had verified"; return 1; }
	fi
}

# verify_outcome PROMFILE -> RAN_MISMATCH | RAN_CLEAN | NOT_RUN
#
# The reason this exists rather than reading vmsync's exit code: a -verify run
# that dies BEFORE its compare also exits non-zero, and scoring that as
# "mismatch detected" is how this stage reported PASS for years while never
# once exercising -verify. vmsync emits vmsync_verification_state only for a
# run that actually reached the compare, so its presence answers "did this test
# test anything" and its value answers "what did it find".
verify_outcome() {
	local prom="$1"
	if ! prom_has "$prom" vmsync_verification_state; then
		printf 'NOT_RUN'
		return 0
	fi
	if [ "$(prom_first "$prom" vmsync_verification_state)" = 0 ]; then
		printf 'RAN_CLEAN'
	else
		printf 'RAN_MISMATCH'
	fi
}

# --- Stage 2: verify modes + target-side tampering -------------------------

stage_verify_tamper() {
        log "=== Stage 2: -verify modes with deliberate target-side tampering ==="

        if [ "$DRY_RUN" != yes ]; then
                stage_needs_target_shutoff "$CSV" verify-precondition "stage verify" || return 0
        fi

        # A known-clean baseline under the plain, no-bridge transport -- verify
        # correctness shouldn't depend on which transport carried the
        # preceding sync, and holding it fixed makes the three modes below
        # directly comparable to each other.
        bench_sync verify-baseline full -reinit
        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                warn "baseline full sync for verify testing failed (see $RUN_LOG) -- aborting stage 2$(bench_sync_hint)"
                return 1
        fi

        local target_path=""
        if [ "$DRY_RUN" != yes ]; then
                # "|| true": disk_source_path's own failure (e.g. virsh dumpxml
                # erroring under `set -o pipefail`) must fall through to the
                # specific, actionable message below instead of silently killing the
                # script here with no message at all.
                target_path="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
                [ -n "$target_path" ] || { warn "could not resolve target disk path for dev='$TAMPER_DISK_DEV' via virsh dumpxml -- check TAMPER_DISK_DEV in $CONF"; return 1; }
                log "target disk under test: dev=$TAMPER_DISK_DEV path=$target_path (on $TARGET_HOST)"
        fi

        local vsize=0
        if [ "$DRY_RUN" != yes ]; then
                vsize="$(target_virtual_size "$target_path")" || true
                [ -n "$vsize" ] && [ "$vsize" -gt 0 ] 2>/dev/null \
                        || { warn "could not read the virtual size of $target_path on $TARGET_HOST via qemu-img info -- needed to keep a tamper inside the disk"; return 1; }
                log "target disk virtual size: $vsize bytes"
        fi

        # Sub-test A: the mtime guard. Tamper and leave the mtime alone, so
        # vmsync sees a replica that was written to through the filesystem since
        # the last sync and must refuse the incremental sync outright.
        #
        # This is what stage 2 has in fact been testing all along, unlabelled:
        # the guard fires before the compare, and its non-zero exit was being
        # scored as "-verify detected a mismatch". Naming it makes that coverage
        # real instead of accidental, and leaves the verify sub-tests below free
        # to test what they claim to.
        verify_guard_subtest "$target_path" "$vsize"

        local mode
        for mode in fast full qemu-img; do
                verify_mode_subtest "$target_path" "$vsize" "$mode" "verify-${mode}" tamper
        done

        # The loop above reaches the DIGEST comparator for fast and full,
        # because bench_sync passes -compress and a matching helper on the
        # target turns the digest exchange on. That leaves
        # CompareTCP/CompareTCPCollect -- the byte comparators, which are
        # still exactly what -no-checksum selects and were the only
        # implementation until the digest path existed -- covered by nothing.
        #
        # Two more tampers rather than six: qemu-img shells out to a separate
        # implementation and is unaffected by checksumming either way, so
        # re-running it would cost a tamper cycle to test the same code twice.
        for mode in fast full; do
                verify_mode_subtest "$target_path" "$vsize" "$mode" "verify-${mode}-bytes" tamper -no-checksum
        done

        # Then the two cross-checks. The loops above test each mode on its own
        # tamper at its own random offset, which is broad coverage across runs;
        # these two ask a different question -- whether the modes AGREE, with
        # and without corruption present.
        verify_cross_check_subtest "$target_path" "$vsize"
        verify_clean_oracle_subtest "$target_path"
        return 0
}

# verify_cross_check_subtest PATH VSIZE -- ask all three modes about the SAME
# corruption and require them to agree.
#
# The per-mode loop above already proves each mode detects corruption. It
# cannot prove they agree, because each one gets its own tamper at its own
# random offset -- three separate questions, never the same one twice. This
# asks the question that the skip logic actually turns on:
#
#   fast and full read through nbdsync's own comparator, which SKIPS ranges
#   both sides report as NBD_STATE_ZERO (planCompareChunks). qemu-img does
#   not -- it reads every byte, in a separate implementation vmsync did not
#   write. So qemu-img is the oracle for the skip logic, and it is only an
#   oracle if all three are asked about identical bytes.
#
# A disagreement here is the signal worth having. fast/full clean while
# qemu-img reports a mismatch means the skip ate a real difference -- a
# verify that passes over corruption, which is worse than no verify at all.
#
# It costs two extra full resyncs, which it did not before stage 14's feature
# landed. A -verify that finds a difference now records it on the target
# domain, and that record refuses the next sync -- so the three modes can no
# longer share one tamper. Each is given the same corruption on a healed
# replica instead. See the loop for why re-establishing beats the alternatives:
# there is no way to clear the record without a full recopy, by design.
verify_cross_check_subtest() {
        local path="$1" vsize="$2" mode outcome
        local -a agreed=()

        log "--- verify cross-check: the same corruption, all three modes, expecting all three to agree ---"

        if [ "$DRY_RUN" != yes ]; then
                stage_needs_target_shutoff "$CSV" verify-cross "verify-cross" || return 0
                if ! draw_tamper "$vsize"; then
                        warn "SKIP verify-cross: the configured tamper band does not fit inside a ${vsize}-byte disk"
                        results_row "$CSV" verify-cross tamper "" "" "" "" "" "" "SKIP tamper band does not fit the disk"
                        return 0
                fi
                log "   corrupting at offset $TAMPER_OFF length $TAMPER_LEN (seed $TAMPER_SEED)"
                if ! tamper_target "$path" yes; then
                        results_row "$CSV" verify-cross tamper "" "" "" "" "" "" "SKIP the tamper could not be applied"
                        heal_target verify-cross heal "$path"
                        return 0
                fi
        fi

        # Every mode must see the SAME bytes, and stage 14's feature makes
        # that cost a heal-and-re-tamper between them rather than nothing at
        # all.
        #
        # Tampering once and running all three modes against it is not enough,
        # even though the tamper itself survives: the FINDING survives too. A
        # -verify that ran and found a difference records verify_state=failed
        # on the target domain, and a domain carrying that record refuses the
        # next sync outright. Modes two and three would be refused at
        # preflight in ~0.3s with mode=unknown, never reaching a compare, and
        # the agreement check below would read that as the skip logic dropping
        # real differences -- pointing at pkg/nbdsync for vmsync working as
        # designed.
        #
        # Re-establishing the state is the honest fix: heal (a -force-clean
        # full resync, which is also what clears the record) and re-apply the
        # tamper at the SAME offset, since draw_tamper ran once above and
        # TAMPER_OFF/TAMPER_LEN have not moved since. Each mode then gets a
        # byte-identical replica and a clean record, which is what this
        # cross-check is meant to compare.
        local tampered_once=no
        for mode in fast full qemu-img; do
                if [ "$DRY_RUN" != yes ] && [ "$tampered_once" = yes ]; then
                        heal_target verify-cross "${mode}-heal" "$path"
                        log "   re-corrupting at offset $TAMPER_OFF length $TAMPER_LEN (the same bytes the previous mode saw)"
                        if ! tamper_target "$path" yes; then
                                warn "SKIP verify-cross/$mode: the tamper could not be re-applied after healing, so this mode would compare a CLEAN replica and be scored as having missed a corruption that was never there"
                                results_row "$CSV" verify-cross "${mode}-result" "" "" "" "" "" "" "SKIP tamper could not be re-applied"
                                continue
                        fi
                fi
                tampered_once=yes
                bench_sync verify-cross "$mode" "-verify=$mode"
                if [ "$DRY_RUN" = yes ]; then
                        results_row "$CSV" verify-cross "${mode}-result" DRYRUN "" "" "" "" "" "SKIP dry run"
                        continue
                fi
                outcome="$(verify_outcome "$RUN_PROM")"
                agreed+=("$mode=$outcome")
                case "$outcome" in
                RAN_MISMATCH)
                        log "   PASS: -verify=$mode reported a mismatch"
                        results_row "$CSV" verify-cross "${mode}-result" 0 "" "" "" "" "" "PASS mismatch detected"
                        ;;
                RAN_CLEAN)
                        warn "FAIL: -verify=$mode found NOTHING at offset $TAMPER_OFF length $TAMPER_LEN (reproduce with TAMPER_SEED=$TAMPER_SEED). See $RUN_LOG"
                        results_row "$CSV" verify-cross "${mode}-result" 1 "" "" "" "" "" "FAIL mismatch NOT detected"
                        ;;
                *)
                        warn "FAIL: -verify=$mode never reached its compare, so nothing was verified (exit=$RUN_RC). See $RUN_LOG"
                        results_row "$CSV" verify-cross "${mode}-result" 1 "" "" "" "" "" "FAIL verification never ran"
                        ;;
                esac
        done

        if [ "$DRY_RUN" != yes ]; then
                # The cross-check itself, stated as its own assertion so the
                # report says "the modes disagreed" rather than leaving somebody
                # to notice it across three rows.
                local first="" disagree=0 entry
                for entry in "${agreed[@]}"; do
                        [ -z "$first" ] && first="${entry#*=}"
                        [ "${entry#*=}" = "$first" ] || disagree=1
                done
                if [ "$disagree" = 0 ] && [ -n "$first" ]; then
                        log "   PASS: all three modes agree (${agreed[*]})"
                        results_row "$CSV" verify-cross agreement 0 "" "" "" "" "" "PASS all modes agree: ${agreed[*]}"
                elif printf '%s\n' "${agreed[@]}" | grep -q '=NOT_RUN$'; then
                        # A mode that never compared has no opinion, so this is
                        # not a disagreement about bytes and must not be
                        # reported as one. Said separately because the old
                        # message blamed the skip logic for it, which cost real
                        # time chasing pkg/nbdsync over a run that was refused
                        # before it read anything.
                        warn "FAIL: the cross-check could not be performed -- at least one mode never reached its compare (${agreed[*]}), so the modes were never asked the same question. This says nothing about the skip logic. Check those runs' logs for why they ended early: a refusal at preflight (a recorded verify_state, a role, an out-of-band write) ends a run in well under a second with mode=unknown. Reproduce with TAMPER_SEED=$TAMPER_SEED"
                        results_row "$CSV" verify-cross agreement 1 "" "" "" "" "" "FAIL cross-check not performed: ${agreed[*]}"
                else
                        warn "FAIL: the verify modes DISAGREED about the same corrupted replica (${agreed[*]}). All three ran, so this is a real difference of opinion about identical bytes. qemu-img reads every byte in a separate implementation; fast and full skip ranges both sides report as zero. If qemu-img saw the corruption and they did not, that skip logic (planCompareChunks in pkg/nbdsync) is dropping real differences. Reproduce with TAMPER_SEED=$TAMPER_SEED"
                        results_row "$CSV" verify-cross agreement 1 "" "" "" "" "" "FAIL modes disagree: ${agreed[*]}"
                fi
        fi

        heal_target verify-cross heal "$path"
}

# verify_clean_oracle_subtest PATH -- after the heal, confirm an INDEPENDENT
# full-image comparison also calls the replica clean.
#
# The cross-check above proves the modes agree when there IS corruption. This
# proves they agree when there is not: a freshly reinit'd replica compared
# byte-for-byte by qemu-img, which reads everything and shares no code with
# nbdsync's comparator.
#
# What it catches that nothing else does: a copy path that produces a subtly
# wrong replica in a region vmsync's own verify happens to skip. Both halves
# of vmsync could then agree the replica is fine while it is not, and only an
# outside reader would notice.
verify_clean_oracle_subtest() {
        local path="$1" outcome

        log "--- verify clean-oracle: independent qemu-img comparison of the healed replica ---"

        bench_sync verify-oracle clean "-verify=qemu-img"

        if [ "$DRY_RUN" = yes ]; then
                results_row "$CSV" verify-oracle clean-result DRYRUN "" "" "" "" "" "SKIP dry run"
                return 0
        fi

        outcome="$(verify_outcome "$RUN_PROM")"
        case "$outcome" in
        RAN_CLEAN)
                log "   PASS: an independent full-image comparison agrees the replica is intact"
                results_row "$CSV" verify-oracle clean-result 0 "" "" "" "" "" "PASS independent compare clean"
                ;;
        RAN_MISMATCH)
                warn "FAIL: qemu-img found differences in a replica that was just fully resynced and that vmsync's own verify passed. Either the copy path is producing a wrong replica, or nbdsync's comparator is missing it. Inspect $path by hand. See $RUN_LOG"
                results_row "$CSV" verify-oracle clean-result 1 "" "" "" "" "" "FAIL independent compare found differences"
                ;;
        *)
                warn "FAIL: the independent comparison never reached its compare (exit=$RUN_RC), so the replica is unconfirmed. See $RUN_LOG"
                results_row "$CSV" verify-oracle clean-result 1 "" "" "" "" "" "FAIL oracle verification never ran"
                ;;
        esac
}

# verify_guard_subtest PATH VSIZE -- asserts the mtime guard refuses a target
# that was written to behind vmsync's back.
verify_guard_subtest() {
        local path="$1" vsize="$2"

        log "--- verify-guard: tampering WITHOUT restoring mtime, expecting vmsync to refuse the sync ---"

        if [ "$DRY_RUN" != yes ]; then
                stage_needs_target_shutoff "$CSV" verify-guard "verify-guard" || return 0
                if ! draw_tamper "$vsize"; then
                        warn "SKIP verify-guard: the configured tamper band does not fit inside a ${vsize}-byte disk -- lower TAMPER_BAND_START or TAMPER_LENGTH_MIN in $CONF"
                        results_row "$CSV" verify-guard tamper-result "" "" "" "" "" "" "SKIP tamper band does not fit the disk"
                        return 0
                fi
                log "   corrupting at offset $TAMPER_OFF length $TAMPER_LEN (seed $TAMPER_SEED)"
                # Healed on the way out even though the tamper failed: it can
                # fail AFTER the corruption landed (the mtime restore is the
                # last step), and standing down without healing would leave the
                # next sub-test measuring this one's damage.
                if ! tamper_target "$path" no; then
                        results_row "$CSV" verify-guard tamper-result "" "" "" "" "" "" "SKIP the tamper could not be applied"
                        heal_target verify-guard tamper-heal "$path"
                        return 0
                fi
        fi

        bench_sync verify-guard tamper

        if [ "$DRY_RUN" = yes ]; then
                results_row "$CSV" verify-guard tamper-result DRYRUN "" "" "" "" "" "SKIP dry run"
        elif [ "$RUN_RC" = 0 ]; then
                warn "FAIL: vmsync accepted an incremental sync into a target whose disk had been modified since the last sync -- the mtime guard did not fire. See $RUN_LOG"
                results_row "$CSV" verify-guard tamper-result 1 "" "" "" "" "" "FAIL mtime guard did not fire"
        # Matched on vmsync's own wording, so it breaks when that wording
        # changes -- which it did: adding -timestamp-tolerance-sec rewrote this
        # error and left the old phrase matching nothing, so a guard that fired
        # exactly as intended was reported as "something else went wrong
        # first". The phrase below is the semantic core rather than the whole
        # sentence, to survive the next rewording of the surrounding prose.
        elif grep -q "newer than the last sync timestamp" "$RUN_LOG" 2>/dev/null; then
                log "   PASS: the mtime guard refused the sync"
                results_row "$CSV" verify-guard tamper-result 0 "" "" "" "" "" "PASS mtime guard refused the sync"
        else
                warn "FAIL: the sync failed, but not at the mtime guard -- something else went wrong first: $(log_reason "$RUN_LOG")"
                results_row "$CSV" verify-guard tamper-result 1 "" "" "" "" "" "FAIL failed for some other reason"
        fi

        heal_target verify-guard tamper-heal "$path"
}

# verify_mode_subtest PATH VSIZE MODE SCENARIO PHASE [EXTRA_ARGS...] --
# asserts -verify=MODE detects a corruption the mtime guard cannot see.
#
# SCENARIO and PHASE are passed rather than derived because two stages call
# this: stage 2 once per mode, and stage 8 several times per mode. SCENARIO is
# what stage_pattern matches on, and PHASE has to be unique within a run or the
# per-run log and Prometheus files (named ${SCENARIO}.${PHASE}) overwrite each
# other and the failing attempt's evidence is gone.
#
# EXTRA_ARGS exists for exactly one caller: stage 2 passes -no-checksum to
# reach the BYTE comparator. -verify=fast/full now have two implementations
# behind them -- nbdsync's digest exchange when a matching helper is present
# (the default), and CompareTCP/CompareTCPCollect when it is not -- and
# whichever one bench does not ask for is exercised by nothing at all. The
# assertion is identical for both, so the same subtest serves.
verify_mode_subtest() {
        local path="$1" vsize="$2" mode="$3" scenario="$4" phase="$5" outcome
        shift 5
        local -a extra=("$@")

        log "--- verify=$mode: tampering ${path:-<unresolved in --dry-run>} with the mtime preserved ---"

        if [ "$DRY_RUN" != yes ]; then
                stage_needs_target_shutoff "$CSV" "$scenario" "$scenario/$phase" || return 0
                if ! draw_tamper "$vsize"; then
                        warn "SKIP $scenario/$phase: the configured tamper band does not fit inside a ${vsize}-byte disk"
                        results_row "$CSV" "$scenario" "${phase}-result" "" "" "" "" "" "" "SKIP tamper band does not fit the disk"
                        return 0
                fi
                log "   corrupting at offset $TAMPER_OFF length $TAMPER_LEN (seed $TAMPER_SEED)"
                if ! tamper_target "$path" yes; then
                        results_row "$CSV" "$scenario" "${phase}-result" "" "" "" "" "" "" "SKIP the tamper could not be applied"
                        heal_target "$scenario" "${phase}-heal" "$path"
                        return 0
                fi
        fi

        if [ ${#extra[@]} -gt 0 ]; then
                bench_sync "$scenario" "$phase" "-verify=$mode" "${extra[@]}"
        else
                bench_sync "$scenario" "$phase" "-verify=$mode"
        fi

        if [ "$DRY_RUN" = yes ]; then
                results_row "$CSV" "$scenario" "${phase}-result" DRYRUN "" "" "" "" "" "SKIP dry run"
        else
                outcome="$(verify_outcome "$RUN_PROM")"
                case "$outcome" in
                RAN_MISMATCH)
                        log "   PASS: -verify=$mode ran and reported a mismatch"
                        results_row "$CSV" "$scenario" "${phase}-result" 0 "" "" "" "" "" "PASS mismatch detected"
                        ;;
                RAN_CLEAN)
                        warn "FAIL: -verify=$mode ran and found NOTHING after the target was corrupted at offset $TAMPER_OFF length $TAMPER_LEN (reproduce with TAMPER_SEED=$TAMPER_SEED). See $RUN_LOG. This is now unambiguous: no mode discards a mismatch any more -- every mode compares the target against the same frozen source snapshot the copy read from, so a difference cannot be attributed to guest activity. Treat it as a real miss."
                        results_row "$CSV" "$scenario" "${phase}-result" 1 "" "" "" "" "" "FAIL mismatch NOT detected"
                        ;;
                *)
                        warn "FAIL: -verify=$mode never reached its compare -- the run ended before verification ran, so nothing was verified (exit=$RUN_RC). vmsync emits no vmsync_verification_state for such a run. See $RUN_LOG"
                        results_row "$CSV" "$scenario" "${phase}-result" 1 "" "" "" "" "" "FAIL verification never ran"
                        ;;
                esac
        fi

        heal_target "$scenario" "${phase}-heal" "$path"
}

# heal_target SCENARIO PHASE PATH -- undoes a tamper with a full resync.
#
# A full recopy specifically, and unconditionally. An incremental sync would NOT
# fix this: it re-copies only blocks the SOURCE's dirty bitmap says changed, and
# the source never wrote to the offset that was corrupted on the target.
#
# -force-clean rather than a plain -reinit, and that is a consequence of the
# feature stage 14 tests rather than a preference. A -verify that RAN and found
# a difference records verify_state=failed on the target domain, and a domain
# carrying that record REFUSES an ordinary sync and a plain -reinit alike --
# deliberately, because a reinit recopies without proving the result, so
# allowing it would move the replica from "known bad" to "assumed good,
# unverified" while erasing the record that said otherwise. Every tamper
# sub-test above leaves exactly that record behind, so the heal has to be one of
# the two things that gets past it.
#
# -force-clean and not -verify-failure-reinit, of the two: this harness KNOWS it
# corrupted the replica itself, so it is discarding a finding it created rather
# than investigating one, and -verify-failure-reinit would additionally spend a
# full-image compare per heal to re-prove a replica nothing doubts. Against a
# running source it matches -reinit (the chain is rebuilt either way); its
# one extra power that could matter here -- removing the target definition first
# -- does not change the end state. Clearing a shut-down source's bitmaps is NOT
# one of its extra powers, easy as that is to assume: every reinit does
# that, and which route it takes is decided by the source's own state.
#
# Note this means the heal path does not exercise the refusal, so nothing here
# would notice if it broke. That is stage 14's job, which asserts a plain
# -reinit IS refused rather than assuming it.
#
# One of only two die()s inside a stage, and deliberately so. Everywhere
# else a stage returns non-zero so the report still prints, but a heal that
# failed leaves a replica this harness knowingly corrupted: every later stage
# would then measure that damage and report it as a vmsync defect. Losing the
# report is the smaller loss.
heal_target() {
        local scenario="$1" phase="$2" path="$3"
        log "   healing target with a full resync (-force-clean, which also discards the verify_state record the tamper produced)"
        bench_sync "$scenario" "$phase" -force-clean
        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                die "heal-after-tamper resync for $scenario/$phase did not succeed (see $RUN_LOG) -- STOP and inspect $path by hand before trusting this target replica or continuing"
        fi
}

# --- guest dirtying (Stage 8) -------------------------------------------------

# Twenty incremental syncs of an idle guest copy nothing: each one takes a
# checkpoint over an empty dirty bitmap, transfers ~0 bytes, and proves only
# that vmsync can do nothing twenty times. To make the chain mean anything the
# guest has to actually write between copies, which means running a command
# INSIDE it -- there is no virsh verb for that, only the QEMU guest agent.

# wait_for_guest_agent [TIMEOUT] -> 0 once the guest agent answers a ping.
#
# `virsh start` returns as soon as libvirt has launched qemu, but the agent
# inside the guest does not exist until that guest has BOOTED -- tens of
# seconds later. Nothing in libvirt's own domain state distinguishes the two:
# `virsh domstate` says "running" the instant qemu is up, so a stage that
# merely checks the domain is running will happily go on to interrogate an
# agent that is still in the guest's bootloader.
#
# That is not hypothetical. Stage 7 shuts the source down to prove the fence
# works and starts it again on its way out, so every later stage that needs
# the guest agent asks a guest that has been booting for about a second.
# Without this wait, stage 8 skips itself with "guest-exec unavailable" in
# every run that includes stage 7 -- reported as a skip, so it looks like a
# configuration problem on the guest rather than the harness not waiting.
#
# guest-ping rather than guest-info: it is the cheapest RPC that proves the
# agent is answering, and it is enabled wherever the agent runs at all.
# Costs nothing when the agent is already up -- the first ping answers -- so
# this is safe to call unconditionally before any guest-agent work.
wait_for_guest_agent() {
	local timeout="${1:-${GUEST_AGENT_TIMEOUT:-120}}" start=$SECONDS elapsed=0
	# Real elapsed time, not a count of sleeps: libvirt blocks its own
	# agent-command call for seconds when nothing answers, so a loop that
	# added up only its sleeps would overrun the timeout it advertises by
	# whatever virsh spent -- exactly in the case this is meant to bound.
	while [ "$elapsed" -lt "$timeout" ]; do
		if virsh_uri "$SOURCE_URI" qemu-agent-command "$SOURCE_DOMAIN" \
			'{"execute":"guest-ping"}' >/dev/null 2>&1; then
			[ "$elapsed" -gt 0 ] && log "   the guest agent on $SOURCE_DOMAIN answered after ${elapsed}s"
			return 0
		fi
		# Announced once, on the first failure, so a guest that is genuinely
		# booting explains the pause instead of looking like a hang.
		[ "$elapsed" -eq 0 ] && log "   waiting up to ${timeout}s for the guest agent on $SOURCE_DOMAIN (it may still be booting)"
		sleep 5
		elapsed=$((SECONDS - start))
	done
	warn "the guest agent on $SOURCE_DOMAIN did not answer within ${timeout}s"
	return 1
}

# guest_exec_available -> true when the agent will accept guest-exec.
#
# Two separate things have to hold, and the first does not imply the second:
# the agent has to be responding at all, and guest-exec must not be blocked.
# RHEL-family packages ship /etc/sysconfig/qemu-ga with guest-exec and
# guest-file-* in BLOCK_RPCS by default, so an agent that happily services
# vmsync's own FSFreeze will still refuse to run dd. guest-info reports each
# command with its own "enabled" flag, which answers both questions at once.
guest_exec_available() {
	local out
	out="$(virsh_uri "$SOURCE_URI" qemu-agent-command "$SOURCE_DOMAIN" \
		'{"execute":"guest-info"}' 2>&1)" || {
		GUEST_EXEC_WHY="the guest agent did not respond on $SOURCE_DOMAIN (${out//$'\n'/ })"
		return 1
	}
	# Each supported command is its own JSON object, so splitting on '{' puts
	# one command's fields on one line regardless of key order.
	if printf '%s' "$out" | tr '{' '\n' | grep '"name"[[:space:]]*:[[:space:]]*"guest-exec"' | grep -q '"enabled"[[:space:]]*:[[:space:]]*true'; then
		return 0
	fi
	GUEST_EXEC_WHY="the guest agent is running but guest-exec is disabled -- on RHEL-family guests remove it from BLOCK_RPCS in /etc/sysconfig/qemu-ga and restart qemu-guest-agent"
	return 1
}

# guest_dirty -- rewrites GUEST_DIRTY_MIB MiB of random data inside the guest,
# synchronously, so the next sync has something real to copy.
#
# Always the SAME path. Rewriting one file in place keeps the guest's block
# allocation stable, so every round dirties a comparable number of blocks and
# the per-round transferred_bytes in results.csv is a flat line any deviation
# stands out against. A fresh file per round would instead grow the image
# monotonically across twenty rounds and drift the runtime with it.
#
# conv=fsync is not optional: libvirt's dirty bitmap tracks writes that reach
# the virtual block device. A dd that returns with its data still in the
# guest's page cache leaves the bitmap empty and the next incremental sync
# copies nothing -- the exact failure this whole mechanism exists to avoid.
guest_dirty() {
	local out pid status waited=0
	out="$(virsh_uri "$SOURCE_URI" qemu-agent-command "$SOURCE_DOMAIN" \
		"{\"execute\":\"guest-exec\",\"arguments\":{\"path\":\"/bin/dd\",\"arg\":[\"if=/dev/urandom\",\"of=${GUEST_DIRTY_PATH}\",\"bs=1M\",\"count=${GUEST_DIRTY_MIB}\",\"conv=fsync\"],\"capture-output\":true}}" 2>&1)" \
		|| { warn "guest-exec dd failed on $SOURCE_DOMAIN: ${out//$'\n'/ }"; return 1; }

	pid="$(printf '%s' "$out" | grep -o '"pid"[[:space:]]*:[[:space:]]*[0-9]*' | grep -o '[0-9]*$' | head -1)"
	[ -n "$pid" ] || { warn "guest-exec returned no pid: ${out//$'\n'/ }"; return 1; }

	# guest-exec is asynchronous -- it returns a pid immediately. Without this
	# poll the next sync's checkpoint would be taken while dd is still running,
	# and each round would copy an arbitrary fraction of the write.
	while [ "$waited" -lt "${GUEST_DIRTY_TIMEOUT:-120}" ]; do
		status="$(virsh_uri "$SOURCE_URI" qemu-agent-command "$SOURCE_DOMAIN" \
			"{\"execute\":\"guest-exec-status\",\"arguments\":{\"pid\":${pid}}}" 2>&1)" || true
		if printf '%s' "$status" | grep -q '"exited"[[:space:]]*:[[:space:]]*true'; then
			if printf '%s' "$status" | grep -q '"exitcode"[[:space:]]*:[[:space:]]*0'; then
				return 0
			fi
			warn "dd inside $SOURCE_DOMAIN exited non-zero: ${status//$'\n'/ }"
			return 1
		fi
		sleep 2
		waited=$((waited + 2))
	done
	warn "dd inside $SOURCE_DOMAIN did not finish within ${GUEST_DIRTY_TIMEOUT:-120}s"
	return 1
}

# --- Stage 3: -reinit-after-failures -----------------------------------------

# Must match libvirtsync's own metadataNamespace constant (pkg/libvirtsync/
# libvirt.go) -- this is vmsync's metadata block's own namespace URI, not a
# libvirt connection URI. Everything here keys on the URI rather than on a
# prefix, and so does the xpath in vmsync_meta_field (local-name()), because
# the prefix a domain's block carries depends on which vmsync version last
# wrote it and on what libvirt did to it afterwards.
VMSYNC_METADATA_URI="http://vmsync.org/xmlns/libvirt/domain/1.0"

# Must match libvirtsync.TestFaultFailureDefine (pkg/libvirtsync/libvirt.go).
# vmsync rejects an unknown -test value at startup, so a rename there shows up
# here as an immediate, explicit "unknown -test fault" rather than as a stage
# that quietly stops testing anything.
VMSYNC_TEST_FAILURE_DEFINE="failure-define"

# Must match libvirtsync.TestFaultDieWritingBase, and the same reasoning
# applies: a rename there becomes an immediate "unknown -test fault" here
# rather than a stage that silently stops reaching the state it is about.
# Stage 16 additionally refuses to draw any conclusion from a non-zero exit it
# cannot tie to the injection line, precisely because that is what a removed
# fault looks like.
VMSYNC_TEST_DIE_WRITING_BASE="die-writing-base"

stage_reinit_after_failures() {
        log "=== Stage 3: -reinit-after-failures ==="
        local n="${REINIT_AFTER_FAILURES_N:-3}"

        # Own baseline first, same as Stage 2/4 -- RecordTargetSyncFailure is a
        # documented no-op against a target domain that doesn't exist yet ("has
        # nothing to record against"), so without this, running Stage 3 before
        # Stage 1 ever created the target (e.g. --stages reinit in isolation)
        # induces N failures that never actually persist: failure_count stays 0
        # forever and -reinit-after-failures never trips. This doesn't rely on
        # Stage 1 having run at all, so Stage 3 is self-sufficient like the
        # other stages.
        bench_sync reinit-after-failures baseline -reinit
        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                warn "baseline full sync for reinit-after-failures testing failed (see $RUN_LOG) -- aborting stage 3$(bench_sync_hint)"
                return 1
        fi

        # Induce N genuine, repeatable incremental-sync failures by removing
        # the target's own vmsync metadata (last_checkpoint included) -- this
        # is the real-world scenario -reinit-after-failures exists to auto-heal
        # (see main.go's own unverifiableCheckpointMetadataError: "if this
        # target was manually redefined, restored from an old XML, or is
        # otherwise missing vmsync's own metadata, its on-disk state cannot be
        # trusted as a base for an incremental copy"), not an artificial
        # "source domain doesn't exist" failure that says nothing about
        # whether the incremental sync mechanism itself is broken. The source
        # domain stays real and untouched throughout -- only the target's own
        # bookkeeping is removed. A failed run never calls UpdateSyncMetadata,
        # so this corruption persists across every induced attempt below,
        # until the final -reinit trigger run overwrites it with a fresh,
        # consistent value.
        if [ "$DRY_RUN" != yes ]; then
                log "removing target vmsync metadata to induce $n genuine checkpoint-chain-inconsistency failures"
                virsh_uri "$TARGET_URI" metadata "$TARGET_DOMAIN" --uri "$VMSYNC_METADATA_URI" --remove --config \
                        || { warn "could not remove target vmsync metadata for reinit-after-failures testing -- aborting stage 3"; return 1; }
        fi

        log "inducing $n consecutive failures against the real target"
        local i
        for i in $(seq 1 "$n"); do
                bench_sync reinit-after-failures "induce-$i" "-reinit-after-failures=$n"
                if [ "$RUN_RC" = 0 ] && [ "$DRY_RUN" != yes ]; then
                        warn "induced-failure run #$i unexpectedly succeeded (exit=0) -- check $RUN_LOG, this test's assumptions may not hold in your environment"
                fi
        done

        log "running a real, correct sync with -reinit-after-failures=$n -- expecting it to force a full resync instead of the incremental one it would otherwise do"
        bench_sync reinit-after-failures trigger "-reinit-after-failures=$n"

        if [ "$DRY_RUN" = yes ]; then
                :
        elif [ "$RUN_RC" = 0 ] && grep -q 'starting full pull backup' "$RUN_LOG" 2>/dev/null; then
                log "   PASS: -reinit-after-failures=$n correctly forced a full resync after $n induced failures"
                results_row "$CSV" reinit-after-failures result 0 "" "" "" "" "" "PASS forced full resync"
        else
                warn "FAIL: expected a forced full resync after $n induced failures, see $RUN_LOG"
                results_row "$CSV" reinit-after-failures result 1 "" "" "" "" "" "FAIL no forced resync observed"
        fi

        # The false-positive guard for the interrupted-rebuild refusal, made
        # here because this stage ends with the exact run that arms it: a
        # forced full resync renames the replica aside and writes new bases,
        # so it writes replica_incomplete before it starts and must clear it in
        # the write that records success.
        #
        # Stage 16 proves the field is written and refused on. This proves it
        # does not SURVIVE a run that worked -- and the cost of getting that
        # wrong is worse than the refusal never existing: every replica in the
        # estate would be force-only after its first full sync, and operators
        # would learn to type -force-promote without reading what it said.
        if [ "$DRY_RUN" != yes ] && [ "$RUN_RC" = 0 ]; then
                local marker
                marker="$(replica_incomplete "$TARGET_URI" "$TARGET_DOMAIN")"
                if [ -z "$marker" ]; then
                        log "   PASS: the forced full resync left no replica_incomplete behind"
                        results_row "$CSV" reinit-after-failures no-incomplete-marker 0 "" "" "" "" "" "PASS successful reinit cleared the marker"
                else
                        warn "FAIL: the target still carries replica_incomplete='$marker' after a forced full resync that reported success -- every -promote of this replica is now refused until another sync clears it"
                        results_row "$CSV" reinit-after-failures no-incomplete-marker 1 "" "" "" "" "" "FAIL successful reinit left the marker armed"
                fi
        fi
        return 0
}

# --- Stage 4: external snapshot lifecycle -----------------------------------

# stage_external_snapshot exercises the exact scenario libvirt's own
# checkpoint API restricts: "the creation of checkpoints when external
# snapshots exist is currently forbidden". vmsync tolerates this (see
# libvirtsync.IsCheckpointBlockedBySnapshot) by syncing incrementally
# against the existing checkpoint without advancing the chain, rather than
# failing the run outright -- this stage proves that tolerance path is
# real and the data it produces is still correct, not just that the flag
# exists. It also checks the one thing that made this fragile enough to be
# worth a dedicated regression test in the first place: target-side
# naming (disk.QcowDisk.RootSource) must stay pinned to the disk's stable,
# pre-snapshot name throughout, or the target's on-disk path would silently
# drift out from under it the moment a snapshot exists.
#
# Requires the SOURCE domain to be running (see require_dom_running's own
# comment) -- unlike the target, this harness never controls the source's
# power state, so it skips outright rather than trying to start it.

# Every overlay this stage creates is named with this prefix, and the
# precondition below recognises leftovers by it. One constant rather than the
# literal in two places: the two must agree, or the check silently stops
# seeing exactly the files it exists to find.
EXTSNAP_PREFIX="vmsync-bench-extsnap-"

# ext_snapshot_precondition -- refuse to start stage 4 against a source disk
# that is not already flat. 0 to proceed, 1 to abort the stage.
#
# This is the only stage that touches the SOURCE domain's disks, and what it
# is safe to conclude depends on what it finds already there. Three conditions
# make it wrong to begin:
#
#   * A pre-existing backing chain. This is no longer a question of blast
#     radius -- cleanup is snapshot-delete, which unwinds the snapshot this
#     stage created and leaves anybody else's structure alone. It is a
#     question of what the stage would be measuring. The "after the snapshot
#     is gone" phase asserts the checkpoint chain resumes advancing, and
#     against a source that still carries somebody else's snapshot afterwards
#     that assertion is simply false -- it would fail, and blame vmsync for
#     a snapshot this harness never created.
#
#   * The source already running on a bench overlay: a previous stage 4 died
#     between snapshot-create-as and the delete. Continuing would stack a
#     second overlay on the first, and the leftover would then be load-bearing
#     underneath it.
#
#   * A bench overlay on any OTHER disk. Naming one disk in --diskspec does
#     not stop libvirt snapshotting the rest, and for a long time this stage
#     both snapshotted the whole domain and cleaned up exactly one disk of it,
#     so the other disks stacked one overlay per run with nothing looking at
#     them. The create side now excludes them and the delete side would unwind
#     them anyway; this check is what makes the failure visible if either ever
#     stops being true, and what catches the chains left over from before.
#
# Leftover overlay FILES are a different matter and only get reported: once
# the checks above have established the live disk is flat, nothing in the
# source domain's chain can reference them. They are still worth naming --
# the overlay filename is derived from the bench PID, so a recycled PID makes
# snapshot-create-as collide with one -- but they are not a reason to refuse
# to run, and this harness deletes only overlays it created itself rather
# than guessing at ownership of files on a shared host.
ext_snapshot_precondition() {
        local active dir depth stale overlays

        active="$(disk_source_path "$SOURCE_URI" "$SOURCE_DOMAIN" "$TAMPER_DISK_DEV")" || true
        if [ -z "$active" ]; then
                warn "FAIL: could not resolve the source path of $SOURCE_DOMAIN's $TAMPER_DISK_DEV disk -- aborting stage 4 rather than snapshotting a chain this harness cannot see"
                results_row "$CSV" ext-snapshot precondition 1 "" "" "" "" "" "FAIL source disk path unresolvable"
                return 1
        fi

        case "$active" in
        *"$EXTSNAP_PREFIX"*)
                warn "FAIL: $SOURCE_DOMAIN's $TAMPER_DISK_DEV is currently running on a leftover bench overlay ($active) -- an earlier stage 4 died between snapshot-create-as and snapshot-delete. Aborting. To repair: if 'virsh -c $SOURCE_URI snapshot-list --domain $SOURCE_DOMAIN' still lists the snapshot, 'virsh -c $SOURCE_URI snapshot-delete --domain $SOURCE_DOMAIN --snapshotname NAME' undoes it completely; if its metadata is already gone, libvirt no longer knows about it and only 'virsh -c $SOURCE_URI blockcommit $SOURCE_DOMAIN $TAMPER_DISK_DEV --active --pivot --wait' can merge it back. Then re-run"
                results_row "$CSV" ext-snapshot precondition 1 "" "" "" "" "" "FAIL source running on leftover bench overlay"
                return 1
                ;;
        esac

        # The same question asked of every OTHER disk, which is where this went
        # wrong before: a lone --diskspec does not stop libvirt snapshotting the
        # rest of the domain (see stage_external_snapshot), so the collateral
        # overlays stacked up on disks nothing here was looking at. The create
        # side is fixed; this is the check that would have caught it, and the
        # one that catches the leftovers from before the fix.
        local otherdev otheractive stacked=""
        while read -r otherdev; do
                [ -n "$otherdev" ] || continue
                [ "$otherdev" = "$TAMPER_DISK_DEV" ] && continue
                otheractive="$(disk_source_path "$SOURCE_URI" "$SOURCE_DOMAIN" "$otherdev")" || true
                case "$otheractive" in
                *"$EXTSNAP_PREFIX"*)
                        stacked="${stacked}${stacked:+ }${otherdev}=${otheractive}(depth $(disk_backing_depth "$SOURCE_URI" "$SOURCE_DOMAIN" "$otherdev"))"
                        ;;
                esac
        done < <(domain_disk_devs "$SOURCE_URI" "$SOURCE_DOMAIN")

        if [ -n "$stacked" ]; then
                warn "FAIL: $SOURCE_DOMAIN is running on leftover bench overlays on disks this stage never meant to snapshot: $stacked. Earlier bench runs snapshotted the whole domain but cleaned up only $TAMPER_DISK_DEV, so these stacked up one per run. Aborting stage 4 rather than adding another. snapshot-delete cannot undo these: the old cleanup dropped each snapshot's metadata with --metadata, so libvirt no longer knows they exist and only a manual merge is left. For each disk above: read the chain ('virsh -c $SOURCE_URI dumpxml $SOURCE_DOMAIN') and confirm every entry above the real base carries '${EXTSNAP_PREFIX}' -- a foreign snapshot in there must not be flattened -- then 'virsh -c $SOURCE_URI blockcommit $SOURCE_DOMAIN DEV --active --pivot --wait --base /path/to/the/real/base.qcow2', which merges the whole bench stack into the base in one job, and delete the freed overlay files afterwards"
                results_row "$CSV" ext-snapshot precondition 1 "" "" "" "" "" "FAIL leftover bench overlays on non-tamper disks"
                return 1
        fi

        depth="$(disk_backing_depth "$SOURCE_URI" "$SOURCE_DOMAIN" "$TAMPER_DISK_DEV")"
        if [ "$depth" -gt 0 ]; then
                warn "FAIL: $SOURCE_DOMAIN's $TAMPER_DISK_DEV already sits on a ${depth}-level backing chain below $active -- aborting stage 4. The cleanup (snapshot-delete) would leave that chain alone, so this is not about damaging it: it is that the stage's closing assertion, 'the checkpoint chain resumes advancing once the snapshot is gone', cannot hold on a source that still carries somebody else's snapshot afterwards. Running anyway would fail the stage and blame vmsync for a snapshot this harness never created"
                results_row "$CSV" ext-snapshot precondition 1 "" "" "" "" "" "FAIL pre-existing backing chain (depth=$depth)"
                return 1
        fi

        stale="$(virsh_uri "$SOURCE_URI" snapshot-list --domain "$SOURCE_DOMAIN" --name 2>/dev/null | grep "^${EXTSNAP_PREFIX}" || true)"
        if [ -n "$stale" ]; then
                warn "FAIL: $SOURCE_DOMAIN still carries snapshot metadata from an earlier stage 4 ($(printf '%s' "$stale" | tr '\n' ' ')) -- the disk itself is flat, so this is stale bookkeeping only, but leaving it would let snapshot-create-as collide on the name. Clear it with 'virsh -c $SOURCE_URI snapshot-delete --domain $SOURCE_DOMAIN --snapshotname NAME --metadata' and re-run"
                results_row "$CSV" ext-snapshot precondition 1 "" "" "" "" "" "FAIL stale bench snapshot metadata"
                return 1
        fi

        dir="$(dirname "$active")"
        overlays="$(run_shell_on "$SOURCE_HOST" "$SOURCE_LOCAL" "ls -1 '${dir}'/*${EXTSNAP_PREFIX}* 2>/dev/null" || true)"
        if [ -n "$overlays" ]; then
                warn "$SOURCE_DOMAIN's disk directory still holds overlay files from interrupted stage 4 runs. Not a failure -- the live disk is flat, so nothing references them -- but the overlay name is derived from the bench PID, and a recycled PID would make snapshot-create-as collide with one of these: $(printf '%s' "$overlays" | tr '\n' ' ')"
        fi

        log "   precondition OK: $SOURCE_DOMAIN's $TAMPER_DISK_DEV is a flat image ($active) with no bench snapshot metadata left over"
        results_row "$CSV" ext-snapshot precondition 0 "" "" "" "" "" "PASS source disk flat, no stale bench snapshot metadata"
        return 0
}

stage_external_snapshot() {
        log "=== Stage 4: external snapshot lifecycle (sync while a snapshot exists, then after it's removed) ==="

        if [ "$DRY_RUN" != yes ]; then
                stage_needs_target_shutoff "$CSV" ext-snapshot "stage snapshot" || return 0
                require_dom_running "$SOURCE_URI" "$SOURCE_DOMAIN" "source"
                ext_snapshot_precondition || return 1
        fi

        # A real baseline first (not just "some prior sync, whenever") so the
        # "while a snapshot exists" sync below is guaranteed to be a genuine
        # INCREMENTAL run. That distinction matters: CreateCheckpoint failing
        # is only ever tolerated for an incremental sync (parent != "") -- a
        # full sync (parent == "") has no earlier checkpoint to fall back on,
        # so the exact same failure is fatal there by design (see main.go's own
        # comment next to that check). Without this baseline, a first-ever sync
        # of this domain would hit that fatal path instead of the tolerant one
        # this stage exists to test.
        bench_sync ext-snapshot baseline -reinit
        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                warn "baseline full sync for external-snapshot testing failed (see $RUN_LOG) -- aborting stage 4$(bench_sync_hint)"
                return 1
        fi

        local target_path_before=""
        if [ "$DRY_RUN" != yes ]; then
                target_path_before="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
        fi

        local snap_name="${EXTSNAP_PREFIX}$$"
        local overlay_path=""
        if [ "$DRY_RUN" != yes ]; then
                # Naming ONE disk in --diskspec does not limit the snapshot to
                # it. libvirt snapshots every disk it is not told to skip, and
                # gives the unnamed ones an auto-derived overlay name
                # (<base>.<snapshot name>) -- so a lone
                # "--diskspec vda,snapshot=external" on a two-disk domain
                # silently redirects vdb onto an overlay too.
                #
                # snapshot-delete below now unwinds whatever this creates, on
                # every disk, so the exclusions are no longer what keeps the
                # source clean. They are here because the stage is scoped to
                # one disk and should take one disk: TAMPER_DISK_DEV is the
                # only disk the precondition established is safe to redirect,
                # and it is the only disk the assertions look at. Snapshotting
                # the rest would be an unasked-for redirect of a production
                # source's disks to prove nothing extra.
                #
                # snapshot=no is the only way to say it; there is no "these
                # disks only" form.
                local -a snapspec=(--diskspec "${TAMPER_DISK_DEV},snapshot=external")
                local other alldevs
                alldevs="$(domain_disk_devs "$SOURCE_URI" "$SOURCE_DOMAIN")"
                # An empty enumeration is not "a single-disk domain", it is
                # "domblklist did not answer" -- and the two differ by the
                # whole domain getting snapshotted. Refuse rather than fall
                # back to the shape that caused the leak in the first place.
                if [ -z "$alldevs" ]; then
                        warn "could not enumerate $SOURCE_DOMAIN's disks, so the other disks cannot be excluded from the snapshot -- aborting stage 4 rather than redirecting disks this stage never meant to touch"
                        results_row "$CSV" ext-snapshot precondition 1 "" "" "" "" "" "FAIL disk enumeration failed, snapshot not attempted"
                        return 1
                fi
                while read -r other; do
                        [ -n "$other" ] || continue
                        [ "$other" = "$TAMPER_DISK_DEV" ] && continue
                        snapspec+=(--diskspec "${other},snapshot=no")
                done <<<"$alldevs"

                log "creating an external, disk-only snapshot '$snap_name' on source disk $TAMPER_DISK_DEV (${#snapspec[@]} diskspec(s): every other disk excluded with snapshot=no)"
                virsh_uri "$SOURCE_URI" snapshot-create-as --domain "$SOURCE_DOMAIN" --name "$snap_name" \
                        "${snapspec[@]}" --disk-only --atomic \
                        || { warn "failed to create external snapshot '$snap_name' on $SOURCE_DOMAIN -- aborting stage 4 (--atomic means either it's fully created or fully rolled back; nothing should be left half-done)"; return 1; }
                # "|| true": same reasoning as the tamper test's own lookup -- fall
                # through to a specific, actionable message rather than dying here
                # with none.
                overlay_path="$(disk_source_path "$SOURCE_URI" "$SOURCE_DOMAIN" "$TAMPER_DISK_DEV")" || true
                log "source disk now redirected to overlay: ${overlay_path:-<could not resolve>}"
        fi

        log "--- syncing while the external snapshot exists (expect: sync+verify succeed and the target path stays stable) ---"
        bench_sync ext-snapshot during-snapshot -verify=fast
        if [ "$DRY_RUN" != yes ]; then
                local snap_count=0
                local target_path_during=""
                if [ "$RUN_RC" = 0 ]; then
                        snap_count="$(prom_sum "$RUN_PROM" vmsync_external_snapshot_count)"
                        target_path_during="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
                fi

                if [ "$RUN_RC" != 0 ]; then
                        warn "FAIL: sync+verify while an external snapshot existed did not succeed (exit=$RUN_RC) -- see $RUN_LOG"
                        results_row "$CSV" ext-snapshot during-result 1 "" "" "" "" "" "FAIL sync did not succeed with snapshot present"
                elif [ "$snap_count" -lt 1 ]; then
                        # vmsync counts the source domain's external snapshots
                        # itself and reports them as
                        # vmsync_external_snapshot_count. Zero here means the
                        # snapshot this stage just created was not visible to
                        # the sync at all, so nothing below would be testing
                        # what it claims to -- this is the real "did the
                        # snapshot take effect?" check, which the old version
                        # only ever asked rhetorically in a warning message.
                        warn "FAIL: vmsync reported vmsync_external_snapshot_count=$snap_count during the sync, but this stage created an external snapshot on $TAMPER_DISK_DEV -- the snapshot did not take effect, or the metric is broken; see $RUN_LOG"
                        results_row "$CSV" ext-snapshot during-result 1 "" "" "" "" "" "FAIL snapshot not visible to vmsync (count=$snap_count)"
                elif [ -n "$target_path_before" ] && [ "$target_path_during" != "$target_path_before" ]; then
                        warn "FAIL: target disk path changed while the snapshot existed (before='$target_path_before' during='$target_path_during') -- RootSource-based naming should have kept this stable"
                        results_row "$CSV" ext-snapshot during-result 1 "" "" "" "" "" "FAIL target path drifted during snapshot"
                elif grep -q 'checkpoint creation blocked by an existing external snapshot' "$RUN_LOG" 2>/dev/null; then
                        log "   PASS: synced and verified with the external snapshot present; libvirt blocked the new checkpoint and vmsync's tolerance path handled it (vmsync_external_snapshot_count=$snap_count, target path unchanged)"
                        results_row "$CSV" ext-snapshot during-result 0 "" "" "" "" "" "PASS synced+verified via tolerance path, count=$snap_count"
                else
                        # Not a failure. Whether libvirt refuses to create a
                        # checkpoint while an external snapshot exists is
                        # version-dependent, and newer libvirt/qemu pairs allow
                        # it. When they do, vmsync's tolerance path is simply
                        # never needed and the checkpoint chain keeps advancing
                        # normally. Treating "the fallback wasn't needed" as
                        # "the fallback is broken" would make a healthy run
                        # report a regression.
                        #
                        # The assertions that DO apply here -- the sync
                        # succeeded, the snapshot was genuinely visible to
                        # vmsync, and the target path did not drift -- are all
                        # checked above rather than behind the tolerance-log
                        # grep, which would skip them entirely whenever it
                        # didn't match.
                        log "   PASS: synced and verified with the external snapshot present; this libvirt permitted the checkpoint, so the tolerance path was not exercised (vmsync_external_snapshot_count=$snap_count, target path unchanged)"
                        results_row "$CSV" ext-snapshot during-result 0 "" "" "" "" "" "PASS synced+verified, checkpoint not blocked by this libvirt, count=$snap_count"
                fi
        fi

        log "--- removing the external snapshot (snapshot-delete) ---"
        if [ "$DRY_RUN" != yes ]; then
                # snapshot-delete, not blockcommit: it is the inverse of the
                # operation that created this, and it is the one that knows
                # what "this" is. libvirt merges every disk the snapshot
                # covers, pivots each of them, removes the overlay files and
                # drops the metadata -- one call, whatever the snapshot turned
                # out to span. blockcommit knew about a single disk and a
                # single chain, which is precisely how the collateral overlays
                # got left behind: it committed the disk it was told about and
                # had nothing to say about the others.
                #
                # This is still the only place the harness can damage the
                # SOURCE domain, so a failure here is still a die(): a
                # half-merged chain on a production source is not something to
                # carry into another eight stages for the sake of finishing a
                # report.
                virsh_uri "$SOURCE_URI" snapshot-delete --domain "$SOURCE_DOMAIN" --snapshotname "$snap_name" \
                        || die "snapshot-delete failed for $SOURCE_DOMAIN/$snap_name -- STOP: the source domain's disk chain may now be in an inconsistent state, inspect it by hand ('virsh -c $SOURCE_URI dumpxml $SOURCE_DOMAIN' for the chains, 'virsh -c $SOURCE_URI blockjob $SOURCE_DOMAIN $TAMPER_DISK_DEV' for a merge still running) before continuing"

                # What the delete promised, checked rather than assumed. This
                # is the assertion whose absence let the leak run for as long
                # as it did: nothing ever looked at the domain again after
                # cleanup, so nothing noticed the chain had not gone back to
                # flat.
                local leftdev leftactive
                while read -r leftdev; do
                        [ -n "$leftdev" ] || continue
                        leftactive="$(disk_source_path "$SOURCE_URI" "$SOURCE_DOMAIN" "$leftdev")" || true
                        case "$leftactive" in
                        *"$EXTSNAP_PREFIX"*)
                                warn "FAIL: snapshot-delete reported success but $SOURCE_DOMAIN's $leftdev is STILL running on a bench overlay ($leftactive) -- the source domain has not been returned to a flat chain and will accumulate another overlay on the next run. Inspect it by hand before re-running"
                                results_row "$CSV" ext-snapshot cleanup 1 "" "" "" "" "" "FAIL source still on a bench overlay after snapshot-delete"
                                ;;
                        esac
                done < <(domain_disk_devs "$SOURCE_URI" "$SOURCE_DOMAIN")

                if [ -n "$overlay_path" ] && run_shell_on "$SOURCE_HOST" "$SOURCE_LOCAL" "test -e '$overlay_path'" 2>/dev/null; then
                        warn "snapshot-delete left the overlay file $overlay_path behind on $SOURCE_HOST. The chain no longer references it, so this is disk space rather than a correctness problem -- but it is not what a successful delete should leave, and the precondition will report it on the next run"
                fi
        fi

        log "--- syncing again after the external snapshot is gone (expect: checkpoint chain resumes advancing) ---"
        bench_sync ext-snapshot after-snapshot -verify=fast
        if [ "$DRY_RUN" != yes ]; then
                if [ "$RUN_RC" != 0 ]; then
                        warn "FAIL: sync+verify after removing the external snapshot did not succeed (exit=$RUN_RC) -- see $RUN_LOG"
                        results_row "$CSV" ext-snapshot after-result 1 "" "" "" "" "" "FAIL sync did not succeed after snapshot removal"
                elif grep -q 'checkpoint chain did not advance this run' "$RUN_LOG" 2>/dev/null; then
                        warn "FAIL: checkpoint chain still not advancing after the external snapshot was removed -- see $RUN_LOG"
                        results_row "$CSV" ext-snapshot after-result 1 "" "" "" "" "" "FAIL checkpoint chain still not advancing"
                else
                        log "   PASS: synced and verified correctly after the external snapshot was removed, checkpoint chain resumed advancing"
                        results_row "$CSV" ext-snapshot after-result 0 "" "" "" "" "" "PASS synced+verified after snapshot removed"
                fi
        fi
        return 0
}

# --- Stage 5: DefineDomain redefine/rollback coverage -----------------------
#
# libvirtsync.DefineDomain is now the SOLE place vmsync ever undefines or
# redefines the target domain -- this session's -reinit fix deliberately
# removed -reinit's own early undefine specifically so DefineDomain's own
# capture-XML/undefine/redefine/rollback-on-failure sequence would be the
# only thing relied on -- and, before this stage, nothing exercised it. Two
# sub-tests, run back to back under the "define" stage:
#
#   5a (reliable): a real UUID collision, forcing vmsync's documented
#   stripped-UUID retry branch. Scored pass/fail.
#
#   5b (deterministic): one sync run with vmsync's own -test=failure-define,
#   which corrupts the document handed to DomainDefineXML so libvirt refuses
#   the redefine, to check that DefineDomain's rollback genuinely restores
#   the target's prior definition. Scored pass/fail.
#
#   The failure is injected by vmsync itself rather than forced from outside
#   with a timed iptables rule on TARGET_HOST. See
#   stage_define_domain_rollback's own comment for why an external disruption
#   cannot work and why it can report the opposite of the truth.
#
# Not part of the default --stages list (see usage()): both halves are
# deliberate failure injection rather than measurement, and 5a briefly
# defines a throwaway domain on the target. Neither touches host-level
# networking, and neither is more destructive to the target VM than -reinit
# already is -- but run them against the same fully disposable test host this
# whole harness already requires (see the SAFETY note at the top of this
# file).

# domain_definition_xml URI DOMAIN -> prints DOMAIN's current XML, or
# nothing (empty string, exit 0) when it doesn't exist at all -- callers
# that need to tell "genuinely undefined" apart from "query failed" should
# check domain_exists first.
# --inactive, matching the DOMAIN_XML_INACTIVE that DefineDomain itself
# captures and would restore. Without it the two agree only because the target
# happens to be shut off whenever this is called -- correct by accident, and
# silently wrong the first time it is called on a running domain.
domain_definition_xml() {
        virsh_uri "$1" dumpxml --inactive "$2" 2>/dev/null
}

# domain_definition_body URI DOMAIN -> DOMAIN's inactive XML with the
# <metadata> element removed.
#
# This is what stage 5b compares, and the reason is that the previous raw
# comparison had become a guaranteed false failure. vmsync records
# replica_written_at on the TARGET domain after the copy and BEFORE
# DefineDomain runs (cmd/vmsync/main.go), deliberately -- F2 exists so that a
# run which wrote bytes and then failed still says so, which is what keeps
# the next run's out-of-band-write check from wedging. So on a
# -test=failure-define run the target's metadata legitimately changes between
# a "before" snapshot taken outside vmsync and the state DefineDomain
# captures and correctly rolls back to. The rollback was right; the assertion
# was comparing against a document that no longer existed.
#
# Removing the whole element rather than just vmsync's namespaced child: the
# child is nested and namespace-prefixed, which is real XML surgery in awk,
# while the whole element is two unambiguous lines. What that gives up is
# noticing another tool's metadata disappearing across a rollback -- which
# cannot happen here, since the harness requires a dedicated disposable
# target that only vmsync manages. The assertion this leaves is the one 5b
# actually cares about: the domain DEFINITION -- disks, devices, uuid, name
# -- is what makes a promoted replica boot correctly, and vmsync rewrites its
# own metadata on every run regardless.
#
# Handles both <metadata>...</metadata> and a self-closing <metadata/>, since
# an empty element before the run and a populated one after it would
# otherwise differ by that very line.
# DEFINE_REQUIRED_META_FIELDS: the vmsync metadata fields
# libvirtsync.UpdateSyncMetadata sets UNCONDITIONALLY on every target replica.
#
# A literal list rather than one derived from vmsync's own output, for the
# same reason cmd/vmsync-agent's flag-vocabulary test keeps one: deriving both
# sides from the same source would prove nothing. A field dropped from vmsync
# without being dropped here is exactly what should fail.
#
# Deliberately excludes the conditional ones. replication_role,
# source_stopped_at_sync and replica_written_at are each set or removed
# depending on the run, so requiring them outright would make this stage fail
# on a state that is perfectly correct.
DEFINE_REQUIRED_META_FIELDS="last_checkpoint last_sync_timestamp failure_count replica_source checkpoint_at"

# vmsync_meta_snapshot URI DOMAIN -> "field=value" lines for every field 5c
# cares about, present or not.
#
# Absent fields are emitted with an empty value rather than skipped, so a
# field that DISAPPEARS between two snapshots is a visible change rather than
# a shorter list.
vmsync_meta_snapshot() {
	local uri="$1" domain="$2" f
	for f in $DEFINE_REQUIRED_META_FIELDS replication_role replica_written_at; do
		printf '%s=%s\n' "$f" "$(vmsync_meta_field "$uri" "$domain" "$f")"
	done
}

# meta_snapshot_field SNAPSHOT FIELD -> that field's value out of a snapshot.
meta_snapshot_field() {
	printf '%s\n' "$1" | sed -n "s/^${2}=//p" | head -1
}

domain_definition_body() {
        domain_definition_xml "$1" "$2" | awk '
                /^[[:space:]]*<metadata[[:space:]]*\/>[[:space:]]*$/ { next }
                /^[[:space:]]*<metadata>[[:space:]]*$/ { skip = 1 }
                skip != 1 { print }
                /^[[:space:]]*<\/metadata>[[:space:]]*$/ { skip = 0 }
        '
}

stage_define_domain_uuid_collision() {
        log "--- Stage 5a: DefineDomain uuid-collision retry ---"

        if [ "$DRY_RUN" != yes ]; then
                stage_needs_target_shutoff "$CSV" define-precondition "stage define (5a)" || return 0
        fi

        bench_sync define-uuid-collision baseline -reinit
        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                warn "baseline full sync for DefineDomain uuid-collision testing failed (see $RUN_LOG) -- aborting stage 5a$(bench_sync_hint)"
                return 1
        fi

        if [ "$DRY_RUN" = yes ]; then
                bench_sync define-uuid-collision trigger -reinit
                return 0
        fi

        local src_uuid target_uuid_before
        src_uuid="$(domain_uuid "$SOURCE_URI" "$SOURCE_DOMAIN")" || { warn "could not read source domain UUID via $SOURCE_URI: $VIRSH_ERR"; return 1; }
        target_uuid_before="$(domain_uuid "$TARGET_URI" "$TARGET_DOMAIN")" || { warn "could not read target domain UUID via $TARGET_URI: $VIRSH_ERR"; return 1; }
        [ "$target_uuid_before" = "$src_uuid" ] || warn "target UUID ($target_uuid_before) doesn't match source UUID ($src_uuid) right after a fresh -reinit baseline -- unexpected, but continuing"

        # The target replica ($TARGET_DOMAIN) itself is still defined at this
        # point, holding $src_uuid -- that's the baseline sync's own normal,
        # correct behavior. Defining the throwaway domain below with that
        # same UUID would collide with $TARGET_DOMAIN itself (not some
        # unrelated stray domain), since libvirt never allows two domains on
        # the same host to share a UUID. Undefining $TARGET_DOMAIN first
        # frees the UUID for the throwaway domain to claim instead, so the
        # later "trigger" run's own DefineDomain -- which finds no existing
        # $TARGET_DOMAIN to undefine, and goes straight to redefining it with
        # $src_uuid -- collides against the throwaway domain exactly the way
        # a real, independent stray domain squatting on the UUID would.
        # --keep-nvram matches DefineDomain's own DOMAIN_UNDEFINE_KEEP_NVRAM
        # (see libvirt.go) so this doesn't delete a UEFI/OVMF target's real
        # varstore file.
        log "undefining target domain '$TARGET_DOMAIN' to free its uuid for the throwaway collision domain"
        virsh_uri "$TARGET_URI" undefine "$TARGET_DOMAIN" --keep-nvram >/dev/null \
                || { warn "could not undefine target domain '$TARGET_DOMAIN' to free its uuid for the collision test -- aborting stage 5a"; return 1; }

        local collision_name="vmsync-bench-uuid-collision-$$"
        log "defining throwaway domain '$collision_name' on target reusing source UUID $src_uuid"
        printf '%s\n' \
                "<domain type='qemu'>" \
                "  <name>${collision_name}</name>" \
                "  <uuid>${src_uuid}</uuid>" \
                "  <memory unit='KiB'>65536</memory>" \
                "  <os><type arch='x86_64'>hvm</type></os>" \
                "  <devices></devices>" \
                "</domain>" \
                | virsh_uri "$TARGET_URI" define /dev/stdin >/dev/null \
                || { warn "failed to define throwaway uuid-collision domain '$collision_name' on target -- aborting stage 5a"; return 1; }

        bench_sync define-uuid-collision trigger -reinit

        local pass=yes reason="" target_uuid_after=""
        if [ "$RUN_RC" != 0 ]; then
                pass=no
                reason="vmsync failed (exit=$RUN_RC) instead of falling back past the uuid collision"
        else
                if target_uuid_after="$(domain_uuid "$TARGET_URI" "$TARGET_DOMAIN")"; then
                        [ "$target_uuid_after" != "$src_uuid" ] || { pass=no; reason="target UUID unchanged ($target_uuid_after) -- the uuid-collision fallback does not appear to have been exercised"; }
                else
                        pass=no
                        reason="could not read target UUID after the run: $VIRSH_ERR"
                fi
        fi

        if [ "$pass" = yes ]; then
                log "   PASS: DefineDomain survived a real UUID collision via its stripped-UUID retry (new target uuid=$target_uuid_after)"
                results_row "$CSV" define-uuid-collision result 0 "" "" "" "" "" "PASS uuid-fallback retry succeeded"
        else
                warn "FAIL: $reason -- see $RUN_LOG"
                results_row "$CSV" define-uuid-collision result 1 "" "" "" "" "" "FAIL $reason"
        fi

        log "removing throwaway uuid-collision domain '$collision_name'"
        virsh_uri "$TARGET_URI" undefine "$collision_name" >/dev/null 2>&1 \
                || warn "could not undefine throwaway domain '$collision_name' on target -- remove it by hand: virsh -c $TARGET_URI undefine $collision_name"
        return 0
}

# Stage 5b: DefineDomain's rollback-on-failure.
#
# vmsync injects the failure itself, via -test=failure-define. That corrupts
# the document handed to DomainDefineXML rather than skipping the call, so
# libvirt genuinely refuses it and the rollback runs against the state a real
# rejection leaves behind. No timing, no background process, no firewall rules
# on anybody's hypervisor.
#
# Forcing the failure from outside -- watching the log for the undefine and
# then cutting SSH to the target with an iptables rule -- cannot work, for two
# independent reasons:
#
#   - The window runs from UndefineFlags returning to DomainDefineXML being
#     called, and holds no I/O at all -- rename, strip uuid, rewrite disk
#     paths, splice metadata, all in memory, over in about a millisecond.
#     The harness would need up to 200ms of poll latency plus a fresh SSH
#     handshake to get its rule in place: two orders of magnitude late, every
#     run.
#   - Landing it would be worse. The rollback restores over the SAME libvirt
#     connection the disruption severs, so a perfectly timed hit necessarily
#     kills the recovery being tested. No PASS is reachable.
#
# It would also report PASS wrongly: a rule landing slightly EARLY kills the
# undefine instead, which returns before the rollback closure is even
# constructed -- leaving a non-zero exit and an unchanged definition, which is
# exactly what a successful rollback looks like from outside.
#
# The verdict reads vmsync's own log rather than inferring from the exit code,
# because "the rollback restored it" and "nothing was ever undefined" are
# indistinguishable from the outside.
stage_define_domain_rollback() {
        log "--- Stage 5b: DefineDomain rollback-on-failure ---"

        if [ "$DRY_RUN" = yes ]; then
                bench_sync define-rollback baseline -reinit
                bench_sync define-rollback trigger -reinit "-test=$VMSYNC_TEST_FAILURE_DEFINE"
                results_row "$CSV" define-rollback result DRYRUN "" "" "" "" "" "SKIP dry run"
                # Called even here, so --dry-run lists every check this stage
                # would perform rather than silently omitting 5c.
                define_metadata_subtest "" ""
                return 0
        fi

        stage_needs_target_shutoff "$CSV" define-precondition "stage define (5b)" || return 0

        bench_sync define-rollback baseline -reinit
        if [ "$RUN_RC" != 0 ]; then
                warn "baseline full sync for DefineDomain rollback testing failed (see $RUN_LOG) -- aborting stage 5b$(bench_sync_hint)"
                return 1
        fi

        # Captured the same way DefineDomain captures it (DOMAIN_XML_INACTIVE),
        # so "restored to what it was" compares the same document vmsync
        # actually saved and would put back -- minus the <metadata> element,
        # which vmsync legitimately rewrites mid-run and which therefore
        # cannot be part of this assertion. See domain_definition_body for the
        # full reasoning; comparing the raw dumps made this a guaranteed false
        # failure from the moment replica_written_at was added.
        local xml_before
        xml_before="$(domain_definition_body "$TARGET_URI" "$TARGET_DOMAIN")"
        [ -n "$xml_before" ] || { warn "could not capture target domain XML before the rollback test -- aborting stage 5b"; return 1; }

        # Captured here, in 5b's window, because 5c has to compare the state
        # BEFORE the failed define against the state immediately after the
        # rollback -- and 5b's own heal at the end of this function rewrites
        # the metadata, so a check run afterwards could never see it.
        local meta_before
        meta_before="$(vmsync_meta_snapshot "$TARGET_URI" "$TARGET_DOMAIN")"

        # The target must already exist for this to test anything: DefineDomain
        # only undefines, and therefore only has something to roll back to,
        # when it does. The baseline above is what guarantees that.
        bench_sync define-rollback trigger -reinit "-test=$VMSYNC_TEST_FAILURE_DEFINE"

        local rolled_back=no undefine_failed=no
        grep -q "restored target domain to its previous definition" "$RUN_LOG" 2>/dev/null && rolled_back=yes
        grep -q "undefine existing target domain" "$RUN_LOG" 2>/dev/null && undefine_failed=yes

        local xml_after
        xml_after="$(domain_definition_body "$TARGET_URI" "$TARGET_DOMAIN")"

        # 5c, run here for the timing reason above. Its verdict is recorded
        # under its own scenario name, so stripping <metadata> out of 5b's
        # comparison cannot quietly take vmsync's metadata out of the stage's
        # coverage along with it.
        define_metadata_subtest "$meta_before" "$(vmsync_meta_snapshot "$TARGET_URI" "$TARGET_DOMAIN")"

        if [ "$RUN_RC" = 0 ]; then
                warn "FAIL: -test=$VMSYNC_TEST_FAILURE_DEFINE was passed but the run still exited 0 -- the injected failure did not take effect. Is $VMSYNC_BIN old enough not to know the flag? See $RUN_LOG"
                results_row "$CSV" define-rollback result 1 "" "" "" "" "" "FAIL injected failure did not take effect"
        elif [ "$undefine_failed" = yes ]; then
                warn "FAIL: the run failed at the UNDEFINE, before the injected redefine failure -- the rollback path was never reached (see $RUN_LOG)"
                results_row "$CSV" define-rollback result 1 "" "" "" "" "" "FAIL failed at the undefine not the redefine"
        elif [ "$rolled_back" != yes ]; then
                # Distinguished from the cases below because "the redefine
                # failed and nothing tried to restore" and "it restored the
                # wrong thing" are different bugs with different fixes.
                warn "FAIL: the redefine failed as instructed, but vmsync never reported restoring the previous definition -- the rollback did not run (see $RUN_LOG)"
                results_row "$CSV" define-rollback result 1 "" "" "" "" "" "FAIL rollback never ran"
        elif [ -z "$xml_after" ]; then
                warn "FAIL: vmsync reported rolling back, but the target domain is gone/undefined -- the restore did not take (see $RUN_LOG)"
                results_row "$CSV" define-rollback result 1 "" "" "" "" "" "FAIL target left undefined after a reported rollback"
        elif [ "$xml_after" = "$xml_before" ]; then
                log "   PASS: the redefine failed, the rollback ran, and the target's definition matches what it was before"
                results_row "$CSV" define-rollback result 0 "" "" "" "" "" "PASS rollback restored prior definition"
        else
                # Save both documents and show the difference rather than
                # telling the operator to "diff the two dumps by hand" -- there
                # was nothing on disk to diff, so that instruction cost a whole
                # extra run to act on. Whatever survives the metadata strip is
                # a genuine finding and worth reading immediately.
                local before_file="$RUN_DIR/logs/define-rollback.xml-before"
                local after_file="$RUN_DIR/logs/define-rollback.xml-after"
                printf '%s\n' "$xml_before" >"$before_file"
                printf '%s\n' "$xml_after" >"$after_file"
                warn "FAIL: the rollback ran but the target's definition differs from what it was before -- it did not fully restore it. This is the failure mode that matters, because a replica defined from a half-restored document is one that boots wrong on the day it is promoted (see $RUN_LOG)"
                if command -v diff >/dev/null 2>&1; then
                        warn "the difference (vmsync's own <metadata> already excluded), before -> after:"
                        diff -u "$before_file" "$after_file" | head -40 >&2 || true
                else
                        warn "dumps saved for comparison: $before_file and $after_file"
                fi
                results_row "$CSV" define-rollback result 1 "" "" "" "" "" "FAIL target definition differs from before the run"
        fi

        # The trigger run reinitialised the disks and then failed before
        # recording any metadata, so the replica is left with fresh disks and
        # the OLD definition -- consistent, but with no checkpoint bookkeeping.
        # Heal it, so a later stage in the same --stages list does not inherit
        # a target that looks synced and is not.
        heal_target define-rollback heal "$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")"
        return 0
}

# define_metadata_subtest BEFORE AFTER -- Stage 5c: vmsync's own metadata must
# survive a rolled-back redefine.
#
# This exists because 5b deliberately stops looking at it. 5b compares the
# domain DEFINITION with <metadata> stripped out, since vmsync rewrites its
# own metadata mid-run and comparing it raw made 5b a guaranteed false
# failure. But "not part of 5b's comparison" must not become "not tested at
# all": the whole point of the rollback is that a failed redefine leaves the
# replica exactly as usable as it was, and a replica whose vmsync metadata was
# wiped is NOT usable -- the next sync cannot find its checkpoint, and
# promotion has no record of what this domain is or what it was replicating.
#
# Four assertions, in the order their failures would matter:
#
#   1. The block still exists at all. The coarsest form of "the rollback did
#      not delete it", and the one that would bite hardest.
#   2. Every unconditionally-set field is still present.
#   3. Those fields are UNCHANGED. This is the sharp one: they are written
#      through DefineDomain, which 5b just made fail, so a run that committed
#      them anyway would mean the failure was not atomic.
#   4. replica_written_at is present, and is allowed to differ. vmsync records
#      it BEFORE DefineDomain precisely so a run that wrote bytes and then
#      failed still says so (F2) -- so a new value here is correct, and its
#      ABSENCE would be the bug.
define_metadata_subtest() {
	local before="$1" after="$2" field bval aval
	local failures=0 details=""

	log "--- Stage 5c: vmsync metadata must survive the rolled-back redefine ---"

	if [ "$DRY_RUN" = yes ]; then
		results_row "$CSV" define-metadata result DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi

	# The baseline sync must have produced metadata in the first place, or
	# every assertion below passes vacuously against two empty snapshots.
	for field in $DEFINE_REQUIRED_META_FIELDS; do
		if [ -z "$(meta_snapshot_field "$before" "$field")" ]; then
			warn "SKIP 5c: the baseline sync left no $field on the target, so there is no metadata whose survival could be tested. Check the baseline in $RUN_LOG"
			results_row "$CSV" define-metadata result "" "" "" "" "" "" "SKIP no baseline metadata to test"
			return 0
		fi
	done

	if ! has_vmsync_metadata "$TARGET_URI" "$TARGET_DOMAIN"; then
		warn "FAIL: the rollback left the target with NO vmsync metadata at all. The domain definition may be restored, but the replica is unusable: the next sync cannot find its checkpoint and a promotion has no record of what it was replicating. See $RUN_LOG"
		results_row "$CSV" define-metadata result 1 "" "" "" "" "" "FAIL vmsync metadata deleted by the rollback"
		return 0
	fi

	for field in $DEFINE_REQUIRED_META_FIELDS; do
		bval="$(meta_snapshot_field "$before" "$field")"
		aval="$(meta_snapshot_field "$after" "$field")"
		if [ -z "$aval" ]; then
			failures=$((failures + 1))
			details="${details}${details:+; }$field disappeared"
		elif [ "$aval" != "$bval" ]; then
			# Written through DefineDomain, which failed -- so a changed
			# value means the failed define committed part of its work.
			failures=$((failures + 1))
			details="${details}${details:+; }$field changed from '$bval' to '$aval' although the redefine failed"
		fi
	done

	# Present, not unchanged: a new timestamp here is the correct outcome.
	if [ -z "$(meta_snapshot_field "$after" replica_written_at)" ]; then
		failures=$((failures + 1))
		details="${details}${details:+; }replica_written_at is absent although this run wrote the replica disks -- the next run's out-of-band-write check may now refuse it"
	fi

	if [ "$failures" = 0 ]; then
		log "   PASS: vmsync's metadata survived the rollback intact (${DEFINE_REQUIRED_META_FIELDS// /, } unchanged, replica_written_at recorded)"
		results_row "$CSV" define-metadata result 0 "" "" "" "" "" "PASS vmsync metadata intact after rollback"
	else
		warn "FAIL: $failures problem(s) with vmsync's metadata after the rollback -- ${details}. See $RUN_LOG"
		results_row "$CSV" define-metadata result 1 "" "" "" "" "" "FAIL ${details//,/;}"
	fi
}

stage_define_domain() {
        log "=== Stage 5: DefineDomain redefine/rollback coverage ==="
        stage_define_domain_uuid_collision
        stage_define_domain_rollback
        return 0
}

# --- Stage 6: failover (promotion, fencing, the way back) ---------------------

# The DR path had no real-life coverage at all before this stage: promotion,
# the fence a promotion arms, and the role changes that undo both are the
# highest-stakes code in vmsync and were exercised only by unit tests.
#
# Everything here is deliberately POWER-NEUTRAL and DIRECTION-NEUTRAL. It
# never stops a domain and never reverses a pair -- the target is promoted,
# inspected, and put back to `target` with a fresh sync, ending exactly where
# it started. The genuinely invasive half of the DR path (shutting the source
# down, inverting the pair) is Stage 7, separately opt-in, because those
# change state this harness otherwise never touches.
#
# NOT in the default stage list, for one specific reason: an interrupted run
# can leave the target `promoted`, and that makes every subsequent sync fail
# until somebody clears it with -update-role=target. That is a worse thing to
# leave behind than any other stage does, so it is opt-in even though it is
# no more destructive than -reinit already is.
#
# Needs vmsync ON THE TARGET HOST (TARGET_VMSYNC_BIN in bench.conf): -promote
# refuses a remote libvirt URI by design, so that a failover works when the
# other site is unreachable and needs no credentials to reach it. Without
# that setting the stage skips rather than failing.

# vmsync_on_host HOST IS_LOCAL BIN SCENARIO PHASE ARGS... -- runs vmsync on a
# specific host and records a results row, for the modes that refuse a remote
# URI and must therefore run where the domain lives.
#
# Not run_vmsync: that one always supplies -source-uri/-target-uri/-source-
# domain/-target-domain plus a prometheus textfile, which is right for a sync
# and wrong for every mode here -- -promote takes only a target, and would
# reject the source flags outright.
vmsync_on_host() {
	local host="$1" is_local="$2" bin="$3" scenario="$4" phase="$5"
	shift 5
	local log_file="$RUN_DIR/logs/${scenario}.${phase}.log"
	RUN_LOG="$log_file"

	# The helper path goes to these runs too, for the same reason it goes to
	# every other one: the harness must not leave it to vmsync's own default,
	# which is a different path from the one the preflight checked. None of the
	# modes reached through here moves data today -- -promote, -update-role and
	# the fence reads need no bridge -- so this changes nothing about what they
	# do; it means the next mode that does use the helper cannot silently pick
	# up a different binary than the one this harness verified.
	set -- -bridge-helper-path "$(bridge_helper_path)" "$@"

	log "-> $scenario/$phase (on $host)"
	log "   $bin $*"
	if [ "$DRY_RUN" = yes ]; then
		RUN_RC=0
		RUN_OUT=""
		results_row "$CSV" "$scenario" "$phase" DRYRUN 0 "" "" "" "" "dry run -- not executed"
		return 0
	fi

	set +e
	RUN_OUT="$(maybe_ssh_cmd "$is_local" "$host" "$bin" "$@" 2>&1)"
	RUN_RC=$?
	set -e
	printf '%s\n' "$RUN_OUT" >"$log_file"
	log "   exit=$RUN_RC"
	return 0
}

# vmsync_meta_field URI DOMAIN FIELD -> the value of one vmsync metadata
# field, or empty when absent.
#
# The value lives in an `id` ATTRIBUTE, not in element text -- see
# libvirtsync.buildMetadataEntry, which writes <vmsync:role id="promoted"/>.
# Matching on local-name() sidesteps whatever namespace prefix virsh chooses
# to echo back.
vmsync_meta_field() {
	local uri="$1" domain="$2" field="$3"
	virsh_uri "$uri" metadata "$domain" --uri "$VMSYNC_METADATA_URI" --config 2>/dev/null \
		| xmllint --xpath "string(//*[local-name()='${field}']/@id)" - 2>/dev/null || true
}

# replica_incomplete URI DOMAIN -> the raw replica_incomplete value, or empty.
#
# vmsync writes this on the TARGET before a full copy starts destroying the
# replica that is there, and clears it in the same write that records the copy
# as finished. Its PRESENCE is the finding, which is why every caller here
# tests for emptiness rather than for a shape -- an unreadable value still
# refuses a promotion, and a harness that only recognised well-formed ones
# would report a pass for a build that had stopped refusing.
#
# --config, like everything vmsync_meta_field reads: that is the persistent
# definition, which is what -promote reads (DOMAIN_XML_INACTIVE) and the only
# copy that outlives the process that wrote it.
replica_incomplete() {
	vmsync_meta_field "$1" "$2" replica_incomplete
}

# replica_incomplete_key VALUE KEY -> one key's value out of that line.
#
# The value is a single line of comma-separated k=v pairs
# (verb, at, action, host, aside) and this matches the key EXACTLY rather than
# by substring: an assertion about `at` that silently read `verb` instead, or
# one about `aside` that matched some later key ending in those letters, would
# be a passing check on the wrong field.
replica_incomplete_key() {
	printf '%s' "$1" | awk -v k="$2" -F, '{
		for (i = 1; i <= NF; i++) {
			n = index($i, "=")
			if (n > 0 && substr($i, 1, n - 1) == k) { print substr($i, n + 1); exit }
		}
	}'
}

# replica_incomplete_key_count VALUE KEY -> how many times KEY appears.
#
# Exists for one assertion: the field is single-valued and NEVER appended to,
# so a second `aside=` would name a second displaced set that does not exist
# and leave the refusal unable to say what to put back. Counting is the only
# way to notice, since replica_incomplete_key stops at the first match.
replica_incomplete_key_count() {
	printf '%s' "$1" | awk -v k="$2" -F, '{
		c = 0
		for (i = 1; i <= NF; i++) {
			n = index($i, "=")
			if (n > 0 && substr($i, 1, n - 1) == k) c++
		}
		print c
	}'
}

# fo_check SCENARIO LABEL OK DETAIL -- records one assertion, where OK is 0
# for pass and anything else for fail.
#
# Call sites compute OK with an explicit `if [ ... ]; then fo_ok=0; else
# fo_ok=1; fi` rather than testing and reading $? on the next line. That
# looks more verbose than it needs to be and is not: this script runs under
# `set -e`, where a bare failing `[ ... ]` is a failing command and aborts
# the whole harness -- so the obvious spelling would turn the first failed
# assertion into a silent exit instead of a recorded FAIL, which is the
# opposite of what a test stage is for.
fo_check() {
	local scenario="$1" label="$2" ok="$3" detail="${4:-}"
	if [ "$DRY_RUN" = yes ]; then
		# Nothing ran, so nothing was proven. Recording a PASS here would
		# make --dry-run report a clean failover test against hosts that
		# were never contacted, which is worse than reporting nothing.
		log "   SKIP (dry run): $label"
		results_row "$CSV" "$scenario" "${label// /_}" DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi
	if [ "$ok" = 0 ]; then
		log "   PASS: $label"
		results_row "$CSV" "$scenario" "${label// /_}" 0 "" "" "" "" "" "PASS"
	else
		warn "FAIL: $label${detail:+ -- $detail}"
		# results_row strips commas itself -- including from the label, where
		# one left in place makes a failing check disappear from the stage
		# verdict entirely. Nothing to remember here.
		results_row "$CSV" "$scenario" "${label// /_}" 1 "" "" "" "" "" "FAIL $detail"
		FAILOVER_FAILURES=$((FAILOVER_FAILURES + 1))
	fi
}

FAILOVER_FAILURES=0

# clear_target_promotion -> puts the target back to role=target. Prints
# nothing on success; returns non-zero if it did not take.
#
# Used by the cleanup path, where going through vmsync is the point: this is
# the same -update-role an operator would run, and a stage that quietly
# reset state some other way would hide that it had stopped working. The
# PRE-condition reset deliberately does not use it -- see reset_pair_state.
clear_target_promotion() {
	# Three commands, in this order, because -update-role=target is now gated
	# on the durable promotion record and a promotion writes one.
	#
	# paused first: the release refuses a domain still marked promoted, since a
	# demotion re-records the trace and releasing before demoting would be a
	# silent no-op. Then the release, then the retarget. Failures of the first
	# two are deliberately NOT fatal here -- this runs on the cleanup path
	# against a target that may be in any state, including one that was never
	# promoted at all (where both are no-ops, and the release exits 0 saying
	# there was nothing to release). What decides the outcome is the role read
	# at the end, which is the only thing later stages care about.
	ssh_host_cmd "$TARGET_HOST" "$TARGET_VMSYNC_BIN" \
		-update-role paused -target-uri qemu:///system -target-domain "$TARGET_DOMAIN" \
		>/dev/null 2>&1 || true
	ssh_host_cmd "$TARGET_HOST" "$TARGET_VMSYNC_BIN" \
		-release-promotion -target-uri qemu:///system -target-domain "$TARGET_DOMAIN" \
		>/dev/null 2>&1 || true
	ssh_host_cmd "$TARGET_HOST" "$TARGET_VMSYNC_BIN" \
		-update-role target -target-uri qemu:///system -target-domain "$TARGET_DOMAIN" \
		>/dev/null 2>&1 || return 1
	[ "$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)" = target ]
}

# retarget_promoted_target SC LABEL LOCAL_URI -- put a PROMOTED target back to
# role=target, through vmsync, with each command logged like any other run.
#
# Three commands, and the order is forced: a promotion writes a durable
# last_promoted_at, -update-role=target is refused while it stands, and
# -release-promotion refuses a domain still marked promoted (a demotion
# re-records the trace, so releasing first would silently do nothing).
#
# Its own helper rather than three lines repeated at each site, because the
# sites are all cleanup paths in stages whose real subject is something else --
# a verify failure, an interrupted rebuild -- and a stage that fails to put the
# role back does not merely fail its own sub-test. Every later sync into the
# target is refused, and several of these stages heal their replica with a sync
# from a RETURN trap, so leaving the role wrong turns one cleanup slip into a
# die() with a knowingly corrupted replica still in place.
#
# Sets RUN_RC from the last command, so a caller can check the outcome the same
# way it would check a single run.
retarget_promoted_target() {
	local sc="$1" label="$2" local_uri="$3"
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" "${label}-demote" \
		-update-role paused -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" "${label}-release" \
		-release-promotion -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" "$label" \
		-update-role target -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
}

# has_vmsync_metadata URI DOMAIN -> true when the domain carries a vmsync
# metadata block at all.
has_vmsync_metadata() {
	virsh_uri "$1" metadata "$2" --uri "$VMSYNC_METADATA_URI" --config >/dev/null 2>&1
}

# reset_pair_state STAGE -- wipes vmsync's own metadata off the domains this
# stage is about to use, so it starts from a known state.
#
# Deliberately virsh rather than `vmsync -update-role`, and that is the whole
# point of doing it here. A harness that resets state through the code it is
# testing cannot recover when that code is broken: when writing metadata
# failed outright, -update-role failed with it, every run left the target
# promoted, and the next run then failed at its baseline for reasons that
# said nothing about the real fault. virsh talks to libvirt directly and has
# no such dependency.
#
# Wiping the whole block rather than just the role is safe here and slightly
# better: these stages -reinit the target immediately, so nothing in it is
# worth keeping, it sweeps up any junk an interrupted run or a hand-run
# probe left behind, and it makes the stage's own "the sync recorded a
# replica_source" assertion prove the sync wrote it rather than that it
# happened to be there already.
#
# The source is only touched by Stage 7, which sets it paused when it fences;
# its replica_targets and last_replicated_* are rewritten by that stage's own
# baseline sync before anything reads them.
reset_pair_state() {
	local stage="$1" also_source="${2:-no}"
	[ "$DRY_RUN" = yes ] && return 0

	if has_vmsync_metadata "$TARGET_URI" "$TARGET_DOMAIN"; then
		log "   resetting $TARGET_DOMAIN's vmsync metadata on $TARGET_HOST before starting"
		virsh_uri "$TARGET_URI" metadata "$TARGET_DOMAIN" --uri "$VMSYNC_METADATA_URI" --remove --config >/dev/null 2>&1 \
			|| warn "could not clear the target's vmsync metadata; if it is still marked promoted every sync below will be refused"
	fi

	if [ "$also_source" = yes ] && has_vmsync_metadata "$SOURCE_URI" "$SOURCE_DOMAIN"; then
		log "   resetting $SOURCE_DOMAIN's vmsync metadata on $SOURCE_HOST before starting"
		virsh_uri "$SOURCE_URI" metadata "$SOURCE_DOMAIN" --uri "$VMSYNC_METADATA_URI" --remove --config >/dev/null 2>&1 \
			|| warn "could not clear the source's vmsync metadata; a leftover paused role from an interrupted run may still be there"
	fi
	return 0
}

# warn_target_still_promoted [ROLE] says what has to be done by hand, in full.
#
# Worth being explicit rather than terse: a target left with a role that
# refuses syncs refuses EVERY later one, including the next stage's baseline
# and every scheduled run in the estate, and the symptom is a sync failing for
# reasons that say nothing about the role.
#
# ROLE defaults to promoted, which is what all but one caller means. Stage 10
# passes paused: the cure is the same command, but telling an operator their
# target is "marked promoted" when it is not sends them looking for a failover
# that never happened.
warn_target_still_promoted() {
	warn "$TARGET_DOMAIN on $TARGET_HOST is still marked ${1:-promoted}. Every sync into it -- this harness's later stages included -- will be refused until that is cleared. Run these on $TARGET_HOST, in this order:  ${TARGET_VMSYNC_BIN:-vmsync} -update-role paused -target-uri qemu:///system -target-domain $TARGET_DOMAIN  &&  ${TARGET_VMSYNC_BIN:-vmsync} -release-promotion -target-uri qemu:///system -target-domain $TARGET_DOMAIN  &&  ${TARGET_VMSYNC_BIN:-vmsync} -update-role target -target-uri qemu:///system -target-domain $TARGET_DOMAIN"
	warn "the middle command is why there are three: a copy that has been promoted keeps a durable last_promoted_at, and -update-role=target is refused while it stands. The demotion first, because the release refuses a domain still marked promoted."
	warn "if $TARGET_DOMAIN is also RUNNING, stop it before any of the three: vmsync refuses to record a running promoted domain as anything but source, and -release-promotion refuses a running domain outright. No bench stage starts a promoted target, so this should not happen -- if it has, something started it."
}

# require_target_syncable -- confirms the reset actually took.
#
# reset_pair_state removes the metadata; this checks the result, because a
# target still carrying `promoted` or `source` refuses every sync below and
# the failure would otherwise surface three minutes into a full copy, as a
# role error that says nothing about the stage that left it there.
require_target_syncable() {
	local stage="$1" role
	[ "$DRY_RUN" = yes ] && return 0
	role="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
	case "$role" in
	# paused is here because Stage 10 can create it: a restore deliberately
	# pauses the replica it rolled back. It heals on the way out, but a run
	# interrupted mid-stage leaves it set, and a sync into a paused target is
	# refused exactly as one into a promoted target is.
	promoted | source | paused)
		warn "$stage: the target's replication_role is still '$role' after resetting it, so every sync into it would be refused -- skipping."
		warn_target_still_promoted "$role"
		results_row "$CSV" "$stage" skipped 0 "" "" "" "" "" "SKIPPED target role is $role"
		return 1
		;;
	esac

	# The role is not the only thing that refuses a sync any more. A copy that
	# has been promoted keeps last_promoted_at through every role change, and
	# the sync gate reads it -- so a leftover trace from an interrupted stage
	# refuses the baseline below with a message about production data, three
	# minutes into a full copy, on a domain whose role reads perfectly fine.
	#
	# reset_pair_state removes the whole metadata block so this should never
	# fire; it is here because "should never" is exactly the class of thing that
	# cost a debugging session the last time a new field was added to the
	# refusals and not to this check.
	local served
	served="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_promoted_at)"
	if [ -n "$served" ]; then
		warn "$stage: the target still carries last_promoted_at='$served' after resetting it, so every sync into it would be refused as an overwrite of data that served live -- skipping."
		warn "clear it on $TARGET_HOST with:  ${TARGET_VMSYNC_BIN:-vmsync} -release-promotion -target-uri qemu:///system -target-domain $TARGET_DOMAIN"
		results_row "$CSV" "$stage" skipped 0 "" "" "" "" "" "SKIPPED target carries last_promoted_at"
		return 1
	fi
	return 0
}

# target_disk_owner -> "user:group" of the target domain's disk, or empty.
target_disk_owner() {
	local path
	path="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
	[ -n "$path" ] || return 0
	ssh_host_cmd "$TARGET_HOST" stat -c %U:%G "$path" 2>/dev/null | tr -d '[:space:]' || true
}

# expected_qemu_owner -> the "user:group" the target host's libvirt would run
# qemu as, by the same rule vmsync itself uses.
#
# Reimplemented here rather than read out of vmsync's log on purpose: a test
# that asked the thing under test what the right answer was would pass just as
# happily when both were wrong together.
expected_qemu_owner() {
	local u g
	for u in qemu libvirt-qemu; do
		if ssh_host_cmd "$TARGET_HOST" getent passwd "$u" >/dev/null 2>&1; then
			g=qemu
			[ "$u" = libvirt-qemu ] && g=kvm
			if ssh_host_cmd "$TARGET_HOST" getent group "$g" >/dev/null 2>&1; then
				printf '%s:%s' "$u" "$g"
			else
				printf '%s:' "$u"
			fi
			return 0
		fi
	done
	return 0
}

# stage_failover_disk_owner asserts the three ways a target disk can end up
# with the right owner, each of which only happens when a file is CREATED --
# hence a full sync per property.
stage_failover_disk_owner() {
	local sc="$1"
	if [ "$DRY_RUN" = yes ]; then
		# Every check here reads real state back off the target host, so
		# there is nothing meaningful to print for a dry run -- but saying so
		# beats a preview that silently omits two full syncs.
		log "   (dry run: the disk-ownership checks read live state and run two extra -reinit syncs, so they do nothing here)"
		return 0
	fi

	local expected expected_user expected_group
	expected="$(expected_qemu_owner)"
	expected_user="${expected%%:*}"
	expected_group="${expected#*:}"

	if [ -z "$expected_user" ]; then
		warn "the target host has no qemu or libvirt-qemu account, so there is no way to say what SHOULD own its disks -- skipping the ownership checks"
		results_row "$CSV" "$sc" disk_ownership 0 "" "" "" "" "" "SKIP no known qemu account on the target"
		return 0
	fi
	log "   target host runs qemu as '$expected_user' -- checking disk ownership against that"

	# 1. A first-ever sync. The baseline above created these files from
	#    scratch, which is the case that was broken: nothing preserved,
	#    nothing configured, and every distribution ships qemu.conf with the
	#    setting commented out.
	local owner user
	owner="$(target_disk_owner)"
	user="${owner%%:*}"
	if [ "$user" = "$expected_user" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "a fresh sync leaves the disk owned by the qemu user" "$fo_ok" \
		"disk is '$owner' want user '$expected_user'"

	# Stated separately because root is the specific signature of the bug,
	# and a failure saying so is more use than one saying "not qemu".
	if [ "$user" != root ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the disk is not left owned by the SSH user vmsync ran as" "$fo_ok" \
		"disk is '$owner' -- a root-owned disk is one the promoted domain cannot open"

	if [ -z "$expected_group" ]; then
		log "   (skipping the preserve/override checks: no group to use as a sentinel)"
		return 0
	fi

	# 2. -reinit must PRESERVE ownership. This is the sharper half of the
	#    bug: reinit renames the correctly-owned disk aside and creates a
	#    fresh root-owned one, silently turning a bootable replica into one
	#    qemu cannot open.
	#
	#    The sentinel is the GROUP, set to root while leaving the user alone.
	#    That is deliberately harmless -- the disk stays openable by its
	#    owning user throughout, so an interrupted run never leaves an
	#    unbootable replica behind -- while still being distinguishable from
	#    what detection alone would produce.
	local path
	path="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
	if [ -z "$path" ]; then
		warn "could not resolve the target disk path -- skipping the reinit ownership checks"
		return 0
	fi
	if ! ssh_host_cmd "$TARGET_HOST" chown "${expected_user}:root" "$path"; then
		warn "could not set the sentinel ownership -- skipping the reinit ownership checks"
		return 0
	fi

	bench_sync "$sc" owner-preserve -reinit
	owner="$(target_disk_owner)"
	if [ "$owner" = "${expected_user}:root" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "-reinit preserves the ownership it replaces" "$fo_ok" \
		"disk is '$owner' want '${expected_user}:root' -- a reinit that resets ownership silently breaks a working replica"

	# 3. An explicit -target-disk-owner overrides what was preserved. Runs
	#    last so it also puts the ownership back where it belongs, whatever
	#    the checks above found.
	bench_sync "$sc" owner-explicit -reinit -target-disk-owner "$expected"
	owner="$(target_disk_owner)"
	if [ "$owner" = "$expected" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "an explicit -target-disk-owner overrides the preserved one" "$fo_ok" \
		"disk is '$owner' want '$expected'"

	# Whatever happened above, do not leave the sentinel behind.
	if [ "$owner" != "$expected" ]; then
		warn "restoring the target disk's ownership to $expected by hand after a failed check"
		ssh_host_cmd "$TARGET_HOST" chown "$expected" "$path" \
			|| warn "could not restore ownership on $path -- fix it before promoting this replica"
	fi
	return 0
}

stage_failover() {
	log "=== Stage 6: failover -- promotion, fencing, and the way back ==="

	if [ -z "${TARGET_VMSYNC_BIN:-}" ]; then
		warn "TARGET_VMSYNC_BIN is not set in $CONF -- skipping stage 6. -promote must run ON the target host (it refuses a remote libvirt URI by design), so this stage needs to know where vmsync lives there."
		results_row "$CSV" failover skipped 0 "" "" "" "" "" "SKIPPED TARGET_VMSYNC_BIN unset"
		return 0
	fi

	local sc=failover
	# -promote and -update-role act on the host they run on, so the target
	# host is addressed with its own LOCAL uri, never TARGET_URI.
	local src_ref local_uri="qemu:///system"

	if [ "$DRY_RUN" != yes ]; then
		stage_needs_target_shutoff "$CSV" "$sc" "stage failover" || return 0
		reset_pair_state "$sc"
		require_target_syncable "$sc" || return 0
	fi

	# The backstop for everything below. This stage promotes the target and
	# is responsible for putting it back; a die, a Ctrl+C or a failed way
	# back would otherwise leave it promoted, and a promoted target refuses
	# every sync in the estate -- not just this harness's later stages.
	#
	# An EXIT trap rather than RETURN for the same reason Stage 7 uses one:
	# RETURN does not fire on die or on a signal, and those are precisely the
	# cases that would leave it behind.
	FAILOVER_PROMOTED=no
	FAILOVER_CLEANED=no
	failover_cleanup() {
		[ "$FAILOVER_CLEANED" = yes ] && return 0
		FAILOVER_CLEANED=yes
		[ "$FAILOVER_PROMOTED" = yes ] || return 0
		if clear_target_promotion; then
			log "stage 6: the target is back to role=target"
		else
			warn_target_still_promoted
		fi
	}
	trap 'failover_cleanup' EXIT

	# --- baseline: a real replica to promote --------------------------------
	bench_sync "$sc" baseline -reinit
	if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
		warn "baseline full sync failed (see $RUN_LOG) -- aborting stage 6 before anything is promoted$(bench_sync_hint)"
		return 1
	fi

	# replica_source is what a bare -fence-source resolves the fence against,
	# so read it once and assert every later reference against THIS rather
	# than against a hostname reconstructed here. It also catches a real
	# regression directly: this field was once written as "127.0.0.1:<vm>",
	# which names every machine and therefore none.
	if [ "$DRY_RUN" != yes ]; then
		src_ref="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replica_source)"
		if [ -n "$src_ref" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the sync recorded a replica_source on the target" "$fo_ok" "got '$src_ref'"
		case "$src_ref" in
		127.0.0.1:* | localhost:*)
			fo_check "$sc" "replica_source names a real host rather than loopback" 1 "got '$src_ref'"
			;;
		*)
			fo_check "$sc" "replica_source names a real host rather than loopback" 0
			;;
		esac

		# The first half of the interrupted-rebuild false-positive guard. The
		# baseline above was a -reinit: it renamed the replica aside, wrote new
		# bases and armed replica_incomplete before it started, so a field
		# still standing here means the clear did not happen -- and everything
		# below would then be testing a promotion of a replica vmsync believes
		# is half-written, which is stage 16's subject and not this one's.
		local incomplete
		incomplete="$(replica_incomplete "$TARGET_URI" "$TARGET_DOMAIN")"
		if [ -z "$incomplete" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "a healthy replica carries no replica_incomplete" "$fo_ok" \
			"the freshly synced target still says replica_incomplete='$incomplete'"
	fi

	# --- disk ownership -----------------------------------------------------
	# The check that would have caught the bug this stage's neighbours were
	# written after: vmsync creates the target's disks by running qemu-img
	# over SSH, so they belong to that SSH user -- root. qemu does not run as
	# root, so a root-owned disk is one the PROMOTED domain cannot open, and
	# that is discovered during a failover on the copy meant to take over.
	#
	# Costs two extra full copies, because each property under test only
	# happens when a disk file is created from scratch.
	stage_failover_disk_owner "$sc"

	# --- promote, WITHOUT arming a fence ------------------------------------
	# The drill case, and the single most important safety property in the
	# fencing design: a promotion that was not asked to arm a fence must
	# authorise nothing at all.
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" promote \
		-promote -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN" \
		-promote-mode planned -promoted-by bench-harness
	if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "promote succeeds against a freshly synced replica" "$fo_ok" "exit $RUN_RC see $RUN_LOG"
	if [ "$fo_ok" = 0 ]; then FAILOVER_PROMOTED=yes; fi

	# The other half of the interrupted-rebuild guard, and the one that names
	# the cause. A refusal firing on a healthy replica shows up in the check
	# above only as "promote failed", with the reason buried in a log nobody
	# opens until the failover is already going badly. This refusal is the one
	# most able to fire wrongly, because it rests on a field being ABSENT
	# rather than on a value being right: anything that armed it and did not
	# clear it -- a strip list a new field was never added to, a clear that
	# moved after the write that was supposed to contain it -- turns every
	# promotion in the estate into a -force-promote.
	#
	# Its own variable, not fo_ok, which the early return below still needs.
	local fo_incomplete=0
	if grep -q 'a full copy of this replica was STARTED and never recorded as finished' "$RUN_LOG" 2>/dev/null; then fo_incomplete=1; fi
	fo_check "$sc" "the ordinary promotion is not refused over an interrupted rebuild" "$fo_incomplete" \
		"-promote named an interrupted rebuild on a replica the baseline sync finished moments ago -- see $RUN_LOG"

	# Everything below this point only means anything against a target that
	# is actually promoted. Running those checks anyway turns ONE root cause
	# into a wall of failures -- and worse, some of them PASS vacuously: "a
	# promotion with no -fence-source arms nothing" is trivially true when no
	# promotion happened at all, which reads as reassurance about a property
	# that was never exercised.
	if [ "$fo_ok" != 0 ] && [ "$DRY_RUN" != yes ]; then
		warn "the promotion failed, so every check that depends on a promoted target is skipped rather than reported as a separate failure -- fix that first, the rest of this stage cannot say anything until it works"
		results_row "$CSV" "$sc" promotion_dependent_checks 0 "" "" "" "" "" "SKIP promote failed"
		# Nothing was changed, so there is nothing to put back: the target
		# is still the healthy replica the baseline left behind.
		return 0
	fi

	if [ "$DRY_RUN" != yes ]; then
		local role promoted_at promoted_from fence_src fence_id
		role="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
		if [ "$role" = promoted ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the target records role=promoted" "$fo_ok" "got '$role'"

		promoted_at="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" promoted_at)"
		if [ -n "$promoted_at" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the promotion is timestamped" "$fo_ok" "promoted_at empty"

		promoted_from="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" promoted_from)"
		if [ "$promoted_from" = "$src_ref" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "promoted_from names the source the replica came from" "$fo_ok" \
			"got '$promoted_from' want '$src_ref'"

		# The durable half, written by the promotion itself and in the same
		# metadata call. Asserted separately from promoted_at because the two
		# have opposite lifetimes and the whole design rests on that: this one
		# is the only field that will still be here after the role changes, so
		# a promotion that failed to write it leaves nothing at all protecting
		# these disks once the copy is shut down.
		local served_live
		served_live="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_promoted_at)"
		if [ -n "$served_live" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the promotion records that this copy has served live" "$fo_ok" \
			"last_promoted_at empty -- nothing will refuse a sync, restore or force-clean over this copy once its role changes"

		if [ "$served_live" = "$promoted_at" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the durable record and the promotion agree on the time" "$fo_ok" \
			"last_promoted_at='$served_live' promoted_at='$promoted_at' -- written from one clock reading in one call, so they cannot disagree"

		fence_src="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" fence_source)"
		if [ -z "$fence_src" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "a promotion with no -fence-source arms NOTHING" "$fo_ok" \
			"fence_source is '$fence_src' -- a DR drill must not authorise stopping production"
	fi

	# --- a promoted target refuses to be synced into ------------------------
	# The backstop under the whole design. Nothing else in this stage matters
	# if a scheduled sync can still overwrite a domain that is serving live.
	bench_sync "$sc" refuse-sync
	if [ "$DRY_RUN" != yes ]; then
		if [ "$RUN_RC" != 0 ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "syncing into a promoted target is refused" "$fo_ok" \
			"vmsync exited 0 -- see $RUN_LOG"
	fi

	# --- read-fence: reachable, promoted, and NOT fenced --------------------
	# -read-fence is the one failover mode that takes a remote URI, so it runs
	# from here rather than on the target.
	vmsync_on_host "$SOURCE_HOST" "$SOURCE_LOCAL" "$VMSYNC_BIN" "$sc" read-fence-unarmed \
		-read-fence -target-uri "$TARGET_URI" -target-domain "$TARGET_DOMAIN"
	if [ "$DRY_RUN" != yes ]; then
		local reachable trole fid
		if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "-read-fence exits 0 against a reachable peer" "$fo_ok" "exit $RUN_RC"

		reachable="$(json_bool "$RUN_OUT" reachable)"
		if [ "$reachable" = true ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "-read-fence reports the peer as reachable" "$fo_ok" "got '$reachable' from: $RUN_OUT"

		trole="$(json_str "$RUN_OUT" target_role)"
		if [ "$trole" = promoted ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "-read-fence reports the peer's role" "$fo_ok" "got '$trole'"

		fid="$(json_str "$RUN_OUT" id)"
		if [ -z "$fid" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "-read-fence reports no fence when none was armed" "$fo_ok" "got fence id '$fid'"
	fi

	# --- arm a fence on the ALREADY-promoted domain -------------------------
	# The recovery path: promote, notice the old source is still serving, then
	# arm. Re-running -promote must arm the fence without rewriting the
	# original promotion record.
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" arm-fence \
		-promote -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN" \
		-fence-source -promoted-by bench-harness
	if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "a fence can be armed on an already-promoted domain" "$fo_ok" "exit $RUN_RC see $RUN_LOG"

	if [ "$DRY_RUN" != yes ]; then
		local fence_src2 fence_id2 promoted_at2
		fence_src2="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" fence_source)"
		if [ "$fence_src2" = "$src_ref" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the fence names the recorded source" "$fo_ok" \
			"got '$fence_src2' want '$src_ref'"

		fence_id2="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" fence_id)"
		if [ -n "$fence_id2" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the fence has an id, which is what makes it single-use" "$fo_ok" "fence_id empty"

		promoted_at2="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" promoted_at)"
		if [ "$promoted_at2" = "$promoted_at" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "arming a fence leaves the original promotion record alone" "$fo_ok" \
			"promoted_at changed from '$promoted_at' to '$promoted_at2'"

		# And the token must be readable from the other side, which is how the
		# displaced source actually learns about it.
		vmsync_on_host "$SOURCE_HOST" "$SOURCE_LOCAL" "$VMSYNC_BIN" "$sc" read-fence-armed \
			-read-fence -target-uri "$TARGET_URI" -target-domain "$TARGET_DOMAIN"
		local rid rsrc
		rid="$(json_str "$RUN_OUT" id)"
		if [ "$rid" = "$fence_id2" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "-read-fence reports the armed fence id" "$fo_ok" "got '$rid' want '$fence_id2'"

		rsrc="$(json_str "$RUN_OUT" source)"
		if [ "$rsrc" = "$src_ref" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "-read-fence reports who the fence names" "$fo_ok" "got '$rsrc' want '$src_ref'"
	fi

	# --- an unreachable peer is NOT an absence of fencing -------------------
	# Load-bearing: a partition is exactly when a promotion is most likely to
	# have happened and least likely to be visible, so "could not ask" must
	# never read as "nothing is armed".
	if [ "$DRY_RUN" != yes ]; then
		vmsync_on_host "$SOURCE_HOST" "$SOURCE_LOCAL" "$VMSYNC_BIN" "$sc" read-fence-unreachable \
			-read-fence -target-uri "qemu+ssh://vmsync-bench-nonexistent.invalid/system" \
			-target-domain "$TARGET_DOMAIN"
		if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "-read-fence exits 0 for an unreachable peer" "$fo_ok" \
			"exit $RUN_RC -- an unreachable peer is an answer, not a broken invocation"

		local unreachable
		unreachable="$(json_bool "$RUN_OUT" reachable)"
		if [ "$unreachable" = false ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "an unreachable peer reports reachable=false" "$fo_ok" \
			"got '$unreachable' -- silence must never read as 'no fence armed'"
	fi

	# --- the local-URI guard ------------------------------------------------
	# -promote and -shutdown-domain must refuse a remote URI, which is what
	# keeps a failover working when the other site is unreachable. Checked
	# from here because it is rejected before anything is touched.
	case "$TARGET_URI" in
	*+ssh://*)
		if [ "$DRY_RUN" != yes ]; then
			vmsync_on_host "$SOURCE_HOST" "$SOURCE_LOCAL" "$VMSYNC_BIN" "$sc" refuse-remote-uri \
				-shutdown-domain -target-uri "$TARGET_URI" -target-domain "$TARGET_DOMAIN"
			if [ "$RUN_RC" != 0 ]; then fo_ok=0; else fo_ok=1; fi
			fo_check "$sc" "-shutdown-domain refuses a remote libvirt URI" "$fo_ok" "exit $RUN_RC"
		fi
		;;
	*)
		log "   (skipping the remote-URI guard: TARGET_URI is not a +ssh one)"
		;;
	esac

	# --- the way back -------------------------------------------------------
	# -update-role=target is the documented remedy for an unwanted promotion,
	# and it must take the promotion record and the fence with it: a domain
	# carrying role=target alongside a live fence_source would be a token
	# authorising a shutdown that nothing justifies.
	#
	# It is now also gated, and the order below is the whole mechanism in
	# sequence: the release is refused before the demotion, the demotion keeps
	# the durable record, -update-role=target is refused while that record
	# stands, the release then clears it, and only then does the way back open.
	# Every step asserts the refusal AND the recovery -- a gate proven only by
	# its refusals is a gate that might be a lockout.

	# 1. The release refuses to run before the demotion. Not pedantry: a
	#    demotion re-records the trace from promoted_at, so releasing first
	#    and demoting second would silently have no effect, and an operator
	#    would conclude the flag does not work.
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" release-before-demote \
		-release-promotion -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
	if [ "$RUN_RC" != 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "-release-promotion refuses a domain still marked promoted" "$fo_ok" \
		"exit $RUN_RC -- a demotion re-records the trace, so releasing first would be a no-op that looks like success"

	# 2. Demote to paused, which is exactly what the console's "Shut down
	#    cleanly" does, and the state CI-09 was about. The record must SURVIVE
	#    it -- that survival is the entire point of the field, and it is the
	#    one assertion in this stage that no other check can substitute for.
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" demote-to-paused \
		-update-role paused -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
	if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "-update-role=paused succeeds on a promoted copy" "$fo_ok" "exit $RUN_RC see $RUN_LOG"

	if [ "$DRY_RUN" != yes ]; then
		local paused_trace paused_promoted_at
		paused_trace="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_promoted_at)"
		if [ -n "$paused_trace" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "demoting a promoted copy KEEPS the record that it served live" "$fo_ok" \
			"last_promoted_at is empty after -update-role=paused -- every trace of the failover is then gone, and a restore or force-clean over these disks is refused by nothing"

		# And the present-tense record does go, because a domain marked paused
		# beside a promoted_at describes a failover still in force.
		paused_promoted_at="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" promoted_at)"
		if [ -z "$paused_promoted_at" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "demoting still takes the present-tense promotion record" "$fo_ok" \
			"promoted_at is still '$paused_promoted_at'"
	fi

	# 3. The gate itself: the one act that makes this copy syncable again is
	#    refused while the record stands. This is the step every unattended
	#    path is behind -- a scheduled sync and -reinit-after-failures both
	#    need role=target, and neither can reach it on its own.
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" retarget-refused \
		-update-role target -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
	if [ "$RUN_RC" != 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "-update-role=target is refused on a copy that has served live" "$fo_ok" \
		"exit $RUN_RC -- without this refusal the next scheduled sync overwrites a copy that held production data, unattended"

	# 4. The release, which is the one override and has to be typed. After it,
	#    the way back must open -- a gate with no key is a lockout, and the
	#    documented alternative (-invert) is not available to an operator who
	#    has decided the data is disposable.
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" release-promotion \
		-release-promotion -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
	if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "-release-promotion succeeds on a demoted copy" "$fo_ok" "exit $RUN_RC see $RUN_LOG"

	if [ "$DRY_RUN" != yes ]; then
		local released_trace
		released_trace="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_promoted_at)"
		if [ -z "$released_trace" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the release clears the record" "$fo_ok" \
			"last_promoted_at is still '$released_trace', so the release did nothing and the pair is wedged"
	fi

	# 5. And now the way back works, which is what the whole sequence is for.
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" update-role-back \
		-update-role target -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
	if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "-update-role=target succeeds once the promotion is released" "$fo_ok" "exit $RUN_RC see $RUN_LOG"

	if [ "$DRY_RUN" != yes ]; then
		local role_back fence_back promoted_back trace_back
		role_back="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
		if [ "$role_back" = target ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the target is a target again" "$fo_ok" "got '$role_back'"

		# Both were cleared by the demotion in step 2 above, which is where
		# they are asserted. Re-read here anyway, because these are the two
		# fields whose survival would be worst and the demotion is not the only
		# write between then and now: the release and this -update-role have
		# both touched the metadata since, and a merge that resurrected either
		# would leave a domain marked target carrying a token that authorises
		# shutting its source down.
		fence_back="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" fence_source)"
		if [ -z "$fence_back" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the fence is still gone at the end of the way back" "$fo_ok" \
			"fence_source is '$fence_back' on a domain marked target"

		promoted_back="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" promoted_at)"
		if [ -z "$promoted_back" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the promotion record is still gone at the end of the way back" "$fo_ok" \
			"promoted_at is '$promoted_back' on a domain marked target"

		trace_back="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_promoted_at)"
		if [ -z "$trace_back" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the released record does not come back" "$fo_ok" \
			"last_promoted_at is '$trace_back' again -- a domain marked target carrying it refuses its own syncs for ever"

		# Recording the failure is not enough. A target left promoted refuses
		# every later sync, so a stage that merely noted it and moved on would
		# hand the next stage a baseline failure with nothing pointing back
		# here -- which is exactly what happened when -update-role could not
		# write metadata at all.
		if [ "$role_back" != target ]; then
			warn "the way back did not take, so this stage is leaving the pair unusable rather than as it found it"
			warn_target_still_promoted
		fi
	fi

	# --- and replication actually resumes -----------------------------------
	# The point of the way back. A role that clears but leaves the pair broken
	# would be a worse outcome than not clearing at all.
	bench_sync "$sc" resync
	if [ "$DRY_RUN" != yes ]; then
		if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "replication resumes once the promotion is undone" "$fo_ok" \
			"exit $RUN_RC see $RUN_LOG"
	fi

	if [ "$DRY_RUN" != yes ]; then
		if [ "$FAILOVER_FAILURES" -eq 0 ]; then
			log "=== Stage 6: all failover assertions passed ==="
		else
			warn "=== Stage 6: $FAILOVER_FAILURES failover assertion(s) FAILED -- see the report and logs/ ==="
		fi
	fi

	# Cleaned up above by the way-back step, so this only has to confirm it
	# and stand the trap down -- leaving it armed would fire again at script
	# exit, after the report had already been printed.
	failover_cleanup
	trap - EXIT
	return 0
}

# --- Stage 7: fencing end to end, with real agents ----------------------------

# Stage 6 proves the fence TOKEN is written and readable. This proves it is
# ACTED ON: a real vmsync-agent on the source host reads the token from the
# promoted peer's own libvirt and shuts its copy down.
#
# THIS STOPS THE SOURCE VM. Every other stage in this harness deliberately
# leaves the source's power state alone -- this one cannot, because a fence
# only ever acts on a RUNNING domain, and a fence that never fires proves
# nothing. It restores the source afterwards (role and power state both), but
# a crash mid-stage leaves the source shut off and `paused`.
#
# The agents run in --standalone mode, which needs no control plane, no
# enrolment and no credential. Their schedule entry is deliberately DISABLED:
# no syncs run, and the fence still fires -- which is the design property
# being demonstrated, since a displaced source is very often one whose
# replication was already switched off.
#
# An agent is started on BOTH hosts, and the target's has a real job: it must
# NOT fence the promoted domain. sweepFences skips anything whose role is not
# source, and a bug there would stop the copy that just took over.

# agent_standalone_config VM -> the JSON for a --standalone agent that
# schedules nothing and exists only to run its fence loop.
agent_standalone_config() {
	# "config_version" is REQUIRED in a hand-written schedule: the standalone
	# decoder is strict, so a file without one is refused rather than assumed
	# to be current.
	printf '{\n  "config_version": 1,\n  "report_interval_seconds": 60,\n  "poll_wait_seconds": 30,\n  "schedule": [\n    { "vm": "%s", "interval_seconds": 86400, "enabled": false, "profile": {} }\n  ]\n}\n' "$1"
}

# agent_local_config STATE_DIR VMSYNC_BIN -> the agent's own settings file.
#
# The agent takes no configuration flags any more: everything except which
# file to read now lives in this document. Mode is the ABSENCE of a
# "control_plane" block, which is what makes this a standalone agent -- there
# is no --standalone flag to pass and no way for the two to disagree.
#
# prometheus_dir is set to the same scratch directory, which is what makes the
# agent write metrics at all: the loop that produces them is gated on this being
# non-empty (cmd/vmsync-agent/main.go), so without it a stage cannot assert on
# anything the agent publishes -- and the split-brain and fenced-running gauges
# are the only place some conditions are ever reported.
agent_local_config() {
	printf '{\n  "config_version": 1,\n  "state_dir": "%s",\n  "vmsync_path": "%s",\n  "schedule_file": "%s/schedule.json",\n  "prometheus_dir": "%s",\n  "log": { "debug": true }\n}\n' "$1" "$2" "$1" "$1"
}

# agent_start HOST IS_LOCAL AGENT_BIN VMSYNC_BIN_THERE VM WORKDIR -> prints
# the agent's PID.
#
# VMSYNC_BIN_THERE is passed explicitly rather than read from an outer
# variable: the agent shells out to vmsync on ITS OWN host, so the source and
# target agents need different paths, and relying on bash's dynamic scoping
# to carry that in would be a trap for whoever edits this next.
agent_start() {
	local host="$1" is_local="$2" bin="$3" vmsync_there="$4" vm="$5" dir="$6"
	agent_standalone_config "$vm" | run_shell_on "$host" "$is_local" \
		"mkdir -p '$dir' && cat > '$dir/schedule.json'"
	# The agent's own settings are a second file now, not flags.
	agent_local_config "$dir" "$vmsync_there" | run_shell_on "$host" "$is_local" \
		"cat > '$dir/agent.json'"
	# setsid so the agent survives this ssh session closing, which it
	# otherwise would not: without it the remote shell's exit takes the
	# whole process group with it and the fence sweep never happens.
	run_shell_on "$host" "$is_local" \
		"setsid nohup '$bin' --standalone --config '$dir/agent.json' >'$dir/agent.log' 2>&1 < /dev/null & echo \$!"
}

agent_stop() {
	local host="$1" is_local="$2" pid="$3" dir="$4"
	[ -n "$pid" ] || return 0
	# TERM, not KILL: the agent unwinds its loops on SIGTERM, and a KILL
	# mid-shutdown is exactly the crash whose ledger handling this stage is
	# not trying to test.
	run_shell_on "$host" "$is_local" "kill $pid 2>/dev/null || true" || true
}

stage_fence_agent() {
	log "=== Stage 7: fencing end to end, with real agents ==="

	if [ -z "${TARGET_VMSYNC_BIN:-}" ] || [ -z "${SOURCE_AGENT_BIN:-}" ]; then
		warn "TARGET_VMSYNC_BIN and SOURCE_AGENT_BIN must both be set in $CONF -- skipping stage 7. This stage needs vmsync on the target host (to promote) and vmsync-agent on the source host (to be fenced)."
		results_row "$CSV" fence-agent skipped 0 "" "" "" "" "" "SKIPPED binaries unset"
		return 0
	fi

	local sc=fence-agent
	# Both -promote and -update-role act on the host they run on, so both
	# ends use a LOCAL uri -- that restriction is the whole reason a failover
	# needs no credentials to reach the site it is failing away from.
	local local_uri="qemu:///system"
	local agent_dir="${AGENT_WORK_DIR:-/var/tmp/vmsync-bench-agent}"
	local src_pid="" tgt_pid=""
	# Where the source's agent finds vmsync on its own host. VMSYNC_BIN, because
	# this stage already requires SOURCE_LOCAL=yes -- the harness is running on
	# the source, so its own binary is the source's binary. A SOURCE_VMSYNC_BIN
	# override defaulting to this would be one more path to keep in step and
	# never a different value in any working configuration.
	local src_vmsync="$VMSYNC_BIN"

	if [ "$DRY_RUN" = yes ]; then
		log "   (dry run: stage 7 starts real background agents and stops the source VM, so it does nothing here)"
		results_row "$CSV" "$sc" skipped DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi

	# Before the power check, so a run that skips below still leaves the pair
	# clean. A killed Stage 7 leaves the source both shut off AND paused, and
	# clearing the role here means the operator only has to start it.
	#
	# The source's role matters more here than it looks: a fence sweep skips
	# any domain whose role is not `source`, so a leftover `paused` would
	# make the fence silently never fire and this stage fail its central
	# assertion for a reason that has nothing to do with fencing.
	reset_pair_state "$sc" yes

	# A fence only ever acts on a RUNNING domain. Skipping rather than
	# starting the source ourselves, the same way Stage 4 does.
	local src_state
	src_state="$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN")" \
		|| { warn "cannot query the source domain's state${VIRSH_ERR:+: $VIRSH_ERR}"; return 1; }
	if [ "$src_state" != running ]; then
		warn "source domain '$SOURCE_DOMAIN' is '$src_state', not running -- skipping stage 7. A fence only acts on a running domain, and this harness does not start the source itself."
		results_row "$CSV" "$sc" skipped 0 "" "" "" "" "" "SKIPPED source not running"
		return 0
	fi

	stage_needs_target_shutoff "$CSV" "$sc" "stage fence-agent" || return 0
	require_target_syncable "$sc" || return 0

	# Whatever happens below, put the pair back: kill the agents, clear both
	# roles, start the source again.
	#
	# Registered as an EXIT trap, not a RETURN one, and guarded so it runs at
	# most once. RETURN would not fire on `die` (which exits) or on Ctrl+C --
	# and those are precisely the cases that would otherwise leave the source
	# shut off and `paused`, with every later sync refused and nothing on
	# screen saying why.
	FENCE_AGENT_CLEANED=no
	fence_agent_cleanup() {
		[ "$FENCE_AGENT_CLEANED" = yes ] && return 0
		FENCE_AGENT_CLEANED=yes
		log "stage 7: restoring the pair"
		agent_stop "$SOURCE_HOST" "$SOURCE_LOCAL" "$src_pid" "$agent_dir"
		agent_stop "$TARGET_HOST" no "$tgt_pid" "$agent_dir"

		# This stage PROMOTED the target, and a promotion boots it. Nothing
		# here started it deliberately, so it is easy to forget -- but every
		# stage that syncs into the target refuses to run while it is up, and
		# that refusal is a die(), which ends the whole run without a report.
		# So "restoring the pair" has to mean both halves, not just the source
		# this stage fenced.
		#
		# virsh rather than `vmsync -shutdown-domain`, for the reason
		# reset_pair_state gives: a harness that restores state through the
		# code under test cannot recover when that code is broken. It would
		# also mark replication paused, which the role reset below would then
		# have to undo.
		local tgt_now waited=0
		tgt_now="$(dom_state "$TARGET_URI" "$TARGET_DOMAIN" 2>/dev/null || true)"
		if [ "$tgt_now" = running ]; then
			# This domain was BOOTED by the promotion moments ago, which is
			# exactly the case a single fire-and-forget ACPI request cannot
			# handle -- see graceful_shutdown, which re-sends.
			log "   shutting the promoted target down again (this stage started it by promoting it)"
			if ! graceful_shutdown "$TARGET_URI" "$TARGET_DOMAIN" \
				"${TARGET_SHUTDOWN_WAIT_SECONDS:-120}" "the promoted target"; then
				warn "the promoted target did not shut down gracefully within ${SHUTDOWN_WAITED}s despite ${SHUTDOWN_ATTEMPTS} shutdown request(s) -- destroying it. It is a disposable replica and the next stage reinitialises it anyway, but leaving it running would make every later stage refuse to sync. Raise TARGET_SHUTDOWN_WAIT_SECONDS if this guest is legitimately slow to stop."
				virsh_uri "$TARGET_URI" destroy "$TARGET_DOMAIN" >/dev/null 2>&1 || true
			fi
			tgt_now="$(dom_state "$TARGET_URI" "$TARGET_DOMAIN" 2>/dev/null || true)"
		fi

		maybe_ssh_cmd "$SOURCE_LOCAL" "$SOURCE_HOST" "$src_vmsync" \
			-update-role none -target-uri "$local_uri" -target-domain "$SOURCE_DOMAIN" \
			>/dev/null 2>&1 \
			|| warn "could not clear the source's replication role -- do it by hand: vmsync -update-role none -target-uri $local_uri -target-domain $SOURCE_DOMAIN"
		# Through clear_target_promotion rather than a bare -update-role: this
		# target was promoted, so it carries the durable last_promoted_at that
		# -update-role=target is now refused on. That helper does the demote,
		# the release and the retarget in the one order that works.
		clear_target_promotion \
			|| warn "could not put the target back to role=target -- do it by hand, or every later sync into it will be refused; see the three commands warn_target_still_promoted prints"

		if [ "$src_state" = running ]; then
			local now_state
			now_state="$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN" 2>/dev/null || true)"
			if [ "$now_state" != running ]; then
				log "   starting the source domain again (this stage shut it down)"
				virsh_uri "$SOURCE_URI" start "$SOURCE_DOMAIN" >/dev/null 2>&1 \
					|| warn "could not start '$SOURCE_DOMAIN' again -- start it by hand"
			fi
		fi
	}
	trap 'fence_agent_cleanup' EXIT

	# --- baseline ------------------------------------------------------------
	bench_sync "$sc" baseline -reinit
	if [ "$RUN_RC" != 0 ]; then
		warn "baseline full sync failed (see $RUN_LOG) -- aborting stage 7 before anything is promoted$(bench_sync_hint)"
		return 1
	fi

	local src_ref
	src_ref="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replica_source)"
	if [ -n "$src_ref" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the sync recorded a replica_source to fence against" "$fo_ok" "got '$src_ref'"

	# --- promote, arming a fence --------------------------------------------
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" promote-fenced \
		-promote -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN" \
		-promote-mode forced -fence-source -promoted-by bench-harness -start
	if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "promote with -fence-source succeeds" "$fo_ok" "exit $RUN_RC see $RUN_LOG"

	local fence_id
	fence_id="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" fence_id)"
	if [ -n "$fence_id" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the promotion armed a fence" "$fo_ok" "fence_id empty"

	# The fence requires the promoted domain to be RUNNING: stopping the
	# source while nothing serves would leave zero copies up.
	local tgt_state
	tgt_state="$(dom_state "$TARGET_URI" "$TARGET_DOMAIN" 2>/dev/null || true)"
	if [ "$tgt_state" = running ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the promoted copy is running, which the fence requires" "$fo_ok" "got '$tgt_state'"

	# --- 7g: a LIVE promoted copy cannot be relabelled out from under itself --
	#
	# This stage is the only place on the harness where a promoted domain is
	# actually RUNNING and carrying an armed fence, which makes it the only place
	# the CI-36 guard can be exercised at all: SetReplicationRole reads the
	# domain's runtime state, and no unit test can reach that read.
	#
	# Three assertions, and the third is the one that matters most. The refusal
	# alone would be satisfied by a build that refused everything.
	if [ "$DRY_RUN" != yes ] && [ "$tgt_state" = running ]; then
		vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" relabel-live-refused \
			-update-role paused -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
		if [ "$RUN_RC" != 0 ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "-update-role=paused is refused on a RUNNING promoted copy" "$fo_ok" \
			"exit $RUN_RC -- recording a serving copy as an administratively paused replica is the misclick CI-36 is about, and the same write discards the fence against the source it displaced; see $RUN_LOG"

		local live_role live_fence
		live_role="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
		if [ "$live_role" = promoted ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the refused relabel left the role untouched" "$fo_ok" \
			"role is '$live_role' -- a refusal that half-applied would be worse than none"

		live_fence="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" fence_id)"
		if [ "$live_fence" = "$fence_id" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the refused relabel left the armed fence intact" "$fo_ok" \
			"fence_id is '$live_fence', was '$fence_id' -- the token is the credential that authorises stopping the displaced source, and no role change restores it"

		# And the exemption, which is what keeps this a gate rather than a wall:
		# `source` says the same thing `promoted` does about a running copy, so it
		# is accepted -- and it is the one transition that KEEPS the token, because
		# the arrangement the token describes is still in force.
		vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" relabel-live-source \
			-update-role source -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
		if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "-update-role=source IS accepted on a RUNNING promoted copy" "$fo_ok" \
			"exit $RUN_RC -- refusing this would close the one route that keeps the data, which every served-live refusal points at; see $RUN_LOG"

		live_fence="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" fence_id)"
		if [ "$live_fence" = "$fence_id" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "promoted->source KEEPS the armed fence" "$fo_ok" \
			"fence_id is '$live_fence', was '$fence_id' -- this end alone was rewritten, so the domain it displaced still calls itself a source and the arrangement the token describes is still in force"

		# Back to promoted for the rest of the stage, which is all about the fence
		# firing against the source. -update-role promoted writes no promotion
		# record and strips nothing, so the token and the trace both survive it.
		vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" relabel-live-restore \
			-update-role promoted -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
		live_role="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
		if [ "$live_role" = promoted ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "7g put the role back to promoted for the rest of the stage" "$fo_ok" \
			"role is '$live_role' -- everything below this needs a promoted target"
		if [ "$live_role" != promoted ]; then
			warn "7g could not restore replication_role=promoted on $TARGET_DOMAIN; the fence assertions below will not mean anything"
		fi
	else
		results_row "$CSV" "$sc" live_promoted_guard "" "" "" "" "" "" "SKIP the promoted target is not running"
	fi

	# --- the agents ----------------------------------------------------------
	log "starting a standalone agent on the source host (schedule disabled -- only its fence loop matters)"
	src_pid="$(agent_start "$SOURCE_HOST" "$SOURCE_LOCAL" "$SOURCE_AGENT_BIN" "$src_vmsync" "$SOURCE_DOMAIN" "$agent_dir" | tr -d "[:space:]")"
	if [ -n "$src_pid" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the source agent started" "$fo_ok" "no pid returned"

	if [ -n "${TARGET_AGENT_BIN:-}" ]; then
		log "starting a standalone agent on the target host too -- it must NOT fence the promoted domain"
		tgt_pid="$(agent_start "$TARGET_HOST" no "$TARGET_AGENT_BIN" "$TARGET_VMSYNC_BIN" "$TARGET_DOMAIN" "$agent_dir" | tr -d '[:space:]')"
	fi

	# --- wait for the fence to fire ------------------------------------------
	# The agent sweeps once immediately on startup, before its first tick, so
	# this is normally seconds rather than the 60s tick interval. The timeout
	# covers the guest's own shutdown, which is the slow part.
	local waited=0 limit="${FENCE_WAIT_SECONDS:-180}" state=""
	log "waiting up to ${limit}s for the source agent to fence '$SOURCE_DOMAIN'"
	while [ "$waited" -lt "$limit" ]; do
		state="$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN" 2>/dev/null || true)"
		[ "$state" = shutoff ] && break
		sleep 5
		waited=$((waited + 5))
	done

	if [ "$state" = shutoff ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the fence shut the displaced source down" "$fo_ok" \
		"source is '$state' after ${waited}s -- see $agent_dir/agent.log on $SOURCE_HOST"

	# --- and left it in the right state ---------------------------------------
	#
	# 'fenced', not 'paused'. They are separate roles, and 'fenced' is the one
	# nobody CHOSE -- asserting that specific word is what gives this check its
	# force. A single word covering both would leave it unable to tell a domain
	# a fence had stopped from one an operator had suspended by hand, and it
	# would pass on either.
	if [ "$state" = shutoff ]; then
		local src_role
		src_role="$(vmsync_meta_field "$SOURCE_URI" "$SOURCE_DOMAIN" replication_role)"
		if [ "$src_role" = fenced ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the fenced source is left fenced not merely stopped" "$fo_ok" \
			"got role '$src_role' -- without this the next sync would start it replicating again"

		# The ledger is what makes a fence single-use. Its presence here is
		# also what would stop a second attempt on the next sweep.
		local ledger
		ledger="$(run_shell_on "$SOURCE_HOST" "$SOURCE_LOCAL" "cat '$agent_dir/fences.json' 2>/dev/null || true")"
		case "$ledger" in
		*"$fence_id"*) fo_ok=0 ;;
		*) fo_ok=1 ;;
		esac
		fo_check "$sc" "the agent recorded the fence in its durable ledger" "$fo_ok" \
			"fence id '$fence_id' not found in $agent_dir/fences.json"

		# Polled, not sampled once, because the domain reaching shutoff does
		# NOT mean the agent has finished.
		#
		# The agent writes its record as state=running BEFORE it shells out to
		# vmsync, and only rewrites it to done once that process has exited
		# (cmd/vmsync-agent/fence.go). The wait above breaks the moment libvirt
		# reports the domain shut off -- which happens while vmsync is still
		# running, since it goes on to pause replication afterwards. So reading
		# the ledger right then reliably catches the record mid-flight, still
		# saying running, and reports a working fence as a failure.
		#
		# That is exactly why the preceding check passed: the fence id is in
		# the ledger from the running-state write.
		local lwaited=0 llimit="${FENCE_LEDGER_WAIT_SECONDS:-60}" lstate=""
		while [ "$lwaited" -lt "$llimit" ]; do
			ledger="$(run_shell_on "$SOURCE_HOST" "$SOURCE_LOCAL" "cat '$agent_dir/fences.json' 2>/dev/null || true")"
			case "$ledger" in
			*'"state": "done"'* | *'"state":"done"'*) lstate=done; break ;;
			*'"state": "failed"'* | *'"state":"failed"'*) lstate=failed; break ;;
			esac
			sleep 2
			lwaited=$((lwaited + 2))
		done

		case "$lstate" in
		done) fo_ok=0 ;;
		*) fo_ok=1 ;;
		esac
		# "failed" is reported as itself rather than as a timeout: the agent
		# recording a failed fence is a real finding about the fence, and it
		# carries the reason.
		fo_check "$sc" "the ledger records the fence as done" "$fo_ok" \
			"ledger state after ${lwaited}s: ${lstate:-never reached a terminal state}; ledger: $(printf '%s' "$ledger" | tr -d '\n' | cut -c1-200)"

		# --- 7f: a fence that did not stop the domain must stay visible -------
		#
		# The state CI-07 was about, reached the safe way. A genuinely failed
		# ACPI shutdown needs a guest that ignores it, which cannot be staged
		# here -- but the state the agent must react to is "role=fenced AND the
		# domain is running", and `virsh start` produces exactly that, writing
		# no metadata at all. That is not a weaker test of the detector: the
		# detector reads those two facts and nothing else, and this is also a
		# real path into the state, because nothing clears a fenced domain's
		# autostart flag and a host reboot starts it again on its own.
		#
		# Against a pre-change binary every assertion below reads the opposite:
		# the sweep skipped the domain BECAUSE it was fenced, so no warning was
		# logged, vmsync_agent_fenced_running did not exist at all, and
		# vmsync_agent_split_brain_vms stayed at 0 with one VM live in two
		# places.
		local ametrics="$agent_dir/vmsync-agent.prom"
		local before_file
		before_file="$RUN_DIR/logs/${sc}.agent-metrics-before.prom"
		run_shell_on "$SOURCE_HOST" "$SOURCE_LOCAL" "cat '$ametrics' 2>/dev/null || true" >"$before_file" 2>/dev/null || true
		# Anchored on a series that is always present, so "the gauge is absent"
		# can never be satisfied by an empty or missing file.
		if prom_has "$before_file" vmsync_agent_domains_total; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the agent's metrics file is readable before the check" "$fo_ok" \
			"no vmsync_agent_domains_total in $ametrics on $SOURCE_HOST -- every assertion below would otherwise pass on a file that does not exist"

		if [ "$fo_ok" = 0 ]; then
			if [ "$(prom_first "$before_file" vmsync_agent_fenced_running_vms)" = 0 ]; then fo_ok=0; else fo_ok=1; fi
			fo_check "$sc" "the fenced-and-running gauge exists at zero while the fence holds" "$fo_ok" \
				"expected vmsync_agent_fenced_running_vms 0 with the source correctly fenced and stopped; a gauge that only appears once the estate is broken cannot carry a for: clause"

			# Start it again: fenced role, running domain, no metadata written.
			run_shell_on "$SOURCE_HOST" "$SOURCE_LOCAL" "virsh -c '$SOURCE_URI' start '$SOURCE_DOMAIN'" >/dev/null 2>&1 || true
			local fwaited=0 fstate=""
			while [ "$fwaited" -lt 60 ]; do
				fstate="$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN" 2>/dev/null || true)"
				[ "$fstate" = running ] && break
				sleep 2
				fwaited=$((fwaited + 2))
			done
			if [ "$fstate" = running ]; then fo_ok=0; else fo_ok=1; fi
			fo_check "$sc" "the fenced source can be started again for the check" "$fo_ok" \
				"the domain did not reach running after ${fwaited}s (state '$fstate'), so 7f could not be exercised"

			if [ "$fo_ok" = 0 ]; then
				local frole
				frole="$(vmsync_meta_field "$SOURCE_URI" "$SOURCE_DOMAIN" replication_role)"
				if [ "$frole" = fenced ]; then fo_ok=0; else fo_ok=1; fi
				fo_check "$sc" "starting the domain leaves its fenced role intact" "$fo_ok" \
					"role is '$frole' after virsh start -- 7f needs role=fenced AND running, and if a plain start rewrote the role the rest of this proves nothing"

				# One sweep interval plus one metrics write, generously.
				local awaited=0 after_file="$RUN_DIR/logs/${sc}.agent-metrics-after.prom" seen=""
				while [ "$awaited" -lt "${FENCE_WAIT_SECONDS:-120}" ]; do
					run_shell_on "$SOURCE_HOST" "$SOURCE_LOCAL" "cat '$ametrics' 2>/dev/null || true" >"$after_file" 2>/dev/null || true
					if [ "$(prom_first "$after_file" vmsync_agent_fenced_running_vms)" = 1 ]; then seen=yes; break; fi
					sleep 3
					awaited=$((awaited + 3))
				done
				if [ "$seen" = yes ]; then fo_ok=0; else fo_ok=1; fi
				fo_check "$sc" "a fence that left the domain running raises its own gauge" "$fo_ok" \
					"vmsync_agent_fenced_running_vms did not reach 1 within ${awaited}s of $SOURCE_DOMAIN running while marked fenced. This is the alarm for one VM live in two places, and before it existed the sweep skipped the domain precisely BECAUSE it was fenced -- see $after_file"

				if grep -q "vmsync_agent_fenced_running{.*vm=\"$SOURCE_DOMAIN\"" "$after_file" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
				fo_check "$sc" "the gauge names which VM is live twice" "$fo_ok" \
					"no vmsync_agent_fenced_running series for $SOURCE_DOMAIN -- the count says whether, this says which, and which is what an operator needs before touching anything"

				# And it must also keep the pre-existing split-brain alarm up,
				# which is the one operators already have rules for.
				if [ "$(prom_first "$after_file" vmsync_agent_split_brain_vms)" -ge 1 ] 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
				fo_check "$sc" "a failed fence still counts as split brain" "$fo_ok" \
					"vmsync_agent_split_brain_vms is $(prom_first "$after_file" vmsync_agent_split_brain_vms) -- the domain is live beside the promoted copy, so alerts already written against this series must keep firing"
			fi
		fi
	fi

	# --- the target's own agent must have left the promoted copy alone --------
	if [ -n "$tgt_pid" ]; then
		local tgt_after
		tgt_after="$(dom_state "$TARGET_URI" "$TARGET_DOMAIN" 2>/dev/null || true)"
		if [ "$tgt_after" = running ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the target's own agent did NOT fence the promoted copy" "$fo_ok" \
			"promoted domain is '$tgt_after' -- a fence sweep must skip anything whose role is not source"
	fi

	# Restore NOW rather than leaving it to the EXIT trap. The trap is the
	# safety net for a die or a Ctrl+C; if it were also the normal path, a
	# later stage in the same --stages list would run against a source that
	# is still shut off and `paused`, and the report would be written before
	# anything was put back.
	fence_agent_cleanup
	return 0
}

# --- verdicts ------------------------------------------------------------------

# Every stage but the matrix records its own PASS/FAIL/SKIP in the notes
# column, so a stage's verdict is derivable from results.csv rather than
# needing each stage to remember to report one. Stage 1 is the exception: it
# has no assertions, only timings, so its failure signal is a non-zero vmsync
# exit.
#
# NON_MATRIX_SCENARIOS is how the two are told apart. Stage 1's scenario names
# are generated from scenarios.conf and cannot be matched positively, so every
# other stage's names are listed here and Stage 1 is what is left.
# Prefixes rather than an exact list of every scenario name, so a new
# sub-scenario in an existing stage does not silently start counting as a
# Stage 1 transport run. "retention" was doing exactly that: its rows are not
# excluded by name, so every restore point check was being folded into the
# matrix verdict, and its one FAIL would have failed Stage 1 on any run that
# included both.
#
# "checksum" was doing it too, and worse than retention did: stage 13 fails a
# sync ON PURPOSE (13b's falsified digest must be refused), so any run passing
# --stages matrix,checksum reported Stage 1 as FAILED on the strength of a
# deliberate refusal in another stage. Stage 14's rows are already covered by
# the ^verify- prefix.
#
# commit-barrier, interrupted-reinit and journal are here for exactly that
# reason. The first two fail a sync on purpose as well -- one with
# -test=fail-last-disk, one with -test=die-writing-base -- so without them a
# run passing --stages matrix,commit-barrier or matrix,interrupted-reinit
# would report Stage 1 as FAILED on the strength of a deliberate kill in
# another stage. commit-barrier was missing when this line was last touched.
NON_MATRIX_SCENARIOS='^(verify-|reinit-after-failures$|ext-snapshot$|define-|failover$|fence-agent$|retention$|restore$|invert$|wedge$|checksum$|commit-barrier$|interrupted-reinit$|journal$|colocated$)'

# stage_pattern STAGE -> the regex matching that stage's scenario column.
# --- Stage 8: verify after a long incremental chain --------------------------
#
# Everything else here verifies a replica that was built moments ago by a
# single -reinit. This builds one the way a real deployment does -- dozens of
# incremental syncs, each carrying a real guest write -- and only then asks
# whether corruption is still detectable. Deliberately opt-in: it is the
# longest stage by a wide margin.
# --- Stage 8: verify after a long incremental chain --------------------------
#
# Everything else here verifies a replica that was built moments ago by a
# single -reinit. This builds one the way a real deployment does -- dozens of
# incremental syncs, each carrying real guest writes -- and corrupts it at a
# random point PART WAY THROUGH, so the copies that follow run over the damage
# exactly as they would in production.
#
# Two things about the shape are deliberate, and both were wrong in the first
# version of this stage:
#
#   - Every mode gets its OWN full chain. Healing after a tamper has to be a
#     full -reinit (an incremental sync re-copies only what the SOURCE's dirty
#     bitmap says changed, and the source never wrote to the corrupted region),
#     which destroys the chain. So a single chain followed by several
#     tamper/verify/heal attempts tests a deep chain exactly once and a
#     one-deep chain every time after -- while reporting all of them as if
#     they had tested the same thing.
#
#   - The corruption lands at a random copy, not after the last one. Bit rot
#     does not wait for a sync window to close. Injecting it mid-chain asks a
#     question the end-of-chain version cannot: does the damage survive the
#     incremental syncs that follow it, and is it still detectable afterwards?
#
# Deliberately opt-in: it is by far the longest stage.
stage_verify_long() {
	log "=== Stage 8: -verify after a long incremental chain ==="

	local copies="${VERIFY_LONG_COPIES}"

	if [ "$DRY_RUN" != yes ]; then
		# The chain is only meaningful if the guest writes between copies,
		# and that needs a RUNNING guest with a usable agent. Without it this
		# stage would spend an hour proving that vmsync can copy nothing
		# twenty times -- so it skips rather than reporting a green result
		# that verified nothing.
		local src_state
		src_state="$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN")" || src_state="unknown"
		if [ "$src_state" != running ]; then
			warn "SKIP stage verify-long: source domain '$SOURCE_DOMAIN' is '$src_state', but this stage needs a running guest to dirty its own disk between copies"
			results_row "$CSV" verify-long precondition "" "" "" "" "" "" "SKIP source domain not running"
			return 0
		fi
		if [ "$GUEST_DIRTY" = yes ]; then
			# Waited for, not merely probed: if stage 7 ran earlier in this
			# same list it restarted the source seconds ago and the agent is
			# not up yet. On timeout, fall through to guest_exec_available
			# anyway -- it is the one that produces the message saying
			# WHICH of the two problems this is (no agent answering at all,
			# or an agent that answers but blocks guest-exec).
			wait_for_guest_agent || true
			if ! guest_exec_available; then
				warn "SKIP stage verify-long: $GUEST_EXEC_WHY"
				results_row "$CSV" verify-long precondition "" "" "" "" "" "" "SKIP guest-exec unavailable"
				return 0
			fi
		fi
		stage_needs_target_shutoff "$CSV" verify-long "stage verify-long" || return 0
	fi

	local target_path="" vsize=0 mode
	for mode in $VERIFY_LONG_MODES; do
		log "--- verify-long/$mode: building a fresh ${copies}-deep chain ---"

		# -force-clean, not a plain -reinit, and for the same reason
		# heal_target needs one: the PREVIOUS mode's round ended
		# with a -verify that found a difference, which records
		# verify_state=failed on the target domain -- and a domain carrying
		# that record refuses an ordinary sync and a plain -reinit alike,
		# deliberately, because a reinit recopies without proving the result.
		# With a plain -reinit this baseline is refused at preflight in ~0.3s
		# with mode=unknown from the second mode on, and the stage aborts
		# blaming -compress.
		bench_sync verify-long "${mode}-baseline" -force-clean
		if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
			warn "baseline full sync for verify-long/$mode failed (see $RUN_LOG) -- aborting stage 8$(bench_sync_hint)"
			return 1
		fi

		if [ "$DRY_RUN" != yes ] && [ -z "$target_path" ]; then
			target_path="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
			[ -n "$target_path" ] || { warn "could not resolve target disk path for dev='$TAMPER_DISK_DEV' -- check TAMPER_DISK_DEV in $CONF"; return 1; }
			vsize="$(target_virtual_size "$target_path")" || true
			[ -n "$vsize" ] && [ "$vsize" -gt 0 ] 2>/dev/null \
				|| { warn "could not read the virtual size of $target_path on $TARGET_HOST via qemu-img info"; return 1; }
			log "target disk under test: $target_path ($vsize bytes) on $TARGET_HOST"
		fi

		# A round that gives up returns non-zero, and this is a bare
		# command under set -e, so without the guard the script would
		# end here -- before generate_report, which is exactly the
		# failure mode the return-instead-of-die conversion was for.
		verify_long_round "$mode" "$copies" "$target_path" "$vsize" || return 1
	done

	# One heal, at the end. Each round already begins with its own
	# -force-clean, which heals whatever the previous round left behind --
	# including the verify_state record its -verify wrote, which is precisely
	# why that baseline cannot be a plain -reinit any more. Only the last round
	# needs cleaning up after. (A round that gives up part-way skips this and
	# leaves the replica corrupted on purpose -- its message says so and says
	# to inspect it. The stage is marked FAIL and the report still prints.)
	heal_target verify-long final-heal "$target_path"
	return 0
}

# verify_long_round MODE COPIES PATH VSIZE -- one mode's full chain, with the
# corruption injected at a random point inside it.
verify_long_round() {
	local mode="$1" copies="$2" path="$3" vsize="$4"
	local i=1 tamper_at=0 moved=0 tampered=no outcome

	if [ "$DRY_RUN" != yes ]; then
		# Which copy the damage lands after. 1..copies, so it can fall
		# anywhere from "immediately, with every later copy running over it"
		# to "after the last one", and the draw is part of the seeded
		# sequence like every other.
		TAMPER_SEQ=$((TAMPER_SEQ + 1))
		tamper_at=$(($(rng_below "$copies") + 1))
		log "   this round's corruption lands after copy $tamper_at of $copies (seed $TAMPER_SEED)"
	fi

	while [ "$i" -le "$copies" ]; do
		if [ "$DRY_RUN" != yes ] && [ "$GUEST_DIRTY" = yes ]; then
			guest_dirty || warn "guest write $i/$copies failed -- this copy will carry less (or nothing) than intended"
		fi
		bench_sync verify-long "${mode}-copy-${i}"
		if [ "$DRY_RUN" != yes ]; then
			if [ "$RUN_RC" != 0 ]; then
				warn "incremental copy ${mode}-copy-${i} failed (see $RUN_LOG) -- the chain is broken, so nothing after it would mean anything"
				return 1
			fi
			[ "$(prom_sum "$RUN_PROM" vmsync_transferred_bytes)" -gt 0 ] && moved=$((moved + 1))

			if [ "$i" -eq "$tamper_at" ]; then
				stage_needs_target_shutoff "$CSV" verify-long "verify-long tamper" || return 0
				if draw_tamper "$vsize"; then
					log "   corrupting after copy $i at offset $TAMPER_OFF length $TAMPER_LEN"
					# mtime preserved, or the very next copy would die at
					# vmsync's mtime guard instead of running over the damage
					# the way a real incremental sync would.
					if ! tamper_target "$path" yes; then
						results_row "$CSV" verify-long "${mode}-result" "" "" "" "" "" "" "SKIP the tamper could not be applied"
						heal_target verify-long "${mode}-tamper-heal" "$path"
						return 0
					fi
					tampered=yes
				else
					warn "SKIP verify-long/$mode: the configured tamper band does not fit inside a ${vsize}-byte disk"
					results_row "$CSV" verify-long "${mode}-result" "" "" "" "" "" "" "SKIP tamper band does not fit the disk"
				fi
			fi
		fi
		i=$((i + 1))
	done

	if [ "$DRY_RUN" = yes ]; then
		# Still run the verify sync, so --dry-run prints every command line
		# this round would issue -- which is the whole point of --dry-run.
		bench_sync verify-long "${mode}-verify" "-verify=$mode"
		results_row "$CSV" verify-long "${mode}-result" DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi

	# The chain has to have carried real data, or this is a ${copies}-long
	# sequence of no-ops and the verification below tests nothing.
	if [ "$moved" -eq 0 ]; then
		warn "FAIL: none of the $copies incremental copies in the $mode round transferred a byte -- the guest is not dirtying its disk, so this chain is $copies no-ops. Check that GUEST_DIRTY_PATH is writable inside the guest and that dd is reaching the disk."
		results_row "$CSV" verify-long "${mode}-chain" 1 "" "" "" "" "" "FAIL chain carried no data"
		return 0
	fi
	log "   $moved of $copies copies carried real data"
	results_row "$CSV" verify-long "${mode}-chain" 0 "" "" "" "" "" "PASS chain carried real data"

	[ "$tampered" = yes ] || return 0

	# Did the damage survive the copies that ran after it?
	#
	# It legitimately might not: an incremental sync re-copies every block the
	# source's dirty bitmap reports, so if the guest happened to write to the
	# same region the corruption sat in, a later copy overwrote it -- correctly
	# and by design. Verify would then find nothing, and scoring that as "verify
	# missed it" would be a false accusation against the code under test. This
	# is not a rare corner either: GUEST_DIRTY rewrites the same file every
	# round, so its blocks are exactly the ones most likely to be re-copied.
	if ! ssh_host_cmd "$TARGET_HOST" qemu-io -r -f qcow2 \
		-c "'read -P ${TAMPER_PATTERN} ${TAMPER_OFF} ${TAMPER_LEN}'" "'${path}'" >/dev/null 2>&1; then
		log "   SKIP: a later incremental copy overwrote the corrupted region, so there is nothing left for -verify=$mode to find"
		results_row "$CSV" verify-long "${mode}-result" "" "" "" "" "" "" "SKIP corruption healed by a later copy"
		return 0
	fi

	bench_sync verify-long "${mode}-verify" "-verify=$mode"
	outcome="$(verify_outcome "$RUN_PROM")"
	case "$outcome" in
	RAN_MISMATCH)
		log "   PASS: -verify=$mode found the corruption after $((copies - tamper_at)) further incremental copies"
		results_row "$CSV" verify-long "${mode}-result" 0 "" "" "" "" "" "PASS mismatch detected after a deep chain"
		;;
	RAN_CLEAN)
		warn "FAIL: -verify=$mode ran and found NOTHING, though the corruption at offset $TAMPER_OFF length $TAMPER_LEN was still present on disk immediately before the compare (reproduce with TAMPER_SEED=$TAMPER_SEED). See $RUN_LOG"
		results_row "$CSV" verify-long "${mode}-result" 1 "" "" "" "" "" "FAIL mismatch NOT detected"
		;;
	*)
		warn "FAIL: -verify=$mode never reached its compare -- the run ended before verification ran, so nothing was verified (exit=$RUN_RC). See $RUN_LOG"
		results_row "$CSV" verify-long "${mode}-result" 1 "" "" "" "" "" "FAIL verification never ran"
		;;
	esac
	return 0
}


# --- Stage 9: -retention (restore points on the target) ----------------------
#
# Every assertion here is about the target's filesystem, because that is where
# a restore point lives -- vmsync keeps no inventory of its own, deliberately
# (docs/design/restore-points.md explains why it cannot).
#
# The reflink assertion is the one worth reading twice. A restore point that is
# a full byte-for-byte copy would pass every other check in this stage: the
# directory would exist, the disk would be there, it would boot. It would just
# cost a whole replica per copy instead of sharing storage with it, and nothing
# would say so. So the space it actually consumed is measured, not assumed.
stage_retention() {
	log "=== Stage 9: -retention restore points ==="
	local sc=retention
	local fo_ok=0

	if [ "$DRY_RUN" = yes ]; then
		bench_sync "$sc" baseline -reinit "-retention=3,0"
		bench_sync "$sc" reflink-probe "-retention=3,0"
		bench_sync "$sc" within-interval "-retention=3,1h"
		bench_sync "$sc" prune-1 "-retention=2,0"
		bench_sync "$sc" auto-induce-1 "-reinit-after-failures=2" "-retention=2,0"
		bench_sync "$sc" auto-trigger "-reinit-after-failures=2" "-retention=2,0"
		bench_sync "$sc" reinit-sweep -reinit "-retention=2,0"
		fo_check "$sc" "a sync with -retention takes a restore point" 0
		return 0
	fi

	stage_needs_target_shutoff "$CSV" "$sc" "stage retention" || return 0
	reset_pair_state "$sc" || return 0
	require_target_syncable "$sc" || return 0

	# This TARGET DOMAIN's own store, not the shared directory above it.
	# Restore points are kept per target domain, and the directory this sits in
	# is shared with every other domain replicating into TARGET_DISK_PATH.
	local rp_dir
	rp_dir="$(rp_store_dir)"

	# Start from nothing, so every count below is this stage's own doing and
	# not a leftover from an earlier run.
	#
	# One store, never the shared root. Clearing the root would delete every
	# co-located replica's entire history -- which is the defect the per-domain
	# layout fixed, and a harness that did it would be reintroducing it on
	# whatever target it was pointed at.
	rp_clear_store

	# --- does the target support this at all? --------------------------------
	# An interval of 0 means "every sync", which is what a test wants: the
	# alternative is a stage that sleeps for hours.
	bench_sync "$sc" baseline -reinit "-retention=3,0"
	if [ "$RUN_RC" != 0 ]; then
		if grep -q "does not support reflink copies" "$RUN_LOG" 2>/dev/null; then
			warn "SKIP stage retention: $TARGET_DISK_PATH on $TARGET_HOST does not support reflink copies (needs XFS with reflink=1, or btrfs)"
			results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP target filesystem has no reflink support"
			return 0
		fi
		warn "baseline sync with -retention failed (see $RUN_LOG) -- aborting stage 9$(bench_sync_hint)"
		return 1
	fi

	local n
	n="$(rp_count "$rp_dir")"
	if [ "$n" = 1 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "a sync with -retention takes a restore point" "$fo_ok" \
		"expected exactly 1 in $rp_dir but found $n"
	if [ "$fo_ok" != 0 ]; then
		# Everything below reads that restore point. Without one they do not
		# fail informatively -- they fail SEVEN times for one cause, and two of
		# them PASS while proving nothing: the reflink check measures a
		# free-space delta, which is indistinguishable from zero when no copy
		# was made, and the "inspection leaves replication state untouched"
		# check is trivially true of an inspection that had nothing to inspect.
		# A vacuous pass is worse than a failure, so stop here with the one
		# thing an operator needs to look at, the way stage 16 does.
		warn "no restore point exists, so the remaining retention checks are skipped rather than reported as a wall of separate failures -- two of them would otherwise PASS on an empty store and prove nothing. Fix this first: check $RUN_LOG for what the sync said about -retention, and confirm the vmsync binary under test is built from this tree ($rp_dir is the per-domain layout; a binary predating it writes into $(rp_root) directly)"
		return 1
	fi

	# --- is it actually sharing storage? -------------------------------------
	# Measured across one more sync rather than inferred: if these were full
	# copies, the target's free space would fall by roughly the size of the
	# replica every time. A reflink costs a few metadata blocks.
	local tdisk disk_bytes free_before free_after used
	tdisk="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
	if [ -n "$tdisk" ]; then
		disk_bytes="$(ssh_host_cmd "$TARGET_HOST" "qemu-img info --output=json '$tdisk'" 2>/dev/null \
			| awk -F: '/"actual-size"/ { gsub(/[^0-9]/, "", $2); print $2; exit }')"
	fi
	free_before="$(fs_free_bytes "$TARGET_HOST" "$TARGET_DISK_PATH")"
	bench_sync "$sc" reflink-probe "-retention=3,0"
	free_after="$(fs_free_bytes "$TARGET_HOST" "$TARGET_DISK_PATH")"

	if [ -z "${disk_bytes:-}" ] || [ -z "$free_before" ] || [ -z "$free_after" ]; then
		warn "SKIP the reflink space check: could not size the replica or read free space on $TARGET_HOST"
		results_row "$CSV" "$sc" reflink_shares_storage "" "" "" "" "" "" "SKIP could not measure"
	else
		used=$((free_before - free_after))
		# A tenth of the replica is a generous ceiling: a real copy costs the
		# whole thing and a reflink costs metadata, so the gap is orders of
		# magnitude. The slack absorbs whatever else on the host wrote during
		# the sync, and a negative figure (the host freed space) is fine too.
		if [ "$used" -lt $((disk_bytes / 10)) ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the restore point shares storage rather than copying the replica" "$fo_ok" \
			"the filesystem lost ${used}B taking a restore point of a ${disk_bytes}B replica -- a reflink should cost almost nothing"
	fi

	# --- the sidecar ---------------------------------------------------------
	local tag sidecar
	tag="$(rp_newest "$rp_dir")"
	sidecar="$(ssh_host_cmd "$TARGET_HOST" "cat '$rp_dir/$tag/status.json' 2>/dev/null || true")"
	case "$sidecar" in
	*'"verify"'*) fo_ok=0 ;;
	*) fo_ok=1 ;;
	esac
	fo_check "$sc" "the restore point records what is known about it" "$fo_ok" \
		"no verify state in $rp_dir/$tag/status.json: $(printf '%s' "$sidecar" | tr -d '\n' | tr ',' ';' | cut -c1-120)"

	# --- the interval is honoured -------------------------------------------
	local before_count after_count
	before_count="$(rp_count "$rp_dir")"
	bench_sync "$sc" within-interval "-retention=3,1h"
	after_count="$(rp_count "$rp_dir")"
	if [ "$after_count" = "$before_count" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "a sync inside the interval takes no new restore point" "$fo_ok" \
		"count went from $before_count to $after_count despite -retention=3,1h"

	# --- retention prunes to the count, oldest first -------------------------
	local oldest
	oldest="$(rp_oldest "$rp_dir")"
	bench_sync "$sc" prune-1 "-retention=2,0"
	bench_sync "$sc" prune-2 "-retention=2,0"
	after_count="$(rp_count "$rp_dir")"
	if [ "$after_count" = 2 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "retention prunes down to the configured count" "$fo_ok" \
		"expected 2 with -retention=2,0 but found $after_count"

	if rp_has "$rp_dir" "$oldest"; then fo_ok=1; else fo_ok=0; fi
	fo_check "$sc" "pruning removes the oldest restore point first" "$fo_ok" \
		"'$oldest' is still present after pruning to 2"

	# --- listing and cloning change nothing ----------------------------------
	# The phase 1 thesis: an operator can find out whether a restore point is
	# clean without touching replication state. If inspecting one moved
	# last_checkpoint, the next incremental sync would write its delta onto the
	# wrong baseline -- exactly the hazard this feature is designed around, and
	# one that would look like a clean sync right up until a promotion.
	local cp_before cp_after clone_dir="/var/tmp/vmsync-bench-clone.$$"
	cp_before="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_checkpoint)"
	tag="$(rp_newest "$rp_dir")"

	if vmsync_verb "$sc" list -list-restore-points; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "-list-restore-points reports them" "$fo_ok" "see $RUN_LOG"

	if vmsync_verb "$sc" clone -clone-restore-point "$tag" -clone-to "$clone_dir"; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "-clone-restore-point materialises the restore point" "$fo_ok" "see $RUN_LOG"

	if ssh_host_cmd "$TARGET_HOST" "for f in '$clone_dir'/*.qcow2; do qemu-img info \"\$f\" >/dev/null || exit 1; done" >/dev/null 2>&1; then
		fo_ok=0
	else
		fo_ok=1
	fi
	fo_check "$sc" "the clone is a readable qcow2" "$fo_ok" "qemu-img info failed on $clone_dir"

	cp_after="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_checkpoint)"
	if [ "$cp_before" = "$cp_after" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "inspecting a restore point leaves replication state untouched" "$fo_ok" \
		"last_checkpoint moved from '$cp_before' to '$cp_after' -- the next incremental sync would write onto the wrong baseline"

	ssh_host_cmd "$TARGET_HOST" "rm -rf '$clone_dir'" >/dev/null 2>&1 || true

	# --- an AUTOMATIC reinit must not take them ------------------------------
	#
	# The carve-out that matters most, and the only one here where being wrong
	# destroys data rather than disk space. -reinit-after-failures fires on a
	# failure count, and "syncs have been failing repeatedly" is uncomfortably
	# close to "something is wrong with the source" -- which is the exact
	# situation these copies exist for. An auto-reinit that swept them would
	# discard the operator's last good replica at the moment they most need it.
	#
	# Failures are induced the same way Stage 3 induces them: remove the
	# target's vmsync metadata, and every incremental sync refuses with an
	# unverifiable checkpoint chain. Those runs fail before they reach the
	# retention code, so they take no restore points of their own -- which is
	# what makes the count below a clean before/after.
	# The retention count here is deliberately far above the number of restore
	# points present, and that is what makes the assertion mean anything.
	#
	# With a tight count the triggering sync takes one of its own, retention
	# prunes the oldest to stay within the count, and a preserved restore point
	# disappears -- correctly, by policy, having never been touched by the
	# reinit. The check would then report "the automatic reinit did not preserve
	# them" for a run in which it did exactly that. Leaving room for every
	# existing point plus the new one means the ONLY thing that can remove one
	# is the sweep this check is about.
	local n_fail=2 i=1 tags_before keep_all=9
	tags_before="$(rp_list "$rp_dir")"
	if [ -z "$tags_before" ]; then
		warn "SKIP the auto-reinit check: no restore points survived to this point to preserve"
		results_row "$CSV" "$sc" auto_reinit_preserves "" "" "" "" "" "" "SKIP nothing to preserve"
	else
		virsh_uri "$TARGET_URI" metadata "$TARGET_DOMAIN" --uri "$VMSYNC_METADATA_URI" --remove --config >/dev/null 2>&1 \
			|| { warn "could not remove the target's vmsync metadata to induce failures -- aborting stage 9"; return 1; }

		while [ "$i" -le "$n_fail" ]; do
			bench_sync "$sc" "auto-induce-$i" "-reinit-after-failures=$n_fail" "-retention=$keep_all,0"
			if [ "$RUN_RC" = 0 ]; then
				warn "induced sync $i unexpectedly succeeded -- the auto-reinit check below may not exercise what it claims"
			fi
			i=$((i + 1))
		done

		bench_sync "$sc" auto-trigger "-reinit-after-failures=$n_fail" "-retention=$keep_all,0"

		if [ "$RUN_RC" = 0 ] && grep -q "threshold reached" "$RUN_LOG" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "-reinit-after-failures forces a reinit once the threshold is reached" "$fo_ok" \
			"exit $RUN_RC and no threshold message in $RUN_LOG -- the check below would then prove nothing"

		# Every tag that existed before the automatic reinit must still exist.
		# Not a count: the triggering sync takes one of its own, so the count
		# legitimately grows.
		local missing="" t
		for t in $tags_before; do
			rp_has "$rp_dir" "$t" || missing="$missing $t"
		done
		if [ -z "$missing" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "an automatic reinit preserves the existing restore points" "$fo_ok" \
			"gone after -reinit-after-failures fired:$missing -- retention was -retention=$keep_all,0 with only $(rp_count "$rp_dir") points present so pruning cannot account for this; the reinit swept them"

		if grep -q "keeping the existing restore points" "$RUN_LOG" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the automatic reinit says it kept them" "$fo_ok" \
			"no such warning in $RUN_LOG -- an operator would have no way to know they are now charged at full size"
	fi

	# --- an operator -reinit takes them with it ------------------------------
	before_count="$(rp_count "$rp_dir")"
	bench_sync "$sc" reinit-sweep -reinit "-retention=2,0"
	after_count="$(rp_count "$rp_dir")"
	# This very sync takes one of its own, so "swept" means the earlier ones
	# are gone rather than the directory being empty.
	if [ "$after_count" -lt "$before_count" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "an operator -reinit sweeps the restore points of the replica it replaces" "$fo_ok" \
		"count was $before_count before the reinit and $after_count after"

	return 0
}

# rp_count DIR -> how many PUBLISHED restore points are in DIR.
#
# Staging (".incomplete-") and unrecognised entries are excluded by the leading
# digit, which is what makes every count above an assertion about restore
# points rather than about directory contents.
rp_count() {
	ssh_host_cmd "$TARGET_HOST" "ls -1 '$1' 2>/dev/null | grep -c '^[0-9][0-9]*-' || true" | tr -d '[:space:]'
}

# rp_newest / rp_oldest -- sorted numerically, which works because a tag leads
# with the checkpoint instant precisely so it sorts.
rp_newest() {
	ssh_host_cmd "$TARGET_HOST" "ls -1 '$1' 2>/dev/null | grep '^[0-9][0-9]*-' | sort -n | tail -1 || true" | tr -d '[:space:]'
}

rp_oldest() {
	ssh_host_cmd "$TARGET_HOST" "ls -1 '$1' 2>/dev/null | grep '^[0-9][0-9]*-' | sort -n | head -1 || true" | tr -d '[:space:]'
}

rp_has() {
	[ -n "${2:-}" ] || return 1
	ssh_host_cmd "$TARGET_HOST" "test -d '$1/$2'" >/dev/null 2>&1
}

# rp_list DIR -> every published restore point tag, whitespace separated.
# Tags contain no spaces by construction (pkg/restorepoint refuses any
# checkpoint name that is not letters, digits, '.', '_' or '-'), so word
# splitting the result is safe.
rp_list() {
	ssh_host_cmd "$TARGET_HOST" "ls -1 '$1' 2>/dev/null | grep '^[0-9][0-9]*-' | sort -n || true" | tr '\n' ' '
}

# rp_root -> the SHARED restore point directory, which holds one subdirectory
# per target domain plus anything left flat in it by a pre-change vmsync.
#
# Nothing in this harness may rm -rf this path. It is shared by every target
# domain replicating into TARGET_DISK_PATH, and deleting it to get a clean slate
# would destroy a co-located replica's entire recovery history -- the same
# mistake stage 18 guards the engine against. Use rp_store_dir and clear one
# store.
rp_root() {
	printf '%s/.vmsync-rp' "${TARGET_DISK_PATH%/}"
}

# rp_store_dir [DOMAIN] -> one target domain's own restore point directory.
#
# Defaults to TARGET_DOMAIN. The segment is "vm-" plus the same reversible
# encoding the journal path uses (journal_safe_key, which is util.SafeKey):
# there is deliberately no second encoder here, because a shell copy that
# disagreed with the Go one would make every count in this stage read the wrong
# directory and then report a retention bug that does not exist. Stage 18
# asserts the agreement end to end, against the directory vmsync creates.
rp_store_dir() {
	printf '%s/vm-%s' "$(rp_root)" "$(journal_safe_key "${1:-$TARGET_DOMAIN}")"
}

# rp_plant_point DIR TAG SOURCE -- create a restore point directory by hand,
# with a sidecar naming SOURCE, as a sync would have left it.
#
# Used to stand up a CO-LOCATED domain's history without running a second
# replication pair. What the per-domain layout has to guarantee is that this
# domain's sync does not read, prune or delete those directories -- and a
# planted set proves that as well as a real one while being deterministic,
# which a second pair racing the first would not be.
rp_plant_point() {
	local dir="$1" tag="$2" source="$3" at="${2%%-*}"
	ssh_host_cmd "$TARGET_HOST" "mkdir -p '$dir/$tag' && printf '%s' '{\"checkpoint\":\"vmsync-cpt-000001\",\"checkpoint_at\":$at,\"taken_at\":$at,\"source\":\"$source\",\"verify\":\"not-run\",\"disks\":[\"planted.qcow2\"]}' > '$dir/$tag/status.json'" >/dev/null
}

# rp_clear_store [DOMAIN] -- remove ONE domain's restore points, leaving every
# other domain's and anything flat in the shared root untouched.
rp_clear_store() {
	local dir
	dir="$(rp_store_dir "${1:-$TARGET_DOMAIN}")"
	ssh_host_cmd "$TARGET_HOST" "rm -rf '$dir'" >/dev/null 2>&1 || true
}

# fs_free_bytes HOST PATH -> free bytes on the filesystem holding PATH.
#
# df, never du. A reflink copy shares extents and du counts a shared extent
# once for EVERY file referencing it, so du would report a directory of restore
# points at many times its real cost -- and would make the check above claim a
# reflink had consumed a full copy.
fs_free_bytes() {
	ssh_host_cmd "$1" "df -B1 --output=avail '$2' 2>/dev/null | tail -1" | tr -d '[:space:]'
}

# vmsync_verb SCENARIO PHASE ARGS... -- run one standalone vmsync verb against
# the target, logged like any other run.
#
# Not bench_sync: these verbs copy nothing and need no source, and passing them
# -source-uri would misrepresent what they touch. That they work with only
# target-side flags is itself part of what this stage checks -- an operator
# reaching for a restore point may have a source that is gone.

# --- Stage 10: -restore-restore-point (rolling a replica back) ---------------
#
# Stage 9 proves restore points get taken. This proves one can be put back, and
# almost every assertion here is about a way that could go wrong QUIETLY.
#
# The one worth reading twice is "the next sync refuses". A restore rolls the
# replica's contents backwards while the source's checkpoint chain marches on,
# and nothing in vmsync's incremental path looks at disk content -- it compares
# a checkpoint NAME. So a restore that failed to invalidate the target's
# replication metadata would leave the next scheduled sync applying the source's
# newest delta onto six-hour-old data, exiting 0, with green metrics. That run
# would look identical to a healthy one. It is the single failure this whole
# feature is designed around, and it is checked here by running the sync for
# real and asserting both that it refused AND that the restored bytes are still
# on disk afterwards.
stage_restore() {
	log "=== Stage 10: -restore-restore-point ==="
	local sc=restore
	local fo_ok=0

	if [ "$DRY_RUN" = yes ]; then
		bench_sync "$sc" baseline -reinit "-retention=3,0"
		bench_sync "$sc" second "-retention=3,0"
		fo_check "$sc" "the assessment changes nothing" 0
		fo_check "$sc" "the restore rolls the replica back" 0
		fo_check "$sc" "the next sync refuses" 0
		return 0
	fi

	stage_needs_target_shutoff "$CSV" "$sc" "stage restore" || return 0
	reset_pair_state "$sc" || return 0
	require_target_syncable "$sc" || return 0

	# This target domain's own store. Cleared alone, never the shared directory
	# above it: that one holds every co-located replica's history too.
	local rp_dir
	rp_dir="$(rp_store_dir)"
	rp_clear_store

	# --- two restore points, with different contents between them ------------
	bench_sync "$sc" baseline -reinit "-retention=3,0"
	if [ "$RUN_RC" != 0 ]; then
		if grep -q "does not support reflink copies" "$RUN_LOG" 2>/dev/null; then
			warn "SKIP stage restore: $TARGET_DISK_PATH on $TARGET_HOST does not support reflink copies"
			results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP target filesystem has no reflink support"
			return 0
		fi
		warn "baseline sync failed (see $RUN_LOG) -- aborting stage 10$(bench_sync_hint)"
		return 1
	fi

	local replica rp_old rp_new
	replica="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
	if [ -z "$replica" ]; then
		warn "SKIP stage restore: could not resolve the target disk path for dev='$TAMPER_DISK_DEV'"
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP target disk path unresolved"
		return 0
	fi
	rp_old="$(rp_newest "$rp_dir")"

	# Real guest writes, so the two restore points genuinely differ. Without
	# them an idle guest produces two byte-identical copies and every content
	# assertion below would pass without proving anything -- so their absence
	# is recorded as a SKIP rather than quietly weakening the stage.
	local contents_differ=yes
	# Same wait as stage 8, for the same reason: any stage in this list may
	# have restarted the source, and an unwaited agent turns a real assertion
	# into a silent SKIP.
	[ "$GUEST_DIRTY" = yes ] && { wait_for_guest_agent || true; }
	if [ "$GUEST_DIRTY" = yes ] && guest_exec_available; then
		guest_dirty || warn "guest write failed; the two restore points may end up identical"
	else
		contents_differ=no
	fi

	bench_sync "$sc" second "-retention=3,0"
	if [ "$RUN_RC" != 0 ]; then
		warn "second sync failed (see $RUN_LOG) -- aborting stage 10$(bench_sync_hint)"
		return 1
	fi
	rp_new="$(rp_newest "$rp_dir")"

	if [ -z "$rp_old" ] || [ -z "$rp_new" ] || [ "$rp_old" = "$rp_new" ]; then
		warn "SKIP stage restore: expected two distinct restore points, got '$rp_old' and '$rp_new'"
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP could not take two distinct restore points"
		return 0
	fi
	log "restore points: old=$rp_old new=$rp_new, replica=$replica"

	local disk_base old_copy new_copy
	disk_base="$(basename "$replica")"
	old_copy="$rp_dir/$rp_old/$disk_base"
	new_copy="$rp_dir/$rp_new/$disk_base"

	# A restore point must actually be a copy of the replica as it was. If this
	# fails, nothing below means anything.
	if files_same "$replica" "$new_copy"; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the newest restore point is a copy of the current replica" "$fo_ok" \
		"$replica and $new_copy differ"

	if [ "$contents_differ" = yes ]; then
		if files_same "$replica" "$old_copy"; then fo_ok=1; else fo_ok=0; fi
		fo_check "$sc" "the older restore point differs from the current replica" "$fo_ok" \
			"the guest writes between the two syncs did not reach the replica, so a rollback would be unobservable"
	else
		warn "SKIP the content-rollback assertions: guest-exec is unavailable, so both restore points hold identical data and a rollback could not be observed"
		results_row "$CSV" "$sc" content_rollback "" "" "" "" "" "" "SKIP guest-exec unavailable"
	fi

	# --- the assessment must change nothing ----------------------------------
	# -restore-restore-point without -force-restore is meant to be safe to run
	# against a production replica while deciding. If it wrote anything, the
	# operator's way of finding out what a restore would do would itself be the
	# restore.
	local cp_before role_before
	cp_before="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_checkpoint)"
	role_before="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"

	if vmsync_verb "$sc" assess -restore-restore-point "$rp_old"; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the assessment runs and reports" "$fo_ok" "see $RUN_LOG"

	if grep -q "$rp_old" "$RUN_LOG" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the assessment names the restore point it would apply" "$fo_ok" "see $RUN_LOG"

	# A restore replaces the replica's contents wholesale, which is the same
	# hazard a full copy carries, and it is armed the same way: the single
	# metadata write that happens between staging and swapping sets
	# replica_incomplete, and only the swap finishing withdraws it. Between
	# those two the disks are a mixture of two moments while the metadata
	# beside them names one coherent checkpoint -- so without the field a
	# half-swapped machine promotes with nothing to say it should not.
	#
	# The window is inside one process and cannot be sampled from outside, so
	# what is asserted here is the thing that CAN be: the plan the restore
	# applies before it touches a disk contains the field, and the assessment
	# says so. That matters on its own terms too -- the assessment is what an
	# operator reads before saying yes, and a promotion refusal arriving
	# unannounced mid-incident, on a domain they were told would be
	# promotable, is the one surprise this whole feature must not create.
	if grep -q 'replica_incomplete' "$RUN_LOG" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the assessment warns that the restore will arm a promotion refusal" "$fo_ok" \
		"replica_incomplete is not in the assessment's field list, so the restore either does not arm it -- leaving a half-swapped replica promotable -- or arms it without telling the operator it is about to. See $RUN_LOG"

	# Same assessment, without -target-disk-path. A restore refuses outright
	# without the target domain, so it can read the directory off the domain
	# instead -- and doing so uses the same rule the sync used to place the
	# restore points, which the flag only matches when it happens to name the
	# directory the disks are really in.
	if VERB_OMIT_DISK_PATH=yes vmsync_verb "$sc" assess-derived -restore-restore-point "$rp_old"; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the restore finds its restore points without -target-disk-path" "$fo_ok" \
		"it could not locate $rp_dir from the target domain's own disks -- see $RUN_LOG"

	if files_same "$replica" "$new_copy"; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the assessment leaves the replica untouched" "$fo_ok" \
		"$replica changed during an assessment that was supposed to write nothing"

	if [ "$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_checkpoint)" = "$cp_before" ] \
		&& [ "$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)" = "$role_before" ]; then
		fo_ok=0
	else
		fo_ok=1
	fi
	fo_check "$sc" "the assessment leaves replication metadata untouched" "$fo_ok" \
		"last_checkpoint or replication_role moved during an assessment"

	# --- role gating ---------------------------------------------------------
	# A domain failed over to and then shut down for maintenance passes every
	# runtime check there is: it is not running, its disks are there, its
	# metadata is intact. replication_role is the only thing that knows those
	# disks are live data rather than a replica.
	if vmsync_verb "$sc" role-set -update-role promoted >/dev/null 2>&1; then
		if vmsync_verb "$sc" role-refused -restore-restore-point "$rp_old" -force-restore; then
			fo_ok=1
		else
			fo_ok=0
		fi
		fo_check "$sc" "a restore is refused on a promoted domain" "$fo_ok" \
			"the restore was ALLOWED to overwrite a domain marked replication_role=promoted -- see $RUN_LOG"

		# And now the part the role cannot do.
		#
		# The refusal above rests on replication_role=promoted, and the first
		# thing anybody does with a promoted copy they want rid of is shut it
		# down -- which records `paused`. The restore gate deliberately PERMITS
		# paused, because rolling a paused replica back is the ordinary reason
		# the verb exists. So one operation turned the strongest refusal in this
		# stage into an allowance, and the copy that had been serving production
		# became restorable with nothing left on the domain to say otherwise.
		#
		# The durable record is what closes it. Asserted here rather than in
		# stage 6 because this is where a restore is actually attempted: stage 6
		# proves the record survives the demotion, and this proves that
		# surviving it changes the outcome.
		if vmsync_verb "$sc" demote-to-paused -update-role paused >/dev/null 2>&1; then
			if vmsync_verb "$sc" served-live-refused -restore-restore-point "$rp_old" -force-restore; then
				fo_ok=1
			else
				fo_ok=0
			fi
			fo_check "$sc" "a restore is still refused after the promoted copy is demoted to paused" "$fo_ok" \
				"the restore was ALLOWED over a copy that had been promoted, because shutting it down recorded 'paused' and the role gate permits paused -- -force-restore included; the disks it overwrote may have been the only copy of the data that was serving -- see $RUN_LOG"

			# The other half, and it is not optional: a refusal with no way past
			# it would make every replica that had ever been failed over
			# permanently unrestorable, which is a worse fault than the one
			# being fixed.
			if vmsync_verb "$sc" release-promotion -release-promotion >/dev/null 2>&1; then
				# Deliberately WITHOUT -force-restore: the assessment is
				# enough. Both gates run before it is printed, so a refusal
				# still refuses it, and this way the proof that the way back
				# opens does not itself replace the replica's disks -- which
				# would land in the middle of a stage whose own restore has not
				# run yet and whose later assertions compare these files.
				if vmsync_verb "$sc" restore-after-release -restore-restore-point "$rp_old"; then
					fo_ok=0
				else
					fo_ok=1
				fi
				fo_check "$sc" "and it is permitted again once the promotion is released" "$fo_ok" \
					"the restore was still refused after -release-promotion cleared the record, so there is no way back for any replica that has ever been failed over -- see $RUN_LOG"
			else
				warn "SKIP the release check: -release-promotion failed on the demoted copy"
				results_row "$CSV" "$sc" served_live_release "" "" "" "" "" "" "SKIP could not release the promotion"
			fi
		else
			warn "SKIP the served-live gate check: could not demote the promoted copy to paused"
			results_row "$CSV" "$sc" served_live_gate "" "" "" "" "" "" "SKIP could not demote to paused"
		fi

		clear_target_role "put replication_role back after the role gate check"
	else
		warn "SKIP the role gate check: could not set replication_role=promoted"
		results_row "$CSV" "$sc" role_gate "" "" "" "" "" "" "SKIP could not set the role"
	fi

	# --- the restore itself --------------------------------------------------
	local rp_checkpoint rp_checkpoint_at
	rp_checkpoint="$(rp_status_field "$rp_dir" "$rp_old" checkpoint)"
	rp_checkpoint_at="$(rp_status_field "$rp_dir" "$rp_old" checkpoint_at)"

	if vmsync_verb "$sc" restore -restore-restore-point "$rp_old" -force-restore; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the restore runs" "$fo_ok" "see $RUN_LOG"

	if [ "$contents_differ" = yes ]; then
		if files_same "$replica" "$old_copy"; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the replica now holds the restored point's contents" "$fo_ok" \
			"$replica does not match $old_copy after a restore that reported success"

		if files_same "$replica" "$new_copy"; then fo_ok=1; else fo_ok=0; fi
		fo_check "$sc" "the replica no longer holds what it held before" "$fo_ok" \
			"$replica still matches $new_copy, so nothing was actually rolled back"
	fi

	# The restore point must SURVIVE being restored, or an operator gets one
	# attempt: roll back to Tuesday, decide it is also bad, and Monday is gone
	# too. Restoring reflinks a copy rather than moving the original precisely
	# so this holds.
	if rp_has "$rp_dir" "$rp_old" && rp_has "$rp_dir" "$rp_new"; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "restoring consumes neither restore point" "$fo_ok" \
		"'$rp_old' or '$rp_new' is gone from $rp_dir after a restore"

	# --- the displaced contents ----------------------------------------------
	local aside
	aside="$(ssh_host_cmd "$TARGET_HOST" "ls -1 '${replica}'.vmsync-replaced-* 2>/dev/null | tail -1 || true" | tr -d '[:space:]')"
	if [ -n "$aside" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the displaced replica contents are kept" "$fo_ok" \
		"no ${replica}.vmsync-replaced-* on $TARGET_HOST, so the pre-restore contents are unrecoverable"

	if [ -n "$aside" ] && [ "$contents_differ" = yes ]; then
		if files_same "$aside" "$new_copy"; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the kept contents are what the replica held before the restore" "$fo_ok" \
			"$aside does not match $new_copy"
	fi

	# --- the metadata now describes the restored point -----------------------
	local got
	got="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
	if [ "$got" = paused ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the restore pauses replication" "$fo_ok" \
		"replication_role is '$got', not 'paused' -- the next scheduled sync would overwrite the restored data"

	got="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_checkpoint)"
	if [ -n "$rp_checkpoint" ] && [ "$got" = "$rp_checkpoint" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "last_checkpoint names the restored point's checkpoint" "$fo_ok" \
		"last_checkpoint is '$got', the restore point's sidecar says '$rp_checkpoint'"

	got="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" checkpoint_at)"
	if [ -n "$rp_checkpoint_at" ] && [ "$got" = "$rp_checkpoint_at" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "checkpoint_at is rewound, so a promotion measures loss honestly" "$fo_ok" \
		"checkpoint_at is '$got', the restore point's sidecar says '$rp_checkpoint_at'"

	got="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" source_stopped_at_sync)"
	if [ -z "$got" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "source_stopped_at_sync is cleared" "$fo_ok" \
		"source_stopped_at_sync is still '$got' -- a promotion would report a VERIFIED zero data loss for rolled-back data"

	# The other end of the window the assessment announced. Every disk is
	# swapped and every one has an owner qemu can open, so the replica on
	# these files is the restore point entire -- and the refusal has to be
	# withdrawn, because a restore is very often done precisely in order to
	# promote. A field left standing here turns the deliberate rollback an
	# operator performed into a replica they then have to force past, with a
	# message about an interrupted copy that in fact completed.
	got="$(replica_incomplete "$TARGET_URI" "$TARGET_DOMAIN")"
	if [ -z "$got" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the restore withdraws its own promotion refusal once the swap is done" "$fo_ok" \
		"replica_incomplete is still '$got' after a restore that reported success -- this replica is healthy and -promote will refuse it until a sync clears the field, which a paused target first has to be taken out of pause to run"

	# The three fields pkg/failover reads as evidence that a sync landed. A
	# restore that cleared them would leave a replica that cannot be promoted
	# without -force-promote, which is the one thing an operator restores in
	# order to do.
	local ev_cp ev_sync ev_fail
	ev_cp="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_checkpoint)"
	ev_sync="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_sync_timestamp)"
	ev_fail="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" failure_count)"
	if [ -n "$ev_cp" ] && [ -n "$ev_sync" ] && [ "${ev_fail:-0}" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the restored replica still satisfies the promotion evidence checks" "$fo_ok" \
		"last_checkpoint='$ev_cp' last_sync_timestamp='$ev_sync' failure_count='$ev_fail' -- promoting would need -force-promote, and promoting is what a restore is for"

	# --- the whole point: the next sync must not silently corrupt it ---------
	#
	# -reinit-after-failures is passed deliberately, and it is not decoration.
	# A restored replica is paused, so every scheduled sync is refused -- and
	# if a refusal counted as a sync failure, the counter would climb on a
	# domain nobody is trying to sync, eventually reach the threshold, and
	# force a reinit that this same gate refuses. Worse, a non-zero
	# failure_count is itself one of the things that blocks a promotion, so
	# the replica an operator restored in order to promote would quietly stop
	# being promotable. The assertions below check the refusal AND that it
	# left no mark.
	bench_sync "$sc" next-sync "-reinit-after-failures=2"
	if [ "$RUN_RC" != 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the next ordinary sync refuses to run against a restored replica" "$fo_ok" \
		"a sync exited 0 against a replica whose contents were rolled back -- it applied a delta onto the wrong baseline and reported success. See $RUN_LOG"

	if [ "$contents_differ" = yes ]; then
		if files_same "$replica" "$old_copy"; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the restored contents survive the refused sync" "$fo_ok" \
			"$replica no longer matches $old_copy, so the refused sync still wrote to it"
	fi

	got="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" failure_count)"
	if [ "${got:-0}" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "a refused sync is not counted against the restored replica" "$fo_ok" \
		"failure_count is '$got' after one refused sync with -reinit-after-failures=2 -- refusals are being counted as sync failures, so a paused replica climbs to the threshold and stops being promotable"

	got="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
	if [ "$got" = paused ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the refused sync leaves the pause in place" "$fo_ok" \
		"replication_role is '$got' after the refused sync, not 'paused'"

	# --- cleanup: hand the pair back the way the stage found it --------------
	# The role MUST be cleared before the healing -reinit, not after: a paused
	# target is refused at the sync's very first guard, so a reinit run while
	# it is still paused would fail without healing anything -- and the next
	# stage would inherit a paused target holding rolled-back contents.
	clear_target_role "clear replication_role=paused after the restore"
	bench_sync "$sc" heal -reinit
	if [ "$RUN_RC" != 0 ]; then
		warn "the healing -reinit after stage 10 did not succeed (see $RUN_LOG) -- the target is left holding restored (older) contents and paused/target role as last set. Inspect it before trusting later stages"
	fi
	ssh_host_cmd "$TARGET_HOST" "rm -f '${replica}'.vmsync-replaced-*" >/dev/null 2>&1 || true

	return 0
}

# clear_target_role WHY -- put the target back to a role that permits syncing.
#
# Tries vmsync first, because -update-role is what an operator would run and
# exercising it here is worth something. Falls back to virsh for the reason
# reset_pair_state gives at length: a harness that can only restore state
# through the code it is testing cannot recover when that code is broken, and
# the failure then surfaces two stages later as an unrelated-looking baseline
# error. Every stage after this one syncs into the target, and every one of them
# is refused while it is paused, so this must not be able to leave it that way.
#
# Removing the whole metadata block is heavier than clearing one field and is
# fine: the caller -reinits immediately afterwards, which writes it all back.
clear_target_role() {
	local why="$1"
	if vmsync_verb restore role-clear -update-role target >/dev/null 2>&1; then
		return 0
	fi
	warn "vmsync -update-role failed while trying to $why; removing the target's vmsync metadata with virsh instead, so later stages are not left refusing to sync into a paused target"
	virsh_uri "$TARGET_URI" metadata "$TARGET_DOMAIN" --uri "$VMSYNC_METADATA_URI" --remove --config >/dev/null 2>&1 \
		|| warn "that failed too -- $TARGET_DOMAIN on $TARGET_HOST is left with replication_role set and every later sync into it will be refused. Clear it by hand"
	return 0
}

# files_same PATH_A PATH_B -- are these two files byte-identical on the target?
#
# cmp, not md5sum: it short-circuits on the first differing byte, which is the
# common case for the "these must differ" assertions, and it needs no second
# round trip to compare digests. Both paths are on the target host, so this is
# one ssh call.
#
# Exits 0 for identical and non-zero for anything else INCLUDING a missing file,
# so every caller's failure message names the two paths -- a comparison that
# could not be made is not evidence of sameness.
files_same() {
	ssh_host_cmd "$TARGET_HOST" "cmp -s '$1' '$2'" >/dev/null 2>&1
}

# rp_status_field DIR TAG KEY -- one value out of a restore point's sidecar.
#
# sed rather than a JSON parser because jq is not in this harness's dependency
# list and adding one for six fields would be a poor trade. Status.Encode writes
# the sidecar with MarshalIndent, so every field really is on its own line in
# the "key": value shape this matches -- which is a fact about vmsync's own
# writer, not a hope about JSON in general.
rp_status_field() {
	ssh_host_cmd "$TARGET_HOST" "cat '$1/$2/status.json' 2>/dev/null || true" \
		| sed -n "s/^[[:space:]]*\"$3\":[[:space:]]*\"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}[[:space:]]*$/\1/p" \
		| head -1 | tr -d '[:space:]'
}


# --- Stage 13: pre-commit integrity check ------------------------------------
#
# The check hashes every chunk the copy reads, has vmsync-bridge-helper hash
# the same ranges back off the target, and refuses to commit an incremental
# sync's overlay if they disagree. It is ON by default whenever a matching
# helper is present, which makes it the single most important thing in this
# harness to have a NEGATIVE test for: a check that silently never fires is
# indistinguishable from a check that works, and every other stage would keep
# reporting PASS either way.
#
# Corrupting the data itself is not an option here. The check reads the target
# back between the copy and the commit, inside one vmsync process, so there is
# no moment an outside tamper could land in. What CAN be substituted from
# outside is the helper: vmsync invokes it by path (-bridge-helper-path), so a
# wrapper script that runs the real binary and then edits its reply exercises
# the whole comparison -- request, response, header check, digest comparison,
# and the failure handling -- without needing a fault injected into vmsync.
#
# Four sub-tests, each asking a question the others cannot:
#
#   13a checksum/clean   -- an ordinary sync says the check RAN and matched.
#                           Without this, the three below could all pass on a
#                           run where the check never happened.
#   13b checksum/mismatch-- a helper reporting one wrong digest must fail the
#                           run, and the overlay must be GONE while the base
#                           keeps its previous contents.
#   13c checksum/stale   -- a helper too old to emit a format header must be
#                           reported as version skew, NOT as a corrupt
#                           replica. This is the distinction that decides
#                           whether an estate reinits a healthy 50 GiB VM on
#                           false evidence after a partial upgrade.
#   13d checksum/off     -- -no-checksum must actually skip it, so the flag is
#                           a real escape hatch rather than a no-op.
#
# Not in the default stage list: 13b deliberately fails a sync, and all four
# temporarily replace the helper on the target host.

# CHECKSUM_SHIM_DIR holds the wrapper scripts on the TARGET host. Under /tmp
# rather than beside the real helper: this must never risk overwriting the
# operator's actual deployed binary, and vmsync is pointed at the wrapper by
# flag rather than the wrapper being installed over anything.
CHECKSUM_SHIM_DIR="/tmp/vmsync-bench-checksum-shim"

# checksum_real_helper -> the helper the shims wrap, which is the one every
# vmsync this harness launches is pointed at.
checksum_real_helper() {
	bridge_helper_path
}

# checksum_install_shim NAME AWK_PROGRAM -- writes a wrapper at
# $CHECKSUM_SHIM_DIR/NAME that runs the real helper and pipes its stdout
# through AWK_PROGRAM, then prints the wrapper's path.
#
# Only -checksum invocations are rewritten. The same binary also serves the
# bridge relay for -compress, and mangling that would break the transfer
# instead of the check -- so anything without -checksum is exec'd through
# untouched.
checksum_install_shim() {
	local name="$1" awk_prog="$2" real
	real="$(checksum_real_helper)"

	# Written with a quoted heredoc through run_shell_on so the target's
	# shell sees the script verbatim -- no local expansion of $@, $1 or the
	# awk program's own $2/$3 field references.
	#
	# "no" rather than a TARGET_LOCAL: there is no such setting, because this
	# harness runs alongside the SOURCE (SOURCE_LOCAL, default yes) and reaches
	# the target over ssh in every other place too.
	#
	# Three things in the script below are deliberate, and the first two were
	# learned the hard way:
	#
	# NOT "exec real | awk". exec applies to the LEFT SUBSHELL of a pipeline,
	# not to the shim, so control returned and the trailing exec ran the
	# helper a SECOND time -- against a stdin the first invocation had already
	# drained. Over ssh stdin is an unseekable pipe, so the second read got
	# EOF and the helper died with "no header line at all", which looked
	# exactly like a broken helper rather than a broken shim.
	#
	# A temp file rather than a pipe, so the helper's exit status is the
	# shim's. In "real | awk" the pipeline's status is awk's, and awk always
	# succeeds -- a genuinely failing helper would surface as an empty but
	# well-formed response, which vmsync reports as a plan mismatch instead
	# of the real error. pipefail would fix it too but is not POSIX sh.
	#
	# An explicit exit after handling -checksum, so nothing can fall through
	# to the pass-through exec below.
	run_shell_on "$TARGET_HOST" no "mkdir -p '$CHECKSUM_SHIM_DIR' && cat > '$CHECKSUM_SHIM_DIR/$name' <<'VMSYNC_SHIM_EOF'
#!/bin/sh
for a in \"\$@\"; do
	if [ \"\$a\" = -checksum ]; then
		out=\$(mktemp) || exit 1
		$real \"\$@\" >\"\$out\"
		rc=\$?
		if [ \"\$rc\" -ne 0 ]; then
			rm -f \"\$out\"
			exit \"\$rc\"
		fi
		awk '$awk_prog' <\"\$out\"
		rc=\$?
		rm -f \"\$out\"
		exit \"\$rc\"
	fi
done
exec $real \"\$@\"
VMSYNC_SHIM_EOF
chmod +x '$CHECKSUM_SHIM_DIR/$name'" >/dev/null || return 1

	printf '%s\n' "$CHECKSUM_SHIM_DIR/$name"
}

checksum_remove_shims() {
	[ "$DRY_RUN" = yes ] && return 0
	run_shell_on "$TARGET_HOST" no "rm -rf '$CHECKSUM_SHIM_DIR'" >/dev/null 2>&1 || true
}

# checksum_write_args -- echoes the extra vmsync args a sub-test needs so its
# sync actually WRITES something, and dirties the guest first when that is the
# route being used.
#
# The whole stage depends on this: with nothing to copy, the check never runs
# at all (see CHECKSUM_INCREMENTAL). Printing the args rather than running the
# sync keeps the four sub-tests free to add their own flags.
checksum_write_args() {
	if [ "$CHECKSUM_INCREMENTAL" = yes ]; then
		# A failure to dirty is not fatal -- the sync still runs, and the
		# sub-test's own "did the check happen" assertion is what reports
		# the resulting no-op honestly.
		guest_dirty || warn "could not dirty the guest; this sub-test's sync may find nothing to copy"
		return 0
	fi
	printf '%s\n' -reinit
}

# checksum_target_digest_of PATH -> a digest of the target replica's guest
# content, used to prove a refused run left the base alone.
#
# qemu-img's own map output rather than a checksum of the file: two qcow2
# files with identical guest content differ byte for byte, and this only has
# to detect "the base changed", for which the allocation map plus the disk
# size is enough and is far cheaper than reading the image.
checksum_target_digest_of() {
	ssh_host_cmd "$TARGET_HOST" "qemu-img map --output=json -U '$1' 2>/dev/null | md5sum" 2>/dev/null | awk '{print $1}'
}

stage_checksum() {
	log "=== Stage 13: pre-commit integrity check (digest exchange with the target) ==="

	local target_path=""
	if [ "$DRY_RUN" != yes ]; then
		stage_needs_target_shutoff "$CSV" checksum "stage checksum" || return 0
		if ! command -v awk >/dev/null 2>&1; then
			warn "SKIP stage 13: awk is required to build the helper shims"
			results_row "$CSV" checksum precondition "" "" "" "" "" "" "SKIP awk unavailable"
			return 0
		fi
	fi

	# CHECKSUM_INCREMENTAL decides how every sub-test below gets vmsync to
	# WRITE something, and that is the whole difficulty of this stage.
	#
	# The check only runs when a copy actually happened: copyAndCommit returns
	# early at "No changed extents selected, skipping copy" before the overlay
	# is even created, so on an idle source an incremental sync is a complete
	# no-op -- no digests, no exchange, the helper never invoked, and nothing
	# in the log either way. Assuming a baseline-then-incremental pair produces
	# a delta would leave all three interesting sub-tests silently testing
	# nothing against a quiet test VM.
	#
	# So: dirty the guest when the agent allows it, which gives a genuine
	# incremental and is the only way to reach the overlay-is-discarded
	# behaviour 13b wants. Failing that, fall back to -reinit, which always
	# writes the whole disk and still exercises the digest exchange, the
	# version check and the refusal -- just against a base image rather than
	# an overlay. Slower and narrower, but a real test rather than a vacuous
	# pass.
	CHECKSUM_INCREMENTAL=no
	if [ "$DRY_RUN" != yes ] && [ "$GUEST_DIRTY" = yes ] && guest_exec_available; then
		CHECKSUM_INCREMENTAL=yes
		log "guest-exec is available: sub-tests will dirty the guest and run genuine incrementals"
	elif [ "$DRY_RUN" != yes ]; then
		warn "sub-tests will use -reinit instead of incrementals: ${GUEST_EXEC_WHY:-guest dirtying is disabled (GUEST_DIRTY=no)}. The digest exchange, the version check and the refusal are all still tested; what is NOT is that a refused INCREMENTAL discards its overlay and leaves the base intact, which needs a real delta."
	fi

	bench_sync checksum baseline -reinit
	if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
		warn "baseline full sync failed (see $RUN_LOG) -- aborting stage 13$(bench_sync_hint)"
		return 1
	fi
	if [ "$DRY_RUN" != yes ]; then
		target_path="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
	fi

	# Everything below installs shims on the target; make sure they go away
	# whichever way this stage ends.
	trap 'checksum_remove_shims' RETURN

	checksum_clean_subtest
	checksum_mismatch_subtest "$target_path"
	checksum_stale_helper_subtest
	checksum_disabled_subtest
	# Last, because it is the only sub-test whose corruption is REAL: on the
	# -reinit fallback there is no overlay to discard, so it leaves the base
	# damaged and heals after itself.
	checksum_real_corruption_subtest "$target_path"
	return 0
}

# 13a: the check must actually run on an ordinary sync and report a match.
#
# The load-bearing sub-test. If the check is silently disabled -- no helper,
# a version mismatch, a flag typo -- then 13b and 13c would pass for the wrong
# reason and this harness would certify an integrity check that does nothing.
checksum_clean_subtest() {
	log "--- 13a checksum/clean: an ordinary sync must run the check and match ---"

	local -a extra
	mapfile -t extra < <(checksum_write_args)
	if [ ${#extra[@]} -gt 0 ]; then
		bench_sync checksum clean "${extra[@]}"
	else
		bench_sync checksum clean
	fi
	if [ "$DRY_RUN" = yes ]; then
		results_row "$CSV" checksum clean-result DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi

	if [ "$RUN_RC" != 0 ]; then
		warn "FAIL: an ordinary incremental sync failed with the integrity check enabled (exit=$RUN_RC) -- see $RUN_LOG"
		results_row "$CSV" checksum clean-result 1 "" "" "" "" "" "FAIL clean sync failed"
	elif grep -q 'pre-commit integrity check SKIPPED' "$RUN_LOG" 2>/dev/null; then
		warn "FAIL: the integrity check was SKIPPED, so nothing in this stage tests it. vmsync needs a vmsync-bridge-helper on $TARGET_HOST whose -version matches the vmsync being tested; see the log line for which it was. $RUN_LOG"
		results_row "$CSV" checksum clean-result 1 "" "" "" "" "" "FAIL check skipped (helper missing or version-mismatched)"
	elif grep -q 'checksum: target contents match what was sent' "$RUN_LOG" 2>/dev/null; then
		log "   PASS: the check ran and the target's digests matched"
		results_row "$CSV" checksum clean-result 0 "" "" "" "" "" "PASS check ran and matched"
	elif grep -q 'checksum: nothing written, skipping' "$RUN_LOG" 2>/dev/null; then
		# The copy ran but wrote nothing. Honest rather than a pass: the
		# check did not verify anything, and saying so is what keeps this
		# sub-test's own PASS meaningful.
		warn "SKIP: the copy found nothing to write, so the check had nothing to verify. See $RUN_LOG"
		results_row "$CSV" checksum clean-result "" "" "" "" "" "" "SKIP nothing written to check"
	elif grep -q 'No changed extents selected, skipping copy' "$RUN_LOG" 2>/dev/null; then
		# The sync returned before the copy even started, which is earlier
		# than the check lives -- so the log says nothing about it at all.
		# Named separately from the case above because the remedy differs:
		# this one is "the source is not changing", not "the check is broken".
		warn "SKIP: the sync found no changed extents and returned before any copy, so the check was never reached. Enable guest dirtying (GUEST_DIRTY=yes plus a working guest agent) or accept the -reinit fallback. See $RUN_LOG"
		results_row "$CSV" checksum clean-result "" "" "" "" "" "" "SKIP no changed extents, copy never ran"
	else
		warn "FAIL: the sync succeeded but said nothing about the integrity check either way -- neither a match, a skip, nor an absent delta. See $RUN_LOG"
		results_row "$CSV" checksum clean-result 1 "" "" "" "" "" "FAIL no checksum outcome in the log"
	fi
}

# 13b: a helper reporting one wrong digest must fail the run, remove the
# overlay, and leave the base as it was.
checksum_mismatch_subtest() {
	local base_path="$1" shim before after

	log "--- 13b checksum/mismatch: a single falsified digest must refuse the commit ---"

	if [ "$DRY_RUN" = yes ]; then
		results_row "$CSV" checksum mismatch-result DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi

	# Flip the digest on the FIRST data line only. One wrong block is the
	# case that actually matters -- a wholesale corruption would be caught by
	# almost anything, while a single block is what a real bad write looks
	# like. NR==1 is the format header and must pass through untouched, or
	# this would test the header path (13c) instead of the digest path.
	shim="$(checksum_install_shim mismatch 'NR==1 {print; next} NR==2 {print $1, $2, ($3+1); next} {print}')" || {
		warn "SKIP 13b: could not install the helper shim on $TARGET_HOST"
		results_row "$CSV" checksum mismatch-result "" "" "" "" "" "" "SKIP shim install failed"
		return 0
	}

	before="$(checksum_target_digest_of "$base_path")"

	local -a extra
	mapfile -t extra < <(checksum_write_args)
	if [ ${#extra[@]} -gt 0 ]; then
		bench_sync checksum mismatch -bridge-helper-path "$shim" "${extra[@]}"
	else
		bench_sync checksum mismatch -bridge-helper-path "$shim"
	fi

	after="$(checksum_target_digest_of "$base_path")"

	# Distinguish "the check did not fire" from "there was nothing to check".
	# Both leave RUN_RC at 0, and calling the second a FAIL would blame the
	# integrity check for an idle source.
	if grep -qE 'No changed extents selected, skipping copy|checksum: nothing written, skipping' "$RUN_LOG" 2>/dev/null; then
		warn "SKIP: this sync wrote nothing, so there were no digests to falsify and the check was never exercised. See $RUN_LOG"
		results_row "$CSV" checksum mismatch-result "" "" "" "" "" "" "SKIP nothing written, check not exercised"
		return 0
	fi

	if [ "$RUN_RC" = 0 ]; then
		warn "FAIL: the sync SUCCEEDED although the target reported a digest that does not match what was sent. The pre-commit integrity check did not fire, so a bad write would be committed silently. See $RUN_LOG"
		results_row "$CSV" checksum mismatch-result 1 "" "" "" "" "" "FAIL falsified digest not detected"
		return 0
	fi

	if ! grep -q 'do not match the bytes sent' "$RUN_LOG" 2>/dev/null; then
		warn "FAIL: the sync failed, but not with a digest mismatch -- so it failed for some other reason and this sub-test proved nothing. See $RUN_LOG"
		results_row "$CSV" checksum mismatch-result 1 "" "" "" "" "" "FAIL failed for another reason"
		return 0
	fi

	# Anything past here is INCREMENTAL-only. On the -reinit fallback there is
	# no overlay to discard and the base is legitimately rewritten from
	# scratch, so both remaining assertions would be meaningless -- and the
	# base-unchanged one would fail outright, blaming the check for doing
	# exactly what a full sync is supposed to do.
	if [ "$CHECKSUM_INCREMENTAL" != yes ]; then
		log "   PASS: the run failed on the digest mismatch (full-sync mode: no overlay to discard, so that half is untested here)"
		results_row "$CSV" checksum mismatch-result 0 "" "" "" "" "" "PASS mismatch refused (full sync; overlay discard untested)"
		return 0
	fi

	# The overlay must be gone. Its name is the base plus "_" plus the parent
	# checkpoint, so match on the prefix rather than trying to predict which
	# checkpoint this run used.
	local leftovers
	leftovers="$(ssh_host_cmd "$TARGET_HOST" "ls -1 '${base_path}'_* 2>/dev/null" 2>/dev/null || true)"

	if [ -n "$leftovers" ]; then
		warn "FAIL: the check refused the commit but LEFT the overlay behind: $(printf '%s' "$leftovers" | tr '\n' ' ') -- every failed run would leak a delta-sized file. See $RUN_LOG"
		results_row "$CSV" checksum mismatch-result 1 "" "" "" "" "" "FAIL overlay leaked on refusal"
	elif [ -n "$before" ] && [ "$before" != "$after" ]; then
		warn "FAIL: the check refused the commit but the base image CHANGED anyway (allocation map digest $before -> $after). A refused run must leave the replica exactly as it was. See $RUN_LOG"
		results_row "$CSV" checksum mismatch-result 1 "" "" "" "" "" "FAIL base changed despite refusal"
	else
		log "   PASS: the run failed on the digest mismatch, the overlay is gone, and the base is unchanged"
		results_row "$CSV" checksum mismatch-result 0 "" "" "" "" "" "PASS mismatch refused, overlay removed, base intact"
	fi
}

# 13e: the check must catch REAL corruption, not just a falsified reply.
#
# The sub-test that makes 13b mean something. 13b edits the helper's ANSWER, so
# it proves vmsync refuses a commit when told the digests disagree -- the
# plumbing. It cannot prove the check would notice actual wrong bytes, and the
# ways it could fail to are not exotic: vmsync hashing ranges other than the
# ones it wrote, the helper hashing the overlay's BACKING file instead of the
# overlay, an off-by-one in the range plan. Every one of those passes 13b and
# commits corruption in production.
#
# So this one corrupts the bytes for real, via -test=corrupt-before-checksum,
# which vmsync injects in the window between the copy finishing and the digest
# check reading it back -- a window nothing outside the process can reach. It
# writes inside a range the run actually wrote, because the check only hashes
# those: a fixed offset would fall outside the plan on any small incremental
# and sail through unnoticed, which would look like a pass.
#
# The assertion this adds over 13b is the important one: the corrupted bytes
# must NOT be in the base afterwards. That is checked directly, by reading the
# exact offset vmsync logged back off the base and requiring the pattern NOT to
# be there -- stronger than 13b's allocation-map digest, which would miss an
# in-place overwrite of already-allocated clusters.
checksum_real_corruption_subtest() {
	local base_path="$1" off len line heal=no

	log "--- 13e checksum/real-corruption: genuinely wrong bytes on the target must be caught ---"

	if [ "$DRY_RUN" = yes ]; then
		results_row "$CSV" checksum real-corruption-result DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi

	local -a extra
	mapfile -t extra < <(checksum_write_args)
	if [ ${#extra[@]} -gt 0 ]; then
		bench_sync checksum real-corruption -test=corrupt-before-checksum "${extra[@]}"
	else
		bench_sync checksum real-corruption -test=corrupt-before-checksum
	fi

	# Same distinction 13b draws, and for the same reason: an idle source that
	# copied nothing is not a failure of the integrity check. vmsync refuses
	# the fault outright in that case, so the message is its own.
	if grep -qE 'No changed extents selected, skipping copy|checksum: nothing written, skipping|there is no hashed range to corrupt' "$RUN_LOG" 2>/dev/null; then
		warn "SKIP: this sync wrote nothing, so there was no hashed range to corrupt and the check was never exercised. See $RUN_LOG"
		results_row "$CSV" checksum real-corruption-result "" "" "" "" "" "" "SKIP nothing written, check not exercised"
		return 0
	fi
	if grep -q 'needs qemu-io on the target host' "$RUN_LOG" 2>/dev/null; then
		warn "SKIP: the target host has no qemu-io, so the fault could not be injected. See $RUN_LOG"
		results_row "$CSV" checksum real-corruption-result "" "" "" "" "" "" "SKIP qemu-io missing on the target"
		return 0
	fi

	if [ "$RUN_RC" = 0 ]; then
		warn "FAIL: the sync SUCCEEDED although vmsync had written garbage into the image it was about to check. This is the failure 13b cannot see: the check reacts to a falsified REPLY but does not notice genuinely wrong BYTES, so a real bad write would be committed silently. See $RUN_LOG"
		results_row "$CSV" checksum real-corruption-result 1 "" "" "" "" "" "FAIL real corruption not detected"
		return 0
	fi
	if ! grep -q 'do not match the bytes sent' "$RUN_LOG" 2>/dev/null; then
		warn "FAIL: the sync failed, but not with a digest mismatch -- so it failed for some other reason and this sub-test proved nothing. See $RUN_LOG"
		results_row "$CSV" checksum real-corruption-result 1 "" "" "" "" "" "FAIL failed for another reason"
		return 0
	fi

	# Everything past here is INCREMENTAL-only, exactly as in 13b: the -reinit
	# fallback writes the base directly, so there is no overlay to discard and
	# the base is legitimately left corrupted. That case heals on the way out.
	if [ "$CHECKSUM_INCREMENTAL" != yes ]; then
		log "   PASS: real corruption was detected (full-sync mode: no overlay, so the base is genuinely damaged and will be healed)"
		results_row "$CSV" checksum real-corruption-result 0 "" "" "" "" "" "PASS real corruption detected (full sync; base damaged, healed)"
		heal_target checksum real-corruption-heal "$base_path"
		return 0
	fi

	# The offset vmsync chose, taken from its own log rather than guessed: it
	# comes from the digest plan and is different every run.
	#
	# WHICH disk carries it is not predictable on a multi-disk domain, and
	# that is not a flaw in the fault: every disk injects inside its own
	# worker, the workers race, and the first one to reach the digest check
	# fails the whole run before the others get that far. On hap01l the 2MiB
	# delta on vdb beats the 89-extent delta on vda every time. Filtering the
	# log by a base chosen in advance therefore found nothing whenever the
	# race went to another disk, and the sub-test skipped without asserting.
	#
	# So the disk is read OUT of the injection line instead of imposed on it:
	# whichever disk won, that is the one whose base must not contain the
	# pattern. The overlay is named "<base>_<parent checkpoint>", so stripping
	# that suffix gives the base to check.
	line="$(grep 'deliberately corrupting' "$RUN_LOG" 2>/dev/null | head -1 || true)"
	if [ -z "$line" ]; then
		warn "SKIP: the run refused the commit, but no injection line was found in the log at all, so no base can be checked at the corrupted offset. See $RUN_LOG"
		results_row "$CSV" checksum real-corruption-result 0 "" "" "" "" "" "PASS real corruption detected (no injection line logged)"
		return 0
	fi
	local hit_overlay hit_base
	hit_overlay="$(printf '%s' "$line" | sed -n 's/.*[[:space:]]image=\([^ ]*\).*/\1/p')"
	hit_base="$(printf '%s' "$hit_overlay" | sed 's/_vmsync-cpt-[0-9]\{1,\}$//')"
	if [ -z "$hit_overlay" ] || [ "$hit_base" = "$hit_overlay" ]; then
		# An incremental run that injected into something not named like an
		# overlay. Reporting it beats asserting against a path this did not
		# parse -- a wrong path makes qemu-io fail, which reads as a pass.
		warn "SKIP: the injection line names '${hit_overlay:-<unparsed>}', which is not an overlay of the form <base>_vmsync-cpt-NNNNNN, so the base to check cannot be derived. See $RUN_LOG"
		results_row "$CSV" checksum real-corruption-result 0 "" "" "" "" "" "PASS real corruption detected (overlay path not parsable)"
		return 0
	fi
	log "   the injection landed on $hit_overlay; checking its base $hit_base"
	off="$(printf '%s' "$line" | sed -n 's/.*[[:space:]]offset=\([0-9]*\).*/\1/p')"
	len="$(printf '%s' "$line" | sed -n 's/.*[[:space:]]length=\([0-9]*\).*/\1/p')"

	# Every disk's overlay, not just the corrupted one's. Since the commit
	# barrier a refusal on ONE disk must discard the overlays of ALL of them,
	# so a leak on the disks that copied cleanly is exactly the regression
	# worth catching here, and checking only the failed disk would miss it.
	local leftovers="" tdev tpath found
	while read -r tdev; do
		[ -n "$tdev" ] || continue
		tpath="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$tdev")" || true
		[ -n "$tpath" ] || continue
		found="$(ssh_host_cmd "$TARGET_HOST" "ls -1 '${tpath}'_* 2>/dev/null" 2>/dev/null || true)"
		[ -n "$found" ] && leftovers="${leftovers}${leftovers:+ }$(printf '%s' "$found" | tr '\n' ' ')"
	done < <(source_qcow_devs)

	if [ -n "$leftovers" ]; then
		warn "FAIL: the check refused the commit but LEFT overlay(s) behind: $leftovers -- every failed run would leak a delta-sized file, and since the commit barrier a refusal must discard the overlays of every disk, not just the one that failed. See $RUN_LOG"
		results_row "$CSV" checksum real-corruption-result 1 "" "" "" "" "" "FAIL overlay leaked on refusal"
		return 0
	fi

	# The direct assertion. `read -P` SUCCEEDS when the data matches the
	# pattern, so success here means the corruption reached the base and the
	# refusal did not protect it -- which is why the test is inverted.
	if [ -n "$off" ] && [ -n "$len" ] && ssh_host_cmd "$TARGET_HOST" qemu-io -r -t none -f qcow2 \
		-c "'read -P 0xa5 ${off} ${len}'" "'${hit_base}'" >/dev/null 2>&1; then
		warn "FAIL: the check refused the commit, but the corrupted pattern IS PRESENT in $hit_base at offset $off length $len -- the refusal did not stop the bad bytes reaching the replica, which is the one thing it exists to do. See $RUN_LOG"
		results_row "$CSV" checksum real-corruption-result 1 "" "" "" "" "" "FAIL corruption reached the base despite refusal"
		heal=yes
	else
		log "   PASS: real corruption was detected, no overlay was left behind, and $hit_base does not contain it at offset $off"
		results_row "$CSV" checksum real-corruption-result 0 "" "" "" "" "" "PASS real corruption refused, overlay removed, base clean at the corrupted offset"
	fi

	# Only when the replica may actually be damaged. On the passing path the
	# base was just proven untouched at the one offset that was written to, so
	# a full resync would cost a copy to fix nothing.
	if [ "$heal" = yes ]; then
		heal_target checksum real-corruption-heal "$hit_base"
	fi
}

# 13c: a helper too old to speak the format must be reported as SKEW, never as
# corruption.
#
# The most consequential distinction in the whole feature. Both failures stop
# the run, so a test that only checked the exit code would pass either way --
# what matters is which conclusion an operator draws. Told "version skew" they
# redeploy a binary; told "your replica does not match" they may reinit a
# perfectly good 50 GiB VM on evidence that was never about the data.
checksum_stale_helper_subtest() {
	local shim

	log "--- 13c checksum/stale: a headerless reply must read as version skew, not corruption ---"

	if [ "$DRY_RUN" = yes ]; then
		results_row "$CSV" checksum stale-result DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi

	# Drop the format header, which is exactly what a helper predating it
	# would produce: bare digest lines and nothing else.
	shim="$(checksum_install_shim stale 'NR==1 {next} {print}')" || {
		warn "SKIP 13c: could not install the helper shim on $TARGET_HOST"
		results_row "$CSV" checksum stale-result "" "" "" "" "" "" "SKIP shim install failed"
		return 0
	}

	local -a extra
	mapfile -t extra < <(checksum_write_args)
	if [ ${#extra[@]} -gt 0 ]; then
		bench_sync checksum stale -bridge-helper-path "$shim" "${extra[@]}"
	else
		bench_sync checksum stale -bridge-helper-path "$shim"
	fi

	if grep -qE 'No changed extents selected, skipping copy|checksum: nothing written, skipping' "$RUN_LOG" 2>/dev/null; then
		warn "SKIP: this sync wrote nothing, so no digest exchange happened and the header was never read. See $RUN_LOG"
		results_row "$CSV" checksum stale-result "" "" "" "" "" "" "SKIP nothing written, header not exercised"
		return 0
	fi

	if [ "$RUN_RC" = 0 ]; then
		warn "FAIL: the sync SUCCEEDED against a helper whose reply had no format header -- the version check is not being applied, so a mismatched helper's digests would be trusted. See $RUN_LOG"
		results_row "$CSV" checksum stale-result 1 "" "" "" "" "" "FAIL headerless reply accepted"
	elif grep -q 'do not match the bytes sent' "$RUN_LOG" 2>/dev/null; then
		warn "FAIL: a stale helper was reported as a DATA MISMATCH. This is the false-corruption case the format header exists to prevent: an operator would conclude the replica is bad and reinit it, when the only problem is an out-of-date binary on $TARGET_HOST. See $RUN_LOG"
		results_row "$CSV" checksum stale-result 1 "" "" "" "" "" "FAIL skew reported as corruption"
	elif grep -q 'digest format mismatch' "$RUN_LOG" 2>/dev/null; then
		log "   PASS: reported as a format/version mismatch rather than as a corrupt replica"
		results_row "$CSV" checksum stale-result 0 "" "" "" "" "" "PASS skew reported as skew"
	else
		warn "FAIL: the run failed, but named neither a format mismatch nor a data mismatch, so it is unclear what an operator would conclude. See $RUN_LOG"
		results_row "$CSV" checksum stale-result 1 "" "" "" "" "" "FAIL failed with an unclear reason"
	fi
}

# 13d: -no-checksum must genuinely skip the check.
#
# Cheap, and it guards the escape hatch. The check is on by default and
# requires a helper, so -no-checksum is what an operator reaches for when it
# is in the way; a flag that quietly does nothing would leave them stuck.
checksum_disabled_subtest() {
	log "--- 13d checksum/off: -no-checksum must skip the check ---"

	# Needs a writing sync like the others: "-no-checksum was honoured" is
	# only meaningful on a run that would otherwise have done the exchange.
	local -a extra
	mapfile -t extra < <(checksum_write_args)
	if [ ${#extra[@]} -gt 0 ]; then
		bench_sync checksum off -no-checksum "${extra[@]}"
	else
		bench_sync checksum off -no-checksum
	fi
	if [ "$DRY_RUN" = yes ]; then
		results_row "$CSV" checksum off-result DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi

	if [ "$RUN_RC" != 0 ]; then
		warn "FAIL: -no-checksum made the sync fail (exit=$RUN_RC) -- see $RUN_LOG"
		results_row "$CSV" checksum off-result 1 "" "" "" "" "" "FAIL sync failed with -no-checksum"
	elif grep -q 'checksum: asking the target to hash' "$RUN_LOG" 2>/dev/null; then
		warn "FAIL: -no-checksum was passed but the target was asked for digests anyway -- the flag is not being honoured. See $RUN_LOG"
		results_row "$CSV" checksum off-result 1 "" "" "" "" "" "FAIL flag ignored"
	elif grep -q 'disabled by -no-checksum' "$RUN_LOG" 2>/dev/null; then
		log "   PASS: the check was skipped and the run said why"
		results_row "$CSV" checksum off-result 0 "" "" "" "" "" "PASS check disabled by flag"
	else
		warn "FAIL: -no-checksum neither ran the check nor said it was disabled, so what happened is unclear. See $RUN_LOG"
		results_row "$CSV" checksum off-result 1 "" "" "" "" "" "FAIL no explanation in the log"
	fi
}

# --- Stage 12: a failed run must not wedge the next one ----------------------

# The regression this exists for, seen in production 2026-09-01: a run copied
# every disk successfully and then FAILED afterwards -- a failed -verify, in
# that case -- and every subsequent run then refused with
#
#   target file ... has an mtime ... newer than the last sync timestamp
#
# ...blaming an out-of-band writer that did not exist. last_sync_timestamp is
# written only by the whole-run success path, so the failed run left the disks
# freshly written and the timestamp stale, and each later refusal happened
# BEFORE the copy that would have moved it on. One failed verify wedged the
# pair permanently.
#
# The fix (replica_written_at, see docs/design/verify.md F2) records when
# vmsync last wrote each replica disk, independently of whether the run went
# on to succeed, stat'd on the TARGET's own clock so the comparison is
# same-clock and exact.
#
# This CANNOT be a unit test. It needs a real failed run that genuinely wrote
# to a real replica, and then a second real run to prove it is not refused --
# the whole bug lives in state persisted between two processes.
#
# -test=failure-define is what makes a run fail AFTER the copy has landed: it
# fails the target's redefine, which is the last step, so the disks are
# written and last_sync_timestamp is never recorded. That is precisely the
# shape of the original incident, reached deterministically instead of by
# corrupting anything.
# --- Stage 14: the recorded verification failure -----------------------------
#
# Stage 2 proves -verify NOTICES a corrupted replica. This stage proves the
# notice OUTLIVES the run that made it, which is a separate property and the
# one that actually protects an estate.
#
# The gap it closes: a successful sync rebuilds the target's definition from the
# SOURCE's XML, so every target-only field it does not explicitly write is lost.
# A mismatch found at 02:00 was therefore erased by the ordinary incremental at
# 03:00, and the promotion at 09:00 saw a replica with a clean record. Nobody
# was lied to exactly once -- the finding was in a log -- but nothing that
# decides anything could see it.
#
# So six assertions, in the order the state moves:
#   14a the finding is written to the target domain
#   14b an ordinary sync is REFUSED while it stands, and does not count as a
#       sync failure
#   14c a plain -reinit is refused too -- the non-obvious half, because a
#       reinit does recopy everything and so looks like a repair, but it never
#       verifies the result, so allowing it would move the replica from "known
#       bad" to "assumed good" while erasing the record that said otherwise
#   14d -verify-failure-reinit repairs it, and only a PASSING verify clears it
#   14e when the repair's OWN verify fails, vmsync stops rather than trying a
#       third time, and leaves the replica faulty for a human
#   14f a PROMOTION is refused while the finding stands -- the end every one of
#       the assertions above exists to serve, since promoting is the moment a
#       replica stops being a copy and starts being what users are talking to
#       -- and the override that gets past it says in its log what it overrode
#
# 14a-14d run off the tamper this stage applies; 14e cannot, because the repair
# recopies the whole replica and so heals any corruption staged from outside --
# that is what makes 14d pass. It brings its own fault instead
# (-test=corrupt-after-commit), which fires after each copy is committed, so both
# rungs of the ladder fail. 14f needs no fault of its own at all: it runs on the
# record 14e leaves standing, which is exactly the state a real estate finds its
# replica in the morning after a verification failed twice overnight.
stage_verify_failure() {
	log "=== Stage 14: a verification failure must outlive the run that found it ==="

	if [ "$DRY_RUN" = yes ]; then
		results_row "$CSV" verify-failure precondition DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi

	stage_needs_target_shutoff "$CSV" verify-failure "stage verify-failure" || return 0

	# -force-clean, not -reinit: this stage may be re-run after a previous
	# attempt left the record in place, and a plain -reinit would then be
	# refused by the very interlock under test -- so the baseline would fail
	# for the reason the stage exists to prove, which reads as a broken
	# harness rather than a working feature.
	bench_sync verify-failure baseline -force-clean
	if [ "$RUN_RC" != 0 ]; then
		warn "baseline full sync failed (see $RUN_LOG) -- aborting stage 14$(bench_sync_hint)"
		return 1
	fi

	local target_path vsize
	target_path="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
	[ -n "$target_path" ] || { warn "could not resolve target disk path for dev='$TAMPER_DISK_DEV' -- check TAMPER_DISK_DEV in $CONF"; return 1; }
	vsize="$(target_virtual_size "$target_path")" || true
	[ -n "$vsize" ] && [ "$vsize" -gt 0 ] 2>/dev/null \
		|| { warn "could not read the virtual size of $target_path on $TARGET_HOST -- needed to keep a tamper inside the disk"; return 1; }

	# Whatever happens below, leave the pair clean: a stage that exits with the
	# replica still corrupted AND recorded faulty would make every later stage
	# measure this stage's damage. heal_target uses -force-clean, which is one
	# of the two things that gets past the record.
	trap 'heal_target verify-failure final-heal "$target_path"' RETURN

	# And an EXIT trap beside it, for the one sub-test that promotes: 14f.
	# RETURN does not fire on die or on a signal, which are exactly the cases
	# that would leave the target promoted -- and a promoted target does not
	# merely fail this stage, it makes the heal above a die() of its own,
	# because that heal is a sync and the role gate refuses syncs into a
	# promoted domain. Same shape as stages 6 and 7, guarded so it acts only
	# when 14f actually promoted and has not already put the role back.
	VF_PROMOTED=no
	VF_CLEANED=no
	verify_failure_promotion_cleanup() {
		[ "$VF_CLEANED" = yes ] && return 0
		VF_CLEANED=yes
		[ "$VF_PROMOTED" = yes ] || return 0
		if clear_target_promotion; then
			log "stage 14: the target is back to role=target"
		else
			warn_target_still_promoted
		fi
	}
	trap 'verify_failure_promotion_cleanup' EXIT

	if ! draw_tamper "$vsize"; then
		warn "SKIP stage 14: the configured tamper band does not fit inside a ${vsize}-byte disk"
		results_row "$CSV" verify-failure precondition "" "" "" "" "" "" "SKIP tamper band does not fit the disk"
		return 0
	fi
	log "   corrupting at offset $TAMPER_OFF length $TAMPER_LEN (seed $TAMPER_SEED)"
	if ! tamper_target "$target_path" yes; then
		results_row "$CSV" verify-failure precondition "" "" "" "" "" "" "SKIP the tamper could not be applied"
		return 0
	fi

	verify_failure_record_subtest || return 0
	verify_failure_refuses_sync_subtest
	verify_failure_refuses_reinit_subtest
	verify_failure_repair_subtest
	# Late, because it is the only sub-test that does not depend on the tamper
	# applied above -- it brings its own fault -- and because it deliberately
	# ends with the replica faulty for the RETURN trap to clean up.
	verify_failure_gives_up_subtest
	# Last, and running on what 14e leaves behind rather than on anything of
	# its own. 14d CLEARS the record the moment a verify passes, so this is
	# the only point after it where a fresh verify_state=failed is standing
	# to be promoted against. Running last also means the one thing it
	# changes -- the domain's role, briefly -- is put back with nothing but
	# the RETURN trap's heal behind it, so a restore that fails cannot turn
	# into another sub-test failing for a reason that is not its own.
	verify_failure_refuses_promote_subtest
	# Stood down here, not left armed: the sub-test puts the role back itself,
	# and an EXIT trap still armed at this point would fire at the end of the
	# whole run, long after the heal below has rebuilt this replica.
	trap - EXIT
	return 0
}

# 14a: a -verify that ran and found a difference must record it on the target.
#
# The load-bearing sub-test. If nothing is written here, 14b/14c would "pass"
# by refusing nothing at all, and this stage would certify an interlock that
# does not exist. Returns non-zero to stop the stage, since every assertion
# below is about a record this one proves is there.
verify_failure_record_subtest() {
	log "--- 14a verify-failure/record: the mismatch must be written to the target domain ---"

	bench_sync verify-failure mismatch -verify=fast
	local outcome state failed_at
	outcome="$(verify_outcome "$RUN_PROM")"
	if [ "$outcome" != RAN_MISMATCH ]; then
		warn "FAIL: -verify=fast did not report a mismatch after the target was corrupted at offset $TAMPER_OFF length $TAMPER_LEN (outcome=$outcome, exit=$RUN_RC, reproduce with TAMPER_SEED=$TAMPER_SEED). Nothing below can be tested without a finding to record. See $RUN_LOG"
		results_row "$CSV" verify-failure mismatch-result 1 "" "" "" "" "" "FAIL no mismatch to record"
		return 1
	fi

	state="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" verify_state)"
	failed_at="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" verify_failed_at)"
	if [ "$state" != failed ]; then
		warn "FAIL: -verify found a mismatch but the target carries verify_state='$state' (wanted 'failed'). The finding died with the process: tonight's incremental will overwrite it and a promotion will see a clean replica. See $RUN_LOG"
		results_row "$CSV" verify-failure mismatch-result 1 "" "" "" "" "" "FAIL verify_state not recorded"
		return 1
	fi
	# Present and numeric. The date is what turns the record into a decision --
	# "failed 20 minutes ago" and "failed in March" are not the same call --
	# and a non-numeric value would render as garbage in the refusal message.
	if [ -z "$failed_at" ] || ! [ "$failed_at" -gt 0 ] 2>/dev/null; then
		warn "FAIL: verify_state was recorded but verify_failed_at='$failed_at' is not a unix timestamp, so neither the refusal nor a promotion assessment can say WHEN the replica went bad. See $RUN_LOG"
		results_row "$CSV" verify-failure mismatch-result 1 "" "" "" "" "" "FAIL verify_failed_at missing or not a timestamp"
		return 1
	fi

	log "   PASS: the finding was recorded (verify_state=failed, verify_failed_at=$failed_at)"
	results_row "$CSV" verify-failure mismatch-result 0 "" "" "" "" "" "PASS finding recorded on the target"
	return 0
}

# 14b: an ordinary sync must be refused while the record stands, and must not
# be counted as a sync failure.
#
# The refusal is what gives the record a lifetime -- without it the next sync
# rebuilds the definition and the finding is gone. The failure_count exemption
# matters for a reason that is easy to miss: a non-zero count is ITSELF reported
# by a promotion assessment, so counting this would stack a "may be stale"
# verdict on top of the "may be wrong" one that is the real finding, and an
# operator would see two problems where there is one.
verify_failure_refuses_sync_subtest() {
	log "--- 14b verify-failure/refuses-sync: an ordinary sync must be refused ---"

	# -reinit-after-failures so the counter is live at all: vmsync only records
	# a failure when it is set.
	bench_sync verify-failure refuse-sync -verify=fast -reinit-after-failures=5
	local state failures=0 details=""
	if [ "$RUN_RC" = 0 ]; then
		failures=$((failures + 1))
		details="the sync SUCCEEDED against a replica recorded as having failed verification, so the finding has been erased"
	elif ! grep -q 'does not permit syncing into this domain' "$RUN_LOG" 2>/dev/null; then
		failures=$((failures + 1))
		details="the sync failed (exit=$RUN_RC) but not with the recorded-verification-failure refusal, so it may have been stopped by something unrelated"
	fi
	# The refusal must not consume the record it is protecting.
	state="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" verify_state)"
	if [ "$state" != failed ]; then
		failures=$((failures + 1))
		details="${details}${details:+; }the refusal left verify_state='$state', so it cleared the very record it exists to enforce"
	fi
	if grep -q 'recorded sync failure in target metadata' "$RUN_LOG" 2>/dev/null; then
		failures=$((failures + 1))
		details="${details}${details:+; }the refusal was counted toward -reinit-after-failures, which can only climb (the reinit it would force is refused by this same gate) and blocks promotion on a replica whose real problem is already recorded"
	fi

	if [ "$failures" = 0 ]; then
		log "   PASS: the sync was refused, the record survived, and nothing was counted against the domain"
		results_row "$CSV" verify-failure refuse-sync-result 0 "" "" "" "" "" "PASS ordinary sync refused"
	else
		warn "FAIL: $details. See $RUN_LOG"
		results_row "$CSV" verify-failure refuse-sync-result 1 "" "" "" "" "" "FAIL $failures check(s) failed"
	fi
}

# 14c: a plain -reinit must be refused too.
#
# The half that looks wrong until it is spelled out. A reinit DOES recopy every
# byte, so it plausibly repairs the replica -- but it never verifies the result,
# so permitting it would take the domain from "known bad" to "assumed good,
# unverified" while deleting the record that said otherwise. That is precisely
# the state a promotion assessment exists to distrust, and it is why the two
# ways past are the one that re-proves the replica and the one that says out
# loud it is discarding a finding.
verify_failure_refuses_reinit_subtest() {
	log "--- 14c verify-failure/refuses-reinit: a plain -reinit must be refused as well ---"

	bench_sync verify-failure refuse-reinit -reinit -verify=fast
	local state failures=0 details=""
	if [ "$RUN_RC" = 0 ]; then
		failures=$((failures + 1))
		details="a plain -reinit SUCCEEDED, so the replica is now unverified with no record that it ever failed"
	elif ! grep -q 'does not permit syncing into this domain' "$RUN_LOG" 2>/dev/null; then
		failures=$((failures + 1))
		details="the -reinit failed (exit=$RUN_RC) but not with the recorded-verification-failure refusal"
	fi
	state="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" verify_state)"
	if [ "$state" != failed ]; then
		failures=$((failures + 1))
		details="${details}${details:+; }the refused -reinit left verify_state='$state'"
	fi

	if [ "$failures" = 0 ]; then
		log "   PASS: the plain -reinit was refused and the record survived"
		results_row "$CSV" verify-failure refuse-reinit-result 0 "" "" "" "" "" "PASS plain -reinit refused"
	else
		warn "FAIL: $details. See $RUN_LOG"
		results_row "$CSV" verify-failure refuse-reinit-result 1 "" "" "" "" "" "FAIL $failures check(s) failed"
	fi
}

# 14d: -verify-failure-reinit must repair the replica and clear the record.
#
# This one exercises the whole ladder rather than just the override, and it does
# so without any help from the harness. The record is present, so the run is
# allowed past it -- but the run itself is an ordinary INCREMENTAL, and an
# incremental copies only what the source's dirty bitmap says changed. The
# source never wrote to the tampered offset, so the corruption is still there
# and this run's own verify fails. That is what fires the ladder: a full recopy
# (which does overwrite the tampered bytes) and a second verify, which passes.
#
# So a pass here means: the override worked, the first verify failed, the recopy
# ran, the second verify passed, and only then was the record cleared.
verify_failure_repair_subtest() {
	log "--- 14d verify-failure/repair: -verify-failure-reinit must recopy, re-verify, and only then clear ---"

	bench_sync verify-failure repair -verify=fast -verify-failure-reinit
	local state failures=0 details=""
	if [ "$RUN_RC" != 0 ]; then
		failures=$((failures + 1))
		details="the repair run failed (exit=$RUN_RC) although a full recopy overwrites the tampered bytes and should verify clean"
	fi
	if ! grep -q 'attempting ONE full recopy and a second verification' "$RUN_LOG" 2>/dev/null; then
		failures=$((failures + 1))
		details="${details}${details:+; }the ladder never fired -- the run got past the record but nothing recopied, so a pass here would mean the flag is a plain override rather than a repair"
	fi
	state="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" verify_state)"
	if [ -n "$state" ]; then
		failures=$((failures + 1))
		details="${details}${details:+; }the repair verified clean but left verify_state='$state', so this replica stays unsyncable forever and only -force-clean can free it"
	fi

	if [ "$failures" = 0 ]; then
		log "   PASS: the ladder recopied, re-verified, and cleared the record"
		results_row "$CSV" verify-failure repair-result 0 "" "" "" "" "" "PASS repaired and record cleared"
	else
		warn "FAIL: $details. See $RUN_LOG"
		results_row "$CSV" verify-failure repair-result 1 "" "" "" "" "" "FAIL $failures check(s) failed"
	fi
}

# 14e: after the repair's OWN verify fails, vmsync must stop and leave the
# replica faulty -- not try a third time.
#
# The headline behaviour of the whole feature, and the one sub-test that cannot
# be staged with a tamper. Corruption applied from outside is overwritten by
# the repair's full recopy, which is precisely the recopy's job -- so the second
# verify passes and 14d is what you get instead. Reaching this branch needs a
# fault that fires again on the recopy, from inside the process, which is what
# -test=corrupt-after-commit is: it writes over the replica AFTER the copy has been
# committed and confirmed, on every run, incremental or full.
#
# So this run does the whole ladder with both rungs failing: copy, corrupt,
# verify fails, record; full recopy, corrupt again, verify fails again, give up.
verify_failure_gives_up_subtest() {
	log "--- 14e verify-failure/gives-up: a repair that also fails verification must stop, not retry ---"

	bench_sync verify-failure gives-up -test=corrupt-after-commit -verify=fast -verify-failure-reinit
	local state failures=0 details=""

	if [ "$RUN_RC" = 0 ]; then
		failures=$((failures + 1))
		details="the run SUCCEEDED although every verify under -test=corrupt-after-commit finds a genuine difference -- either the fault did not fire or the mismatch was not treated as one"
	fi
	# The distinctive marker of the give-up branch. Without it the run may have
	# failed in the "repair could not be completed" branch instead, which is a
	# different outcome: that one blames the mechanism, this one concludes the
	# data is wrong twice over.
	if ! grep -q 'FAILED verification AGAIN after a full recopy' "$RUN_LOG" 2>/dev/null; then
		failures=$((failures + 1))
		details="${details}${details:+; }the run did not reach the give-up branch -- it failed for some other reason, so the 'do not try a third time' decision is untested"
	fi
	if ! grep -q 'will not try a third time' "$RUN_LOG" 2>/dev/null; then
		failures=$((failures + 1))
		details="${details}${details:+; }nothing said the ladder had stopped, so an operator reading this log has no way to know a third attempt is not coming"
	fi
	# And the state a human has to find afterwards.
	state="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" verify_state)"
	if [ "$state" != failed ]; then
		failures=$((failures + 1))
		details="${details}${details:+; }the replica failed verification twice but carries verify_state='$state' -- nothing will refuse the next sync into it, and a promotion will see it as clean"
	fi

	if [ "$failures" = 0 ]; then
		log "   PASS: the repair's verify failed too, vmsync stopped after one attempt, and the replica is recorded faulty"
		results_row "$CSV" verify-failure gives-up-result 0 "" "" "" "" "" "PASS gave up after one repair, replica left faulty"
	else
		warn "FAIL: $details. See $RUN_LOG"
		results_row "$CSV" verify-failure gives-up-result 1 "" "" "" "" "" "FAIL $failures check(s) failed"
	fi
}

# 14f: while the record stands, a PROMOTION must be refused as well.
#
# The assertion the other five exist to make possible. 14b and 14c prove the
# SYNC gate acts on the record; this is the only one that proves the PROMOTION
# gate does -- a separate check, in a separate package, reached by a code path
# no other sub-test crosses. That separation is exactly why the promotion gate
# could sit there passing its own unit tests, against inputs built by hand,
# while production promoted the bad replica anyway. A promotion is where a replica stops being a
# copy and becomes what users are talking to, so a replica a comparison has
# already found different from its source is precisely the one that must not
# become production without somebody saying, in a flag and in the log, that
# they are booting a copy known to be wrong.
#
# Worth asserting rather than assuming, because the check it exercises is split
# across two packages: pkg/failover decides from a struct, and the promote
# command has to fill that struct in from the domain. A promote that never
# reads verify_state off the target leaves the refusal as dead code that every
# unit test still passes -- those tests set the field by hand -- while
# production promotes the bad replica without a word. Nothing else in this
# harness would catch that: stage 6 promotes a replica it synced moments
# earlier, which carries no finding for a promotion to ignore.
#
# Placement is load-bearing and is why this runs last. 14d CLEARS the record as
# soon as a verify passes, which is the whole point of that sub-test, so a
# promotion attempted after it would meet a clean replica and "pass" by
# refusing nothing -- the same vacuous pass the stage header warns about for
# 14b and 14c. 14e then leaves a FRESH verify_state=failed behind on purpose,
# because it is the sub-test that ends with the replica faulty for a human to
# find, and that human's next move is the one being tested here.
#
# The precondition is therefore READ rather than assumed. 14e can fail or be
# skipped -- it needs a vmsync built with -test=corrupt-after-commit -- and a
# promotion refused against a replica carrying no finding at all would prove
# nothing while reporting a pass, which is worse than reporting nothing.
verify_failure_refuses_promote_subtest() {
	log "--- 14f verify-failure/refuses-promote: a promotion must be refused while the record stands ---"

	# -promote acts on the host it runs on and refuses a remote libvirt URI by
	# design -- that is what keeps a failover working when the other site is
	# unreachable -- so it needs vmsync ON the target host, exactly as stages
	# 6, 7 and 11 do. Saying so beats letting the attempt fail: a promote that
	# could not be run at all exits non-zero just like a promote that was
	# refused, and that is the confusion this whole sub-test is written to
	# avoid.
	if [ -z "${TARGET_VMSYNC_BIN:-}" ]; then
		warn "SKIP 14f: TARGET_VMSYNC_BIN is not set in $CONF, and -promote must run ON $TARGET_HOST because it refuses a remote libvirt URI by design -- so there is no way from here to attempt the promotion this sub-test is about."
		results_row "$CSV" verify-failure refuse-promote-result "" "" "" "" "" "" "SKIP TARGET_VMSYNC_BIN unset"
		return 0
	fi

	local state
	state="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" verify_state)"
	if [ "$state" != failed ]; then
		warn "SKIP 14f: $TARGET_DOMAIN carries verify_state='$state' rather than 'failed', so there is no recorded verification failure for a promotion to be refused over. 14e is what leaves one standing -- read its row above before treating this as a vmsync problem."
		results_row "$CSV" verify-failure refuse-promote-result "" "" "" "" "" "" "SKIP no verify_state record to promote against"
		return 0
	fi

	# The target host addressed by its own LOCAL uri, never TARGET_URI, for
	# the reason above.
	local local_uri="qemu:///system"
	local failures=0 details="" role role_before refuse_log refuse_rc force_log force_rc

	# Captured BEFORE the attempt so the refusal can be checked positively --
	# "the role did not change" rather than "the role is not promoted".
	# vmsync_meta_field swallows every error it meets, so a virsh that will
	# not answer, a missing xmllint or an SSH hiccup all read as an empty
	# string; an assertion phrased as [ "$role" != promoted ] is satisfied by
	# every one of those without the domain having been looked at, which is
	# the wrong-reason pass this sub-test exists to rule out. The verify_state
	# read above already proved the same path works on this pair a moment ago.
	role_before="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"

	# Both outcomes of this run are read more than once below, so they are
	# copied out of RUN_LOG/RUN_RC immediately: those two are globals that the
	# next vmsync_on_host overwrites, and an assertion reading a later run's
	# exit code while quoting this one's is exactly the kind of wrong-reason
	# pass this sub-test exists to rule out.
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" verify-failure refuse-promote \
		-promote -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN" \
		-promote-mode forced -promoted-by bench-harness
	refuse_log="$RUN_LOG"
	refuse_rc="$RUN_RC"
	if [ "$refuse_rc" = 0 ]; then
		failures=$((failures + 1))
		details="-promote SUCCEEDED against a replica recorded as having failed verification, so a copy already proven not to match its source becomes production with nothing said and nobody asked to override anything"
	elif ! grep -q "verification found this replica's contents differing from its source" "$refuse_log" 2>/dev/null; then
		# Not satisfied by the exit code alone, and deliberately so. A promote
		# that never ran -- wrong path, unreadable domain, a libvirt that would
		# not answer -- exits non-zero too, and so does one refused over some
		# unrelated piece of missing evidence. Accepting any of those as proof
		# would record this interlock as working on a run where it was never
		# consulted, which is the exact failure mode that let the gap this
		# sub-test covers survive a green harness.
		failures=$((failures + 1))
		details="-promote failed (exit=$refuse_rc) but its log never names the recorded verification failure, so something else stopped it -- a binary that is not there, a domain that could not be read, or an unrelated evidence problem all end this way, and none of them proves the finding was so much as read"
	fi

	# Refused has to mean nothing was written, not merely that something was
	# reported. A domain left promoted by a run that called itself refused is
	# worse than either outcome alone: the replica is serving and the only
	# record of how it got there says it was not allowed to.
	role="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
	if [ "$role" != "$role_before" ]; then
		failures=$((failures + 1))
		details="${details}${details:+; }the promotion reported itself refused but $TARGET_DOMAIN's replication_role moved from '$role_before' to '$role', so the refusal is cosmetic -- and at 'promoted' every sync into this domain is now refused as well"
	fi

	# And the way past, which must exist and must be loud. An interlock with
	# no supported override is one an operator routes around by hand during
	# an outage, and an override that says nothing leaves no durable trace
	# that a copy known to be wrong was chosen deliberately -- which is the
	# only thing that distinguishes a considered decision from a mistake when
	# somebody reads this domain's metadata a week later.
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" verify-failure force-promote \
		-promote -force-promote -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN" \
		-promote-mode forced -promoted-by bench-harness
	force_log="$RUN_LOG"
	force_rc="$RUN_RC"
	# Armed the moment the promotion may have taken, so the stage's EXIT trap
	# can put the role back if anything between here and the restore below
	# ends the run instead -- a die, a Ctrl+C, a failed assertion that exits.
	[ "$force_rc" = 0 ] && VF_PROMOTED=yes
	if [ "$force_rc" != 0 ]; then
		failures=$((failures + 1))
		details="${details}${details:+; }-force-promote failed too (exit=$force_rc), so the documented way past this refusal does not work and an operator facing a real outage has no supported way to boot the only copy they have left"
	elif ! grep -q 'promoted despite' "$force_log" 2>/dev/null; then
		failures=$((failures + 1))
		details="${details}${details:+; }-force-promote succeeded without recording WHAT it was overriding, so the override is indistinguishable in the log from an ordinary promotion of a healthy replica"
	elif ! grep -q "verification found this replica's contents differing from its source" "$force_log" 2>/dev/null; then
		failures=$((failures + 1))
		details="${details}${details:+; }-force-promote said it promoted despite something, but never that the something was the recorded verification failure -- so the finding was still not read, and the override is loud about the wrong thing"
	fi

	role="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
	if [ "$force_rc" = 0 ] && [ "$role" != promoted ]; then
		failures=$((failures + 1))
		details="${details}${details:+; }-force-promote exited 0 but left replication_role='$role', so it reported a promotion it did not actually perform"
	fi

	# Whatever any of the above found, put the role back -- unconditionally,
	# and not behind any of the branches, because the assertions are the part
	# most likely to be wrong and the restore is the part that must not be.
	# The stage's RETURN trap heals this replica with a -force-clean sync, and
	# a sync into a promoted domain is refused by the role gate -- so a target
	# left promoted here does not merely fail this sub-test, it turns the heal
	# into a die() that ends the whole run with a knowingly corrupted replica
	# still in place. -update-role=target is the same documented way back
	# stage 6 uses, and it takes the promotion record with it.
	retarget_promoted_target verify-failure restore-role "$local_uri"
	role="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
	if [ "$role" = target ]; then
		# Verified back, so the stage's EXIT trap has nothing left to undo.
		VF_PROMOTED=no
	else
		failures=$((failures + 1))
		details="${details}${details:+; }the way back did not take and $TARGET_DOMAIN is left with replication_role='$role' rather than 'target'"
		# Only a role that still REFUSES syncs is an emergency, and only that
		# one deserves the full remedy printed: the heal on the way out of this
		# stage is the very next thing that would run into it.
		case "$role" in
		promoted | source | paused) warn_target_still_promoted "$role" ;;
		esac
	fi

	if [ "$failures" = 0 ]; then
		log "   PASS: the promotion was refused by name, the domain stayed a target, and the override promoted it while saying what it overrode"
		results_row "$CSV" verify-failure refuse-promote-result 0 "" "" "" "" "" "PASS promotion refused while the finding stands"
	else
		warn "FAIL: $details. See $refuse_log and $force_log"
		results_row "$CSV" verify-failure refuse-promote-result 1 "" "" "" "" "" "FAIL $failures check(s) failed"
	fi
}

stage_wedge() {
	log "=== Stage 12: a failed run must not wedge the next one ==="

	if [ "$DRY_RUN" = yes ]; then
		bench_sync wedge baseline -reinit
		bench_sync wedge failing-run "-test=failure-define"
		bench_sync wedge recovery
		results_row "$CSV" wedge result DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi

	stage_needs_target_shutoff "$CSV" wedge "stage wedge" || return 0

	# The failing run has to WRITE, or there is no advanced mtime and nothing
	# to wedge: an incremental with a clean dirty bitmap returns before it
	# touches the base. So the guest must dirty its own disk first, which
	# needs a running source with a usable agent -- the same requirement, and
	# the same skip, as stage verify-long.
	local src_state
	src_state="$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN")" || src_state="unknown"
	if [ "$src_state" != running ]; then
		warn "SKIP stage wedge: source domain '$SOURCE_DOMAIN' is '$src_state', but the failing run has to copy something for there to be a wedge to test"
		results_row "$CSV" wedge precondition "" "" "" "" "" "" "SKIP source domain not running"
		return 0
	fi
	if ! guest_exec_available; then
		warn "SKIP stage wedge: $GUEST_EXEC_WHY -- without guest writes the failing run copies nothing, leaves the mtime untouched, and this stage would report a green result having tested nothing"
		results_row "$CSV" wedge precondition "" "" "" "" "" "" "SKIP guest-exec unavailable"
		return 0
	fi

	# Baseline, so the pair is clean and the target carries a
	# last_sync_timestamp for the failing run to leave behind.
	bench_sync wedge baseline -reinit
	if [ "$RUN_RC" != 0 ]; then
		warn "baseline full sync for the wedge test failed (see $RUN_LOG) -- aborting stage 12$(bench_sync_hint)"
		return 1
	fi

	local sync_before
	sync_before="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_sync_timestamp)"
	if [ -z "$sync_before" ]; then
		warn "the target carries no last_sync_timestamp after a successful baseline -- something is wrong upstream of this stage"
		results_row "$CSV" wedge precondition 1 "" "" "" "" "" "FAIL no last_sync_timestamp after baseline"
		return 1
	fi
	log "--- last_sync_timestamp after baseline: $sync_before ---"

	guest_dirty || {
		warn "could not dirty the source guest's disk -- the failing run below would copy nothing, so this stage would prove nothing"
		results_row "$CSV" wedge precondition "" "" "" "" "" "" "SKIP guest_dirty failed"
		return 0
	}

	# The failing run: copies, commits, then fails on the redefine.
	log "--- running a sync that copies successfully and then fails (expect: non-zero exit) ---"
	bench_sync wedge failing-run "-test=failure-define"
	if [ "$RUN_RC" = 0 ]; then
		warn "FAIL: -test=failure-define exited 0 -- the fault did not fire, so nothing below is testing a failed run"
		results_row "$CSV" wedge failing-run 1 "" "" "" "" "" "FAIL fault injection did not fail the run"
		return 1
	fi

	# What the failed run must and must not have left behind.
	local sync_after written_at w_ok=1 s_ok=1
	sync_after="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_sync_timestamp)"
	written_at="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replica_written_at)"

	[ -n "$written_at" ] && w_ok=0
	if [ "$w_ok" = 0 ]; then
		log "   PASS: the failed run recorded replica_written_at='$written_at'"
		results_row "$CSV" wedge written-at 0 "" "" "" "" "" "PASS replica_written_at recorded by a failed run"
	else
		warn "FAIL: the failed run recorded no replica_written_at, so the next run has nothing but the stale last_sync_timestamp to compare against -- the wedge is still there. See $RUN_LOG"
		results_row "$CSV" wedge written-at 1 "" "" "" "" "" "FAIL no replica_written_at after a failed run"
	fi

	# It must NOT have advanced: last_sync_timestamp means "a run completed",
	# and this one did not. Advancing it would turn a replication stall into a
	# replica that failover believes is good.
	[ "$sync_after" = "$sync_before" ] && s_ok=0
	if [ "$s_ok" = 0 ]; then
		log "   PASS: last_sync_timestamp still '$sync_after' -- unchanged by the failed run"
		results_row "$CSV" wedge last-sync-held 0 "" "" "" "" "" "PASS last_sync_timestamp not advanced by a failed run"
	else
		warn "FAIL: last_sync_timestamp moved from '$sync_before' to '$sync_after' on a run that FAILED -- failover reads that field as evidence a sync landed, so this replica now looks promotable when it is not"
		results_row "$CSV" wedge last-sync-held 1 "" "" "" "" "" "FAIL last_sync_timestamp advanced by a failed run"
	fi

	# The actual regression: the NEXT run must not be refused.
	log "--- running again (expect: not refused by the out-of-band-write check) ---"
	bench_sync wedge recovery
	if grep -q "newer than the last sync timestamp" "$RUN_LOG" 2>/dev/null; then
		warn "FAIL: the run after a failed one was REFUSED by the out-of-band-write check -- this is the wedge, unfixed. See $RUN_LOG"
		results_row "$CSV" wedge recovery 1 "" "" "" "" "" "FAIL next run refused after a failed run"
		return 1
	fi
	if [ "$RUN_RC" != 0 ]; then
		# Refusal is what this stage is about; any OTHER failure is still a
		# failure, but naming it separately keeps the report honest about
		# which thing broke.
		warn "the run after a failed one did not succeed, though it was NOT refused by the mtime check (see $RUN_LOG)$(bench_sync_hint)"
		results_row "$CSV" wedge recovery 1 "" "" "" "" "" "FAIL next run failed for some other reason"
		return 1
	fi
	log "   PASS: the run after a failed one was not refused"
	results_row "$CSV" wedge recovery 0 "" "" "" "" "" "PASS next run not refused"

	if [ "$w_ok" != 0 ] || [ "$s_ok" != 0 ]; then
		return 1
	fi
	return 0
}
# --- Stage 11: -invert (reversing a pair's direction) ------------------------
#
# Nothing benched inversion before this, which is why it earns a stage: it is
# the one operation that rewrites BOTH ends' metadata, and every one of its
# failure modes leaves a pair that looks configured and cannot sync.
#
# The check the stage was written for is the disk-path one. -target-disk-path
# names where THIS direction's replicas go, so after an inversion the same
# value points at where the new SOURCE keeps its disks, not the new target. Reuse
# it on the reversed sync and the copy lands somewhere the new target does not
# keep its disks; the domain is then redefined to match and its original disks
# are orphaned -- still on disk, still costing space, referenced by nothing.
# vmsync's answer is to warn and name the value to use, so that warning is
# asserted here: that it fires when the two ends differ, and that it names the
# right directory.
#
# What this stage deliberately does NOT do is run the reversed sync. That would
# write into the SOURCE domain's disks, which no other stage does and which the
# harness's own warnings about Stage 4 exist for -- and it would prove only what
# the assertions below already establish. Everything here is metadata and log
# text; the source's disks are never written.
stage_invert() {
	log "=== Stage 11: -invert direction reversal ==="
	local sc=invert
	local fo_ok=0

	if [ -z "${TARGET_VMSYNC_BIN:-}" ]; then
		warn "TARGET_VMSYNC_BIN is not set in $CONF -- skipping stage 11. An inversion needs the target promoted first, and -promote must run ON the target host (it refuses a remote libvirt URI by design)."
		results_row "$CSV" "$sc" skipped 0 "" "" "" "" "" "SKIPPED TARGET_VMSYNC_BIN unset"
		return 0
	fi

	if [ "$DRY_RUN" = yes ]; then
		bench_sync "$sc" baseline -reinit
		fo_check "$sc" "invert flips both ends' roles" 0
		fo_check "$sc" "the old direction is refused afterwards" 0
		fo_check "$sc" "invert is idempotent" 0
		return 0
	fi

	stage_needs_target_shutoff "$CSV" "$sc" "stage invert" || return 0
	reset_pair_state "$sc" yes
	require_target_syncable "$sc" || return 0

	# The backstop. An inversion leaves the SOURCE marked as a replication
	# target, and a source in that state refuses to be synced FROM by every
	# later stage and every scheduled run in the estate -- a worse thing to
	# leave behind than a promoted target, because nothing else in this
	# harness resets the source. An EXIT trap rather than RETURN for the
	# reason Stage 6 gives: RETURN does not fire on a signal.
	INVERT_DONE=no
	INVERT_CLEANED=no
	INVERT_STOPPED_SOURCE=no
	invert_cleanup() {
		[ "$INVERT_CLEANED" = yes ] && return 0
		INVERT_CLEANED=yes

		# The source restart comes FIRST and is unconditional on
		# INVERT_DONE: this stage shuts the source down to satisfy
		# -invert's own precondition, and leaving a production-shaped
		# source powered off is a worse thing to walk away from than any
		# metadata this stage could scramble. It also has to happen even
		# when the stage aborted before inverting anything.
		if [ "$INVERT_STOPPED_SOURCE" = yes ]; then
			local now_state
			now_state="$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN" 2>/dev/null || true)"
			if [ "$now_state" != running ]; then
				log "   starting the source domain again (this stage shut it down)"
				virsh_uri "$SOURCE_URI" start "$SOURCE_DOMAIN" >/dev/null 2>&1 \
					|| warn "could not start '$SOURCE_DOMAIN' again -- start it by hand; every later stage that dirties the guest needs it running"
			fi
		fi

		[ "$INVERT_DONE" = yes ] || return 0
		log "stage 11: putting the pair back the way round it started"
		# virsh, not vmsync -update-role: this has to work when the code
		# under test is what is broken. reset_pair_state explains at length.
		reset_pair_state "$sc" yes
		bench_sync "$sc" heal -reinit
		if [ "$RUN_RC" != 0 ]; then
			warn "stage 11: the healing -reinit did not succeed (see $RUN_LOG). $SOURCE_DOMAIN and $TARGET_DOMAIN have had their vmsync metadata cleared, so nothing refuses a sync -- but the pair has no checkpoint chain until one completes."
		fi
	}
	trap invert_cleanup EXIT

	# --- baseline ------------------------------------------------------------
	bench_sync "$sc" baseline -reinit
	if [ "$RUN_RC" != 0 ]; then
		warn "baseline full sync failed (see $RUN_LOG) -- aborting stage 11 before anything is inverted$(bench_sync_hint)"
		invert_cleanup
		trap - EXIT
		return 1
	fi

	# Captured BEFORE the inversion: afterwards the roles are swapped and
	# "which end keeps its disks where" reads backwards.
	local new_target_dir new_source_dir cpts_before
	new_target_dir="$(domain_disk_dir "$SOURCE_URI" "$SOURCE_DOMAIN")"
	new_source_dir="$(domain_disk_dir "$TARGET_URI" "$TARGET_DOMAIN")"
	cpts_before="$(vmsync_checkpoint_count "$SOURCE_URI" "$SOURCE_DOMAIN")"

	# The two ends as VMSYNC spells them, read from what the baseline sync
	# just wrote, never rebuilt from SOURCE_HOST/TARGET_HOST.
	#
	# Stage 6 makes the same choice and gives the reason: a reference this
	# harness reconstructs would be asserting that two strings this harness
	# built match each other. These are the real ones, and comparing them
	# also catches the regression that once wrote replica_source as
	# "127.0.0.1:<vm>" -- a name for every machine and therefore for none.
	local src_ref tgt_ref
	src_ref="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replica_source)"
	tgt_ref="$(vmsync_meta_field "$SOURCE_URI" "$SOURCE_DOMAIN" replica_targets)"
	case "$tgt_ref" in
	*,*)
		# A source that fans out to several targets. The entry for THIS pair
		# cannot be picked apart from the others without reconstructing a
		# hostname, which is the thing being avoided.
		warn "$SOURCE_DOMAIN replicates to more than one target ($tgt_ref); the peer-reference assertions need a single-target pair"
		tgt_ref=""
		;;
	esac
	log "disk directories: new source (currently target) = ${new_source_dir:-<scattered>}, new target (currently source) = ${new_target_dir:-<scattered>}"

	if [ "${cpts_before:-0}" -gt 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the baseline left a checkpoint chain on the source" "$fo_ok" \
		"$SOURCE_DOMAIN has $cpts_before vmsync checkpoints, so there is no chain for the inversion to discard and that assertion below would pass vacuously"

	# --- promote, so there is something to invert toward ---------------------
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" promote \
		-promote -target-uri "qemu:///system" -target-domain "$TARGET_DOMAIN" \
		-promote-mode planned -promoted-by bench-harness
	if [ "$RUN_RC" != 0 ]; then
		warn "promote failed (see $RUN_LOG) -- aborting stage 11; an inversion needs a promoted peer"
		# The promotion may have landed even though the command failed, so
		# the reset has to run -- and run NOW rather than at shell exit,
		# which is where an armed EXIT trap would fire. A target left
		# promoted refuses every sync in the estate, so every later stage
		# would fail for a reason that has nothing to do with them.
		INVERT_DONE=yes
		invert_cleanup
		trap - EXIT
		return 1
	fi
	INVERT_DONE=yes

	# --- the guard: inversion is refused while the old source runs -----------
	# An inversion makes the old source a replication target, and a running
	# target is one scheduled sync away from being overwritten under a live
	# workload. vmsync refuses rather than stopping a production VM as a side
	# effect of a metadata command -- which is exactly the kind of helpfulness
	# nobody wants from a tool that can also delete disks.
	#
	# Free to assert, because the source IS running at this point: every other
	# stage needs it up.
	local src_state
	src_state="$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN" 2>/dev/null || true)"
	if [ "$src_state" = running ]; then
		vmsync_verb_pair "$sc" invert-while-running -invert
		if [ "$RUN_RC" != 0 ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "invert is refused while the old source is still running" "$fo_ok" \
			"exit $RUN_RC -- inverting would make $SOURCE_DOMAIN a replication target while it is serving, and the next sync would overwrite it under a live workload. See $RUN_LOG"

		if grep -qi "shut it down" "$RUN_LOG" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "and says what to do about it" "$fo_ok" \
			"the refusal in $RUN_LOG does not tell the operator to shut the domain down first"
	else
		warn "SKIP the running-source refusal check: $SOURCE_DOMAIN is '$src_state', not running"
		results_row "$CSV" "$sc" invert_refuses_running_source "" "" "" "" "" "" "SKIP source not running"
	fi

	# --- stop the source, which is that precondition -------------------------
	if [ "$src_state" = running ]; then
		log "   shutting the source down: -invert requires it, because the inversion makes it a replication target"
		INVERT_STOPPED_SOURCE=yes
		local now_state="$src_state"
		if graceful_shutdown "$SOURCE_URI" "$SOURCE_DOMAIN" \
			"${SOURCE_SHUTDOWN_WAIT_SECONDS:-120}" "the source"; then
			now_state=shutoff
		fi
		local waited="$SHUTDOWN_WAITED"
		if [ "$now_state" != shutoff ]; then
			# Deliberately NOT destroyed. Stage 6 will hard-stop a disposable
			# replica, but this is the SOURCE -- the one domain in this pair
			# standing in for production -- and pulling its power to run a
			# test is a worse trade than not running the test. The cleanup
			# still starts it if it did stop later.
			warn "SKIP the rest of stage 11: $SOURCE_DOMAIN did not shut down within ${waited}s and this stage will not destroy the source to proceed. Raise SOURCE_SHUTDOWN_WAIT_SECONDS if this guest is legitimately slow to stop."
			results_row "$CSV" "$sc" invert_precondition "" "" "" "" "" "" "SKIP source would not shut down"
			invert_cleanup
			trap - EXIT
			return 0
		fi
	fi

	# --- invert ---------------------------------------------------------------
	# Run from here rather than on either host: -invert genuinely spans both
	# ends, and this is the one failover verb that takes a remote URI.
	vmsync_verb_pair "$sc" invert -invert
	if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "invert succeeds against a promoted peer" "$fo_ok" "exit $RUN_RC -- $(log_reason "$RUN_LOG")"

	# --- both ends' metadata flipped -----------------------------------------
	local got
	got="$(vmsync_meta_field "$SOURCE_URI" "$SOURCE_DOMAIN" replication_role)"
	if [ "$got" = target ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the old source is now a replication target" "$fo_ok" \
		"$SOURCE_DOMAIN's replication_role is '$got', not 'target'"

	got="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
	if [ "$got" = source ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the promoted replica is now the source" "$fo_ok" \
		"$TARGET_DOMAIN's replication_role is '$got', not 'source'"

	if [ -n "$tgt_ref" ]; then
		got="$(vmsync_meta_field "$SOURCE_URI" "$SOURCE_DOMAIN" replica_source)"
		if [ "$got" = "$tgt_ref" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the new target records who it now replicates from" "$fo_ok" \
			"replica_source is '$got', expected '$tgt_ref' -- the same string the baseline sync wrote as this source's replica_target"
	else
		results_row "$CSV" "$sc" new_target_replica_source "" "" "" "" "" "" "SKIP the pair fans out to several targets"
	fi

	if [ -n "$src_ref" ]; then
		got="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replica_targets)"
		case ",$got," in
		*",$src_ref,"*) fo_ok=0 ;;
		*) fo_ok=1 ;;
		esac
		fo_check "$sc" "the new source records who it now replicates to" "$fo_ok" \
			"replica_targets is '$got', which does not contain '$src_ref' -- the same string the baseline sync wrote as this target's replica_source"
	else
		results_row "$CSV" "$sc" new_source_replica_targets "" "" "" "" "" "" "SKIP the baseline recorded no replica_source to compare against"
	fi

	# The promotion record goes with the promotion. Leaving it would make a
	# domain that is now an ordinary source still read as failed-over-to.
	got="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" promoted_at)"
	if [ -z "$got" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the promotion record is cleared by the inversion" "$fo_ok" \
		"promoted_at is still '$got' on a domain that is now simply the source"

	# --- the old source's checkpoint chain is discarded ----------------------
	# It describes a chain running the other way, a later fail-back would
	# chain onto it, and it blocks the undefine that every sync INTO this
	# domain now ends with.
	local cpts_after
	cpts_after="$(vmsync_checkpoint_count "$SOURCE_URI" "$SOURCE_DOMAIN")"
	if [ "${cpts_after:-1}" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the old source's checkpoint chain is discarded" "$fo_ok" \
		"$SOURCE_DOMAIN still has $cpts_after vmsync checkpoints after the inversion; a fail-back would chain onto a chain running the wrong way"

	# --- the warnings an operator has to act on ------------------------------
	if grep -q "must be a full one" "$RUN_LOG" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "invert says the first reversed sync must be full" "$fo_ok" \
		"no such warning in $RUN_LOG -- there is no checkpoint chain this way round, and an operator not told that will try an incremental"

	# THE check this stage exists for.
	if [ -z "$new_target_dir" ] || [ -z "$new_source_dir" ]; then
		warn "SKIP the -target-disk-path warning check: one of the domains keeps its disks in more than one directory, which is the case vmsync deliberately says nothing about"
		results_row "$CSV" "$sc" disk_path_warning "" "" "" "" "" "" "SKIP disks span several directories"
	elif [ "$new_target_dir" = "$new_source_dir" ]; then
		# Symmetric layout: leaving -target-disk-path unset already puts the
		# copy at the source's own path, so a warning would be noise. Assert
		# the SILENCE -- a warning that always fires teaches operators to
		# ignore it.
		if grep -q "different directories" "$RUN_LOG" 2>/dev/null; then fo_ok=1; else fo_ok=0; fi
		fo_check "$sc" "no disk-path warning when both ends use the same directory" "$fo_ok" \
			"invert warned about differing directories when both ends use $new_source_dir"
	else
		if grep -q "different directories" "$RUN_LOG" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "invert warns that the reversed sync needs a different -target-disk-path" "$fo_ok" \
			"the two ends keep disks in different directories ($new_source_dir vs $new_target_dir) and nothing in $RUN_LOG said so -- reusing the old value would orphan $SOURCE_DOMAIN's disks"

		if grep -q -- "-target-disk-path $new_target_dir" "$RUN_LOG" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the warning names the directory to use" "$fo_ok" \
			"the warning does not name '$new_target_dir', which is where $SOURCE_DOMAIN actually keeps its disks -- a warning an operator cannot act on is not one"
	fi

	# --- the interlock: the OLD direction is now refused ---------------------
	# The half that makes an interrupted inversion survivable. Syncing the
	# old way round would overwrite the new source with its own replica, and
	# the role gate is what stands between a cron job and exactly that.
	#
	# Nothing is written by this: the gate runs immediately after the target
	# connection, long before -reinit's disk removal or any copy.
	bench_sync "$sc" old-direction
	if [ "$RUN_RC" != 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "a sync in the old direction is refused after inverting" "$fo_ok" \
		"a sync exited 0 into $TARGET_DOMAIN, which is now the SOURCE of this pair -- it would overwrite the original with its own replica. See $RUN_LOG"

	# --- idempotence ---------------------------------------------------------
	# An inversion is two metadata writes on two hosts, so the second can
	# fail with the first done. Re-running is the documented recovery, and it
	# has to be safe on a pair that is already fully inverted.
	vmsync_verb_pair "$sc" invert-again -invert
	if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "inverting an already-inverted pair succeeds" "$fo_ok" \
		"exit $RUN_RC -- re-running is the recovery for a half-applied inversion, so it must not fail on a complete one: $(log_reason "$RUN_LOG")"

	if grep -qi "already inverted" "$RUN_LOG" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "and says it had nothing to do" "$fo_ok" \
		"no 'already inverted' in $RUN_LOG -- a second run reporting success without saying it was a no-op reads as having done the work twice"

	invert_cleanup
	trap - EXIT
	return 0
}

# log_reason FILE -- the line from a vmsync log most likely to say why it
# failed, for putting in an assertion's failure detail.
#
# Worth the few lines. "see <path>" is a fine pointer when somebody is sitting
# at the machine, and useless in a pasted report -- which is how these results
# actually get read. A stage that fails without quoting the reason costs a
# whole extra run to diagnose.
#
# Prefers the last ERROR/FATAL line, since vmsync's trace output ends with its
# own summary lines after the real cause; falls back to the last non-empty
# line. Newlines and commas are squashed because this lands in a CSV field.
log_reason() {
	local f="$1" line=""
	[ -f "$f" ] || { printf 'no log at %s' "$f"; return 0; }
	line="$(grep -iE 'ERROR|FATAL' "$f" 2>/dev/null | tail -1)"
	[ -z "$line" ] && line="$(grep -v '^[[:space:]]*$' "$f" 2>/dev/null | tail -1)"
	printf '%s' "$line" | tr '\n' ' ' | tr ',' ';' | cut -c1-400
}

# vmsync_verb_pair SCENARIO PHASE ARGS... -- run a vmsync verb that spans BOTH
# ends, from here.
#
# -invert is the only one: it writes to the old source and the promoted
# replica, so unlike -promote it cannot run on one host with a local URI.
# Distinct from run_vmsync, which adds a prometheus textfile and the transport
# flags a sync needs and a verb rejects.
vmsync_verb_pair() {
	local scenario="$1" phase="$2"
	shift 2
	local log_file="$RUN_DIR/logs/${scenario}.${phase}.log"
	RUN_LOG="$log_file"
	local args=(
		-source-uri "$SOURCE_URI" -source-domain "$SOURCE_DOMAIN"
		-target-uri "$TARGET_URI" -target-domain "$TARGET_DOMAIN"
	)
	[ -n "${SSH_USER:-}" ] && args+=(-ssh-user "$SSH_USER")
	[ -n "${SSH_KEY:-}" ] && args+=(-ssh-key "$SSH_KEY")
	[ -n "${SSH_PORT:-}" ] && args+=(-ssh-port "$SSH_PORT")
	[ -n "${SSH_KNOWN_HOSTS:-}" ] && args+=(-ssh-known-hosts "$SSH_KNOWN_HOSTS")
	args+=("$@")
	log "-> $scenario/$phase"
	log "   $VMSYNC_BIN ${args[*]}"
	set +e
	"$VMSYNC_BIN" "${args[@]}" >"$log_file" 2>&1
	RUN_RC=$?
	set -e
	return 0
}

# domain_disk_dir URI DOMAIN -> the single directory this domain keeps its
# disks in, or empty when they are spread across more than one.
#
# Empty rather than a first answer on purpose: it mirrors vmsync's own
# singleDiskDir, which says nothing at all about a scattered domain because
# there is no single -target-disk-path that could describe one.
# domblklist rather than xmllint on the domain XML, unlike disk_source_path
# beside it: --xpath on an attribute returns nodes as `file="..." file="..."`
# on one line, which only splits back apart correctly if no path contains a
# space. virsh prints one disk per row and already has to be installed.
#
# $2 == "disk" drops cdroms and floppies, which have source paths and are not
# what a replica is made of.
domain_disk_dir() {
	virsh_uri "$1" domblklist "$2" --details 2>/dev/null \
		| awk '$2 == "disk" && $4 ~ /^\// { p = $4; sub(/\/[^\/]*$/, "", p); print p }' \
		| sort -u \
		| awk 'NR==1 { d=$0 } NR>1 { exit 1 } END { if (NR==1) print d }'
}

# domain_disk_devs URI DOMAIN -> every disk target dev, one per line.
#
# Every DISK, not just the qcow2 ones source_qcow_devs reports: this exists to
# enumerate what an external snapshot would touch if not told otherwise, and
# libvirt will happily put a qcow2 overlay over a raw disk as well. Narrowing
# it to qcow2 would leave exactly the disks that need excluding un-excluded.
#
# domblklist over xmllint for the reason domain_disk_dir gives beside it, and
# $2 == "disk" to drop cdroms and floppies, which have no chain worth touching.
domain_disk_devs() {
	virsh_uri "$1" domblklist "$2" --details 2>/dev/null \
		| awk '$2 == "disk" && $3 != "" { print $3 }'
}

# vmsync_checkpoint_count URI DOMAIN -> how many vmsync-managed checkpoints
# the domain carries.
#
# Only vmsync's own: a domain may legitimately carry checkpoints somebody
# else created, and counting those would make "the chain was discarded" fail
# for a reason that has nothing to do with vmsync.
vmsync_checkpoint_count() {
	virsh_uri "$1" checkpoint-list "$2" --name 2>/dev/null \
		| grep -c '^vmsync-cpt-' || true
}
vmsync_verb() {
	local scenario="$1" phase="$2"
	shift 2
	local log_file="$RUN_DIR/logs/${scenario}.${phase}.log"
	RUN_LOG="$log_file"
	# -target-domain is passed even though the two read-only verbs this
	# started out serving (-list-restore-points, -clone-restore-point) work
	# off the filesystem alone and ignore it. -restore-restore-point and
	# -update-role do not: both act on the domain, and both refuse without
	# it. Leaving it out made every verb that touches libvirt fail here for a
	# reason that had nothing to do with what was being tested.
	local args=(-target-uri "$TARGET_URI" -target-domain "$TARGET_DOMAIN")
	# VERB_OMIT_DISK_PATH=yes leaves -target-disk-path off, which is how the
	# restore's ability to find its own restore points gets tested: it reads
	# the directory off the target domain, and the flag would mask a failure
	# to do so.
	[ "${VERB_OMIT_DISK_PATH:-no}" = yes ] || args+=(-target-disk-path "$TARGET_DISK_PATH")
	[ -n "${SSH_USER:-}" ] && args+=(-ssh-user "$SSH_USER")
	[ -n "${SSH_KEY:-}" ] && args+=(-ssh-key "$SSH_KEY")
	[ -n "${SSH_PORT:-}" ] && args+=(-ssh-port "$SSH_PORT")
	[ -n "${SSH_KNOWN_HOSTS:-}" ] && args+=(-ssh-known-hosts "$SSH_KNOWN_HOSTS")
	args+=("$@")
	log "-> $scenario/$phase"
	log "   $VMSYNC_BIN ${args[*]}"
	"$VMSYNC_BIN" "${args[@]}" >"$log_file" 2>&1
}
# source_qcow_devs -- every qcow2 disk target dev on the source domain, one
# per line, in the order libvirt reports them (which is the order vmsync
# itself iterates, so the LAST one here is the one -test=fail-last-disk
# refuses).
source_qcow_devs() {
	virsh_uri "$SOURCE_URI" dumpxml "$SOURCE_DOMAIN" 2>/dev/null \
		| xmllint --xpath "//disk[driver/@type='qcow2']/target/@dev" - 2>/dev/null \
		| tr ' ' '\n' \
		| sed -n 's/.*dev="\([^"]*\)".*/\1/p'
}

# target_base_fingerprints -- "dev mtime size blocks" for every target base
# image, one per line.
#
# mtime is the assertion that matters: qemu-img commit writes the base, so a
# disk that committed has a newer one. Size and allocated blocks ride along
# because they move too, and a fingerprint agreeing on all three is much
# harder to pass by accident than one resting on a single number.
#
# Reads the paths from the TARGET's own domain XML rather than deriving them,
# so a relocated -target-disk-path is followed rather than guessed at.
target_base_fingerprints() {
	local dev path
	while read -r dev; do
		[ -n "$dev" ] || continue
		path="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$dev")" || true
		if [ -z "$path" ]; then
			printf '%s MISSING\n' "$dev"
			continue
		fi
		printf '%s %s\n' "$dev" \
			"$(ssh_host_cmd "$TARGET_HOST" stat -c '%Y %s %b' "$path" 2>/dev/null | tr -d '\r')"
	done < <(source_qcow_devs)
}

# target_disk_paths -> the target's own path for every qcow2 disk of the source
# domain, one per line.
#
# Read off the TARGET domain's own XML, never composed from TARGET_DISK_PATH,
# for the reason target_base_fingerprints reads it too: that XML is what the
# replica actually is. After an interrupted rebuild it is also the thing under
# test, because it still describes the copy the rebuild replaced -- which is
# exactly why every other promotion check passes.
#
# The devs are collected with mapfile before anything is run over SSH. A
# `while read` fed by a process substitution shares its stdin with every
# command in the loop body, and ssh reads stdin by default, so an ssh call
# inside such a loop can swallow the remaining disks and silently shorten the
# list this harness then asserts against.
target_disk_paths() {
	local -a devs=()
	local dev path
	mapfile -t devs < <(source_qcow_devs)
	# Guarded because "${devs[@]}" on an empty array is an unbound variable
	# under `set -u` on bash before 4.4, which would end the whole harness
	# rather than this function.
	[ "${#devs[@]}" -gt 0 ] || return 0
	for dev in "${devs[@]}"; do
		[ -n "$dev" ] || continue
		path="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$dev")" || true
		[ -n "$path" ] && printf '%s\n' "$path"
	done
	return 0
}

# sweep_replaced_disks -- remove every .vmsync-replaced-* file beside the
# target's disks.
#
# Two call sites need it for two different reasons. Before an interruption is
# staged, because "exactly ONE aside file per disk" is the assertion and a set
# left by the baseline -reinit ten seconds earlier would fail it for a reason
# that has nothing to do with vmsync. After a stage that forced rename mode,
# because a full-size copy per disk left on the target's filesystem is how a
# long run fills it and then fails three stages later for a cause nobody
# connects back here.
sweep_replaced_disks() {
	local -a paths=()
	local path
	mapfile -t paths < <(target_disk_paths)
	[ "${#paths[@]}" -gt 0 ] || return 0
	for path in "${paths[@]}"; do
		[ -n "$path" ] || continue
		ssh_host_cmd "$TARGET_HOST" "rm -f '${path}'.vmsync-replaced-*" >/dev/null 2>&1 || true
	done
	return 0
}

# kill_orphaned_target_exports -- stop any qemu-nbd the TARGET host is still
# running for this domain, and remove its pidfile.
#
# Only stage 16 needs this, and only because of what its fault does. Every
# other failure path in vmsync stops its own exports: the deferred cleanup
# runs, or the signal handler replays the registered stop commands.
# -test=die-writing-base calls os.Exit with the write export still open, and
# that is the point of it -- nothing unwinds, exactly as nothing unwinds after
# a power cut. What it leaves behind on the target is a qemu-nbd holding a disk
# file open.
#
# It breaks none of this stage's assertions, since nothing here opens the
# replica's disks. What it does is hold that file's blocks after a later `rm`,
# so an unswept run would leak a process and a disk's worth of space per
# invocation -- and this harness is run repeatedly against the same target.
#
# The pidfile name is vmsync's own (vmsync-qemu-nbd-<target domain>-<dev>.pid
# in -target-runtime-dir, which this harness never overrides, so /run/vmsync).
# The `[ -f ]` guard is there because an unmatched glob arrives at the remote
# shell as its own literal text.
kill_orphaned_target_exports() {
	local dir="${TARGET_RUNTIME_DIR:-/run/vmsync}"
	ssh_host_cmd "$TARGET_HOST" \
		"for p in ${dir}/vmsync-qemu-nbd-${TARGET_DOMAIN}-*.pid; do [ -f \"\$p\" ] || continue; kill -9 \"\$(cat \"\$p\")\" 2>/dev/null || true; rm -f \"\$p\"; done" \
		>/dev/null 2>&1 || true
	return 0
}

# abort_orphaned_source_backup -- end the libvirt pull-backup job a killed run
# left running on the SOURCE domain.
#
# The other half of the same wreckage, and the more damaging half. A fault that
# exits without unwinding skips vmsync's own `abortBackup` cleanup, so the
# source keeps an active block job; libvirt allows one per domain, and vmsync
# refuses at preflight while one exists. Left behind, it does not merely fail
# the next assertion in this stage -- it blocks EVERY later sync of this domain
# on this host, including the ones other stages depend on, until a person runs
# domjobabort by hand. The harness broke it, so the harness clears it.
#
# Tolerant of everything: a run that died before BackupBegin has no job to
# abort, and "no job is active" is the state this wants either way.
abort_orphaned_source_backup() {
	virsh_uri "$SOURCE_URI" domjobabort "$SOURCE_DOMAIN" >/dev/null 2>&1 || true
	return 0
}

# journal_safe_key DOMAIN -- the filename component actionlog.SafeKey produces.
#
# Reimplemented here rather than asked of vmsync, and in the same order
# pkg/actionlog's own replacer lists: "%" first, so a "%" the domain really
# contains cannot be confused with one this encoding introduced, then "/" and
# then " ". A harness that guessed the filename instead would look at a path
# that does not exist and report a missing journal for every domain whose name
# has a space in it -- a failure indistinguishable from the one this stage is
# actually for.
journal_safe_key() {
	local s="$1"
	s="${s//%/%%}"
	s="${s//\//%2f}"
	s="${s// /%20}"
	printf '%s' "$s"
}

# journal_file -> the target-side path of TARGET_DOMAIN's action journal, or
# empty when the disk directory cannot be worked out.
#
# <dir of the replica's disks>/.vmsync-journal/<SafeKey(domain)>.jsonl, which
# is actionlog.Root(diskPath) joined with actionlog.File(). TARGET_DISK_PATH
# when it is set, because that is where the sync places the disks and
# therefore the journal; otherwise the directory the target domain's own disks
# are already in, which domain_disk_dir reports only when they all share one.
journal_file() {
	local dir="${TARGET_DISK_PATH:-}"
	[ -n "$dir" ] || dir="$(domain_disk_dir "$TARGET_URI" "$TARGET_DOMAIN")"
	[ -n "$dir" ] || return 0
	printf '%s/.vmsync-journal/%s.jsonl' "${dir%/}" "$(journal_safe_key "$TARGET_DOMAIN")"
}

# Stage 15: the commit barrier.
#
# Proves the property the barrier exists for, and the only one that needed a
# multi-disk domain to state: when ONE disk fails, NO disk commits.
#
# Before the barrier, each disk committed inside its own worker. Disks copy
# concurrently, so a fast disk could merge its delta into the base while a
# slow one was still transferring -- and if that slow one then failed, the
# target was left holding one disk at the new checkpoint and one at the old,
# with nothing recorded as having happened. A promotion of that replica boots
# a guest whose disks disagree about what time it is.
stage_commit_barrier() {
	log "=== Stage 15: commit barrier (one disk fails, none commit) ==="

	if [ "$DRY_RUN" != yes ]; then
		stage_needs_target_shutoff "$CSV" commit-barrier "stage commit-barrier" || return 0
		if ! command -v xmllint >/dev/null 2>&1; then
			warn "SKIP stage 15: xmllint is required to enumerate the source's disks"
			results_row "$CSV" commit-barrier precondition "" "" "" "" "" "" "SKIP xmllint unavailable"
			return 0
		fi
	fi

	local -a devs=()
	if [ "$DRY_RUN" != yes ]; then
		mapfile -t devs < <(source_qcow_devs)
		if [ "${#devs[@]}" -lt 2 ]; then
			# Not a failure, and worth saying precisely: the barrier is still
			# doing its job on this domain, there is simply no second disk for
			# it to protect, so the assertion below would pass without testing
			# anything.
			warn "SKIP stage 15: $SOURCE_DOMAIN has ${#devs[@]} qcow2 disk(s) and this stage needs at least two -- one disk cannot fail while another commits"
			results_row "$CSV" commit-barrier precondition "" "" "" "" "" "" "SKIP needs a multi-disk source domain"
			return 0
		fi
		log "source disks: ${devs[*]} -- -test=fail-last-disk will refuse ${devs[*]: -1}"
	fi

	bench_sync commit-barrier baseline -reinit
	if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
		warn "baseline full sync failed (see $RUN_LOG) -- aborting stage 15$(bench_sync_hint)"
		results_row "$CSV" commit-barrier baseline "" "" "" "" "" "" "FAIL baseline sync failed"
		return 1
	fi

	# A real delta, or the incremental below copies nothing, the fault never
	# fires, and the stage passes having tested nothing -- the same vacuous
	# pass stage 13 documents.
	local dirtied=no
	if [ "$DRY_RUN" != yes ] && [ "$GUEST_DIRTY" = yes ] && guest_exec_available; then
		if guest_dirty; then
			dirtied=yes
		fi
	fi
	if [ "$DRY_RUN" != yes ] && [ "$dirtied" != yes ]; then
		warn "SKIP stage 15: could not dirty the guest (${GUEST_EXEC_WHY:-GUEST_DIRTY=no}), so the incremental would copy nothing and the fault would never fire"
		results_row "$CSV" commit-barrier precondition "" "" "" "" "" "" "SKIP could not produce a delta"
		return 0
	fi

	local before after
	if [ "$DRY_RUN" != yes ]; then
		before="$(target_base_fingerprints)"
		log "target bases before the failing run: $(printf '%s' "$before" | tr '\n' '|')"
	fi

	# The run that must fail, and must leave every base untouched.
	bench_sync commit-barrier fail "-test=fail-last-disk"

	if [ "$DRY_RUN" = yes ]; then
		results_row "$CSV" commit-barrier dry-run "" "" "" "" "" "" "SKIP dry run"
		return 0
	fi

	if [ "$RUN_RC" = 0 ]; then
		warn "FAIL: the sync succeeded with -test=fail-last-disk, which is supposed to fail one disk deliberately. Either the fault did not fire or its error was swallowed -- see $RUN_LOG"
		results_row "$CSV" commit-barrier fault-fires "" "" "" "" "" "" "FAIL the deliberately-failed run reported success"
		return 1
	fi

	# A non-zero exit is NOT enough on its own, and relying on it made this
	# whole stage vacuous.
	#
	# vmsync refuses an unknown -test value at flag validation and exits 2
	# before it copies anything. So against a build where fail-last-disk had
	# been removed -- or where the barrier never existed -- this run would
	# exit non-zero, no disk would be touched, the fingerprints below would be
	# trivially unchanged, and all three assertions would report PASS. The
	# stage would be green precisely when the feature was absent.
	#
	# So: prove the fault fired where it was supposed to, and prove real work
	# happened first. The fault is injected AFTER the copy and digest check,
	# so a genuine run of it leaves both markers behind.
	if ! grep -q "fail-last-disk: failing this disk deliberately" "$RUN_LOG"; then
		warn "FAIL: the run exited non-zero but never logged the fail-last-disk injection, so it failed for some OTHER reason -- most likely refused at preflight before copying anything. That would make every assertion below pass vacuously. See $RUN_LOG"
		results_row "$CSV" commit-barrier fault-fires "" "" "" "" "" "" "FAIL run failed before the fault could fire"
		return 1
	fi
	local verified_disks
	verified_disks="$(grep -c "checksum: target contents match what was sent" "$RUN_LOG" || true)"
	if [ "${verified_disks:-0}" -lt 2 ]; then
		warn "FAIL: only $verified_disks disk(s) got as far as a verified delta before the fault fired. The barrier can only be tested when at least one OTHER disk was ready to commit, so this run proves nothing. See $RUN_LOG"
		results_row "$CSV" commit-barrier fault-fires "" "" "" "" "" "" "FAIL only $verified_disks disk(s) staged; nothing for the barrier to hold back"
		return 1
	fi
	log "the fault fired after $verified_disks disks had verified deltas ready to commit"
	results_row "$CSV" commit-barrier fault-fires "" "" "" "" "" "" "PASS the run failed as intended"

	after="$(target_base_fingerprints)"
	log "target bases after the failing run: $(printf '%s' "$after" | tr '\n' '|')"

	if [ "$before" != "$after" ]; then
		# THE assertion. A changed fingerprint means a disk merged its delta
		# into its base while a sibling was failing, which is exactly the
		# mixed-checkpoint replica the barrier exists to prevent.
		warn "FAIL: a target base image changed during a run that FAILED. One disk committed while another did not, leaving this replica with its disks at different checkpoints. Before: $(printf '%s' "$before" | tr '\n' '|') After: $(printf '%s' "$after" | tr '\n' '|')"
		results_row "$CSV" commit-barrier no-partial-commit "" "" "" "" "" "" "FAIL a disk committed while another failed"
		return 1
	fi
	log "no target base image changed: one disk failing stopped every other disk from committing"
	results_row "$CSV" commit-barrier no-partial-commit "" "" "" "" "" "" "PASS no disk committed when one failed"

	# And the barrier's failure must not look like an interrupted rebuild.
	#
	# The two are opposites and the difference is exactly what
	# replica_incomplete is for. A run that discards every overlay leaves the
	# replica's bases untouched -- the assertion above just proved it -- so
	# the replica is whole, correctly described by its own metadata, and
	# promotable. An interrupted FULL copy leaves half-written bases under
	# metadata describing the copy they replaced, and must refuse. An
	# incremental that armed the field anyway would refuse a promotion of a
	# perfectly good replica on the day of a failover, which is the worst
	# possible time to be wrong in that direction.
	local barrier_marker
	barrier_marker="$(replica_incomplete "$TARGET_URI" "$TARGET_DOMAIN")"
	if [ -z "$barrier_marker" ]; then
		log "the discarded run left no replica_incomplete: a barrier that throws every overlay away has not touched the replica"
		results_row "$CSV" commit-barrier no-incomplete-marker 0 "" "" "" "" "" "PASS a discarded run arms nothing"
	else
		warn "FAIL: the target carries replica_incomplete='$barrier_marker' after a run the barrier discarded entirely. That run left every base exactly as it found it, so this replica is whole -- and -promote will now refuse it, naming a rebuild that never touched these disks"
		results_row "$CSV" commit-barrier no-incomplete-marker 1 "" "" "" "" "" "FAIL a discarded run left the marker armed"
	fi

	# And the barrier must not have broken the ordinary path: a clean
	# incremental has to commit, and the bases have to move. Without this the
	# stage would pass just as happily against a vmsync that had stopped
	# committing altogether.
	if ! guest_dirty; then
		warn "could not dirty the guest again -- skipping the recovery sub-test, so this stage proved that nothing commits on failure but NOT that anything commits on success"
		results_row "$CSV" commit-barrier recovers "" "" "" "" "" "" "SKIP could not produce a second delta"
		return 0
	fi
	bench_sync commit-barrier recover
	if [ "$RUN_RC" != 0 ]; then
		warn "FAIL: a clean incremental after the deliberately-failed one did not succeed (see $RUN_LOG)$(bench_sync_hint)"
		results_row "$CSV" commit-barrier recovers "" "" "" "" "" "" "FAIL clean incremental failed after the fault run"
		return 1
	fi
	local recovered
	recovered="$(target_base_fingerprints)"
	if [ "$recovered" = "$after" ]; then
		warn "FAIL: a clean incremental reported success but no target base changed -- the barrier is discarding deltas it should be committing"
		results_row "$CSV" commit-barrier recovers "" "" "" "" "" "" "FAIL clean incremental committed nothing"
		return 1
	fi
	log "the clean incremental committed: the barrier blocks a failed run without blocking a good one"
	results_row "$CSV" commit-barrier recovers "" "" "" "" "" "" "PASS clean incremental still commits"
	return 0
}

# --- Stage 16: an interrupted rebuild ----------------------------------------
#
# The stage for the one failure a promotion would otherwise accept without a
# word.
#
# A full copy -- `-reinit`, `-force-clean`, or any sync whose computed parent
# is empty -- renames the good replica disks aside and writes NEW base images
# directly, with no overlay to commit at the end, while the target domain keeps
# its OLD metadata until the very last write. A run killed in that window
# leaves the target saying last_checkpoint=<old>, last_sync_timestamp=<old>,
# replica_source=<set>, failure_count=0. Every one of those is true about the
# replica the copy REPLACED and false about the half-written image sitting on
# the disks, so pkg/failover's evidence check finds nothing wrong and -promote
# would boot a half-written machine reporting an ordinary data-loss window --
# while the complete copy sits unread in the .vmsync-replaced-<unix> files
# beside it.
#
# replica_incomplete closes that, and this is the only place the closure is
# proven against a real interruption rather than against a struct literal in a
# unit test. That distinction is not academic here: the promotion gate is split
# across two packages -- pkg/failover decides from a struct, and the promote
# command has to fill that struct in from the domain -- and a field missing
# from that mapping costs nothing at build time while disabling the refusal at
# run time. It is exactly what happened to the verify record once already.
#
# It needs vmsync's own -test=die-writing-base and cannot be done any other
# way: the window is inside one process, between a qemu-img create and a
# metadata write, with no I/O an external harness could interrupt at the right
# moment. A fault that exited right after the `mv` would land in a state
# -promote ALREADY refuses on (disks missing), so the stage would be green
# precisely when the feature was absent.
stage_interrupted_reinit() {
	log "=== Stage 16: an interrupted rebuild must not be promotable ==="
	local sc=interrupted-reinit
	local fo_ok=0
	# -promote and -update-role act on the host they run on, so the target
	# host is addressed with its own LOCAL uri, never TARGET_URI.
	local local_uri="qemu:///system"

	if [ "$DRY_RUN" = yes ]; then
		# rename here too, purely so the command lines --dry-run prints are
		# the ones a real run would use. This stage overrides
		# REPLACED_DISK_ACTION for its own syncs (see below for why it has
		# to), and a dry run that printed the harness's usual `delete` would
		# be advertising a command that cannot test what this stage tests.
		IR_SAVED_REPLACED_DISK_ACTION="$REPLACED_DISK_ACTION"
		REPLACED_DISK_ACTION=rename
		bench_sync "$sc" baseline -reinit
		bench_sync "$sc" fault -reinit "-test=$VMSYNC_TEST_DIE_WRITING_BASE"
		REPLACED_DISK_ACTION="$IR_SAVED_REPLACED_DISK_ACTION"
		fo_check "$sc" "the rebuild dies instead of completing" 0
		fo_check "$sc" "the interrupted rebuild left a replica_incomplete record" 0
		fo_check "$sc" "promoting an interrupted rebuild is refused" 0
		fo_check "$sc" "-force-promote still gets through" 0
		fo_check "$sc" "putting the aside files back restores the complete replica exactly" 0
		fo_check "$sc" "a completed rebuild clears the record" 0
		return 0
	fi

	# -promote refuses a remote libvirt URI by design -- that is what keeps a
	# failover working when the other site is unreachable -- so it has to run
	# ON the target host. Saying so beats letting the attempt fail: a promote
	# that could not be run at all exits non-zero exactly like one that was
	# refused, and telling those two apart is most of what this stage does.
	if [ -z "${TARGET_VMSYNC_BIN:-}" ]; then
		warn "SKIP stage 16: TARGET_VMSYNC_BIN is not set in $CONF, and -promote must run ON $TARGET_HOST. What -promote does with an interrupted rebuild is the entire stage, so there is nothing here to test without it."
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP TARGET_VMSYNC_BIN unset"
		return 0
	fi

	stage_needs_target_shutoff "$CSV" "$sc" "stage interrupted-reinit" || return 0
	reset_pair_state "$sc"
	require_target_syncable "$sc" || return 0

	# rename, whatever bench.conf says, and this is not a preference.
	#
	# The refusal's one-line recovery is "put the .vmsync-replaced-<stamp>
	# files back", and with -replaced-disk-action=delete there are none: the
	# value carries no aside= at all and the wording deliberately branches to
	# say so rather than sending an operator hunting for files that were never
	# created. This harness defaults to delete so a long run does not fill the
	# target's filesystem (see REPLACED_DISK_ACTION near the top), which would
	# turn the three assertions this stage is built around into skips.
	#
	# A global rather than a local, because the cleanup that restores it also
	# runs from an EXIT trap, which may fire when this function is no longer
	# on the stack and its locals no longer exist.
	IR_SAVED_REPLACED_DISK_ACTION="$REPLACED_DISK_ACTION"
	REPLACED_DISK_ACTION=rename

	# The backstop for everything below. This stage promotes the target three
	# times and is responsible for putting it back each time; a die, a Ctrl+C
	# or a failed way back would otherwise leave it promoted, and a promoted
	# target refuses every sync in the estate -- not just this harness's later
	# stages. An EXIT trap rather than RETURN for the reason stages 6 and 7
	# give: RETURN does not fire on a die or on a signal, and those are
	# precisely the cases that would leave it behind.
	IR_PROMOTED=no
	IR_CLEANED=no
	interrupted_reinit_cleanup() {
		[ "$IR_CLEANED" = yes ] && return 0
		IR_CLEANED=yes
		REPLACED_DISK_ACTION="$IR_SAVED_REPLACED_DISK_ACTION"
		# Again here as the backstop: the inline call after the fault run
		# covers the ordinary path, and this covers a die or a Ctrl+C landing
		# between the fault and it. Running it twice costs one ssh and finds
		# nothing the second time.
		kill_orphaned_target_exports
		abort_orphaned_source_backup
		[ "$IR_PROMOTED" = yes ] || return 0
		if clear_target_promotion; then
			log "stage 16: the target is back to role=target"
		else
			warn_target_still_promoted
		fi
	}
	trap 'interrupted_reinit_cleanup' EXIT

	# --- baseline: a complete replica worth losing --------------------------
	bench_sync "$sc" baseline -reinit
	if [ "$RUN_RC" != 0 ]; then
		warn "baseline full sync failed (see $RUN_LOG) -- aborting stage 16 before anything is interrupted$(bench_sync_hint)"
		results_row "$CSV" "$sc" baseline "" "" "" "" "" "" "FAIL baseline sync failed"
		interrupted_reinit_cleanup
		trap - EXIT
		return 1
	fi

	# Guest writes between the baseline and the interrupted rebuild, so the
	# copy in the aside files is demonstrably not the same bytes the
	# half-written one holds.
	#
	# NOT a precondition, unlike stage 15's: a full copy writes every allocated
	# byte whether the guest moved or not, so the fault fires either way. This
	# only makes the recovery assertion further down a comparison of two
	# genuinely different copies rather than of two copies of the same data.
	[ "$GUEST_DIRTY" = yes ] && { wait_for_guest_agent || true; }
	if [ "$GUEST_DIRTY" = yes ] && guest_exec_available; then
		guest_dirty || warn "could not dirty the guest; the assertions below still hold, they just compare copies that may be identical"
	else
		log "   (guest-exec unavailable: ${GUEST_EXEC_WHY:-GUEST_DIRTY=no} -- the rebuild will copy the same bytes the baseline did)"
	fi

	# The baseline above renamed its own predecessor aside, and this stage is
	# about to assert "exactly ONE aside file per disk". Leaving that set
	# behind would fail that assertion for a reason that has nothing to do
	# with vmsync.
	sweep_replaced_disks

	local -a disks=()
	mapfile -t disks < <(target_disk_paths)
	if [ "${#disks[@]}" -eq 0 ]; then
		warn "SKIP stage 16: no target disk path could be resolved from $TARGET_DOMAIN's own XML via $TARGET_URI, so the aside files cannot be found or counted"
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP target disk paths unresolved"
		interrupted_reinit_cleanup
		trap - EXIT
		return 0
	fi

	local cp_before sync_before fingerprints_before
	cp_before="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_checkpoint)"
	sync_before="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_sync_timestamp)"
	fingerprints_before="$(target_base_fingerprints)"
	log "before the interrupted rebuild: last_checkpoint='$cp_before' disks=${disks[*]}"

	# --- the interruption ---------------------------------------------------
	bench_sync "$sc" fault -reinit "-test=$VMSYNC_TEST_DIE_WRITING_BASE"
	local fault_log fault_rc
	fault_log="$RUN_LOG"
	fault_rc="$RUN_RC"

	# Immediately, before anything else looks at the target. The fault exits
	# without unwinding, so the target-side qemu-nbd it started is still there
	# holding a disk file open -- see kill_orphaned_target_exports for why
	# leaving it costs a process and a disk's worth of space per run.
	kill_orphaned_target_exports
	# And the source's backup job, for a sharper reason: every later sync of
	# this domain refuses while it is active, so leaving it would fail the
	# stages after this one for a cause that has nothing to do with them.
	abort_orphaned_source_backup

	if [ "$fault_rc" != 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the rebuild dies instead of completing" "$fo_ok" \
		"vmsync exited 0 under -test=$VMSYNC_TEST_DIE_WRITING_BASE, which is supposed to kill it -- see $fault_log"

	# A non-zero exit proves nothing on its own, and resting on it would make
	# this entire stage vacuous. vmsync refuses an unknown -test value at flag
	# validation and exits 2 having touched nothing at all, so against a build
	# where this fault had been renamed or removed the run would exit
	# non-zero, the target would be untouched, and several assertions below
	# would pass trivially -- green precisely when the feature was gone. The
	# line the injection itself writes is what proves it fired, and where.
	local fired=no
	if grep -q "$VMSYNC_TEST_DIE_WRITING_BASE: killing this process NOW" "$fault_log" 2>/dev/null; then
		fired=yes
		fo_ok=0
	else
		fo_ok=1
	fi
	fo_check "$sc" "the fault fired after a base had been written" "$fo_ok" \
		"the run ended (exit $fault_rc) without logging the injection, so it stopped for some OTHER reason -- refused at flag validation, refused at preflight, or failed before writing anything. Any of those would leave the target in a state -promote already refuses on, and every check below would report a pass for a refusal that was never exercised. See $fault_log"

	if [ "$fired" != yes ]; then
		warn "the interrupted-rebuild state was never reached, so the remaining checks are skipped rather than reported as a wall of separate failures -- fix that first, nothing here can say anything until the fault fires"
		results_row "$CSV" "$sc" interruption_dependent_checks "" "" "" "" "" "" "SKIP the fault did not fire"
		interrupted_reinit_cleanup
		trap - EXIT
		return 0
	fi

	# 137 specifically, not merely non-zero. An orderly failure would have
	# stopped the exports, discarded what it wrote and recorded an outcome --
	# none of which a power cut, an OOM kill or vmsync's own SIGTERM handler
	# does, and none of which leaves the state this stage is about.
	if [ "$fault_rc" = 137 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the rebuild was killed rather than failed, so nothing unwound" "$fo_ok" \
		"exit $fault_rc, want 137 -- a run that unwound tidily would be testing the recovery path instead of the one there is no recovery from"

	# --- what the target says about itself now ------------------------------
	local marker stamp verb aside_keys
	marker="$(replica_incomplete "$TARGET_URI" "$TARGET_DOMAIN")"
	if [ -n "$marker" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the interrupted rebuild left a replica_incomplete record" "$fo_ok" \
		"there is none on $TARGET_DOMAIN, so -promote has nothing to refuse on and would accept these half-written disks reporting an ordinary data-loss window -- the exact defect this field exists to close"

	if [ -z "$marker" ]; then
		warn "nothing was recorded on the target, so every check that reads that record is skipped rather than reported separately"
		results_row "$CSV" "$sc" record_dependent_checks "" "" "" "" "" "" "SKIP no replica_incomplete was written"
		interrupted_reinit_cleanup
		trap - EXIT
		return 0
	fi
	log "replica_incomplete: $marker"

	# In the PERSISTENT definition, which is the only copy that outlives the
	# process that wrote it and the only one -promote reads. Asserted against
	# `dumpxml --inactive` rather than through the metadata API because that
	# is also the command the runbook sends an operator to during an incident:
	# a record only the API can see is one nobody finds.
	if virsh_uri "$TARGET_URI" dumpxml "$TARGET_DOMAIN" --inactive 2>/dev/null | grep -q 'replica_incomplete'; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the record is in the domain's persistent definition" "$fo_ok" \
		"virsh dumpxml --inactive $TARGET_DOMAIN does not mention replica_incomplete, so whatever the metadata API just returned would not survive a libvirtd restart -- and -promote reads the inactive definition"

	verb="$(replica_incomplete_key "$marker" verb)"
	if [ "$verb" = reinit ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the record names the verb that was interrupted" "$fo_ok" \
		"verb='$verb' in '$marker', want 'reinit' -- the refusal quotes this back, and an operator who cannot tell an interrupted reinit from an interrupted restore cannot tell which recovery applies"

	stamp="$(replica_incomplete_key "$marker" aside)"
	aside_keys="$(replica_incomplete_key_count "$marker" aside)"
	if [ -n "$stamp" ] && [ "$aside_keys" = 1 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the record carries exactly ONE aside stamp" "$fo_ok" \
		"aside appears ${aside_keys:-0} time(s) in '$marker'. This field is single-valued and never appended to, and a second stamp would name a second displaced set that does not exist -- leaving the refusal unable to say which files to put back"

	# The rest of the metadata must be untouched, and that is the point rather
	# than a detail. That last_checkpoint, last_sync_timestamp and the others
	# still look perfectly healthy is the whole reason this replica is
	# promotable without the refusal; a stage where the rebuild had cleared
	# them would be proving the refusal against a target that the ordinary
	# evidence checks would have caught on its own.
	local cp_after sync_after
	cp_after="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_checkpoint)"
	sync_after="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" last_sync_timestamp)"
	if [ -n "$cp_after" ] && [ "$cp_after" = "$cp_before" ] && [ "$sync_after" = "$sync_before" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the rest of the metadata still describes the REPLACED replica" "$fo_ok" \
		"last_checkpoint '$cp_before' -> '$cp_after' and last_sync_timestamp '$sync_before' -> '$sync_after'; both must be unchanged, because the refusal is the only thing standing between those healthy-looking fields and a promoted half-written disk"

	# --- the aside files ----------------------------------------------------
	# One stamp names one displaced machine. Two stamps across a multi-disk
	# domain name no coherent set at all, and the refusal could then tell an
	# operator nothing useful about what to put back.
	local path listing n aside_bad=0 aside_detail=""
	for path in "${disks[@]}"; do
		listing="$(ssh_host_cmd "$TARGET_HOST" "ls -1 '${path}'.vmsync-replaced-* 2>/dev/null || true")"
		n="$(printf '%s' "$listing" | grep -c . || true)"
		if [ "${n:-0}" != 1 ]; then
			aside_bad=$((aside_bad + 1))
			aside_detail="${aside_detail}${aside_detail:+; }$path has ${n:-0} aside file(s)"
			continue
		fi
		if [ "$listing" != "${path}.vmsync-replaced-${stamp}" ]; then
			aside_bad=$((aside_bad + 1))
			aside_detail="${aside_detail}${aside_detail:+; }$listing does not carry the stamp $stamp the record names"
		fi
	done
	if [ "$aside_bad" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "every disk has exactly one aside file and they all share that one stamp" "$fo_ok" \
		"$aside_detail"

	# --- the refusal --------------------------------------------------------
	local role_before role_after refuse_log refuse_rc
	role_before="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"

	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" refuse-promote \
		-promote -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN" \
		-promote-mode forced -promoted-by bench-harness
	refuse_log="$RUN_LOG"
	refuse_rc="$RUN_RC"

	if [ "$refuse_rc" != 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "promoting an interrupted rebuild is refused" "$fo_ok" \
		"-promote exited 0 against a replica a rebuild was part-way through replacing, so a half-written image becomes production with nobody asked to override anything -- see $refuse_log"

	# Not satisfied by the exit code, deliberately. A promote that never ran
	# -- wrong path, unreadable domain, a libvirt that would not answer --
	# exits non-zero too, and so does one refused over some unrelated piece of
	# missing evidence. Accepting any of those would record this interlock as
	# working on a run where it was never consulted.
	if grep -q 'a full copy of this replica was STARTED and never recorded as finished' "$refuse_log" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the refusal names the interrupted rebuild, not something else" "$fo_ok" \
		"-promote failed (exit $refuse_rc) but never said so in those terms, so something else stopped it and the record was very possibly never read. See $refuse_log"

	if grep -qF ".vmsync-replaced-$stamp" "$refuse_log" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the refusal names the aside files by their exact stamp" "$fo_ok" \
		"'.vmsync-replaced-$stamp' does not appear in $refuse_log. That suffix is the entire recovery and it exists nowhere else on this host: without it an operator is told their replica is unusable and not that the complete one is lying beside it"

	# Refused has to mean nothing was written. A domain left promoted by a run
	# that called itself refused is worse than either outcome alone: the
	# replica is serving and the only record of how it got there says it was
	# not allowed to. Compared against the role read BEFORE the attempt, not
	# against the literal "promoted", because vmsync_meta_field swallows every
	# error it meets and an unreadable domain would satisfy the negative form
	# without anything having been looked at.
	role_after="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
	if [ "$role_after" = "$role_before" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the refused promotion wrote nothing" "$fo_ok" \
		"replication_role moved from '$role_before' to '$role_after', so the refusal is cosmetic -- and at 'promoted' every sync into this domain is refused as well"

	# --- and the way past ---------------------------------------------------
	# An interlock with no supported override is one an operator routes around
	# by hand during an outage. This one must let them through -- the
	# half-written copy may genuinely be all that is left -- and it must say
	# what it is overriding and stop claiming to know how much data is being
	# given up.
	local force_log force_rc
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" force-promote \
		-promote -force-promote -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN" \
		-promote-mode forced -promoted-by bench-harness
	force_log="$RUN_LOG"
	force_rc="$RUN_RC"
	# Armed the moment the promotion may have taken, so the EXIT trap can put
	# the role back if anything between here and the restore below ends the
	# run instead.
	[ "$force_rc" = 0 ] && IR_PROMOTED=yes

	if [ "$force_rc" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "-force-promote still gets through" "$fo_ok" \
		"-force-promote failed too (exit $force_rc), so the documented way past this refusal does not work and an operator facing a real outage has no supported way to boot the only copy they have -- see $force_log"

	if grep -q 'promoted despite' "$force_log" 2>/dev/null \
		&& grep -q 'a full copy of this replica was STARTED and never recorded as finished' "$force_log" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the override records exactly what it overrode" "$fo_ok" \
		"$force_log does not carry both 'promoted despite' and the interrupted-rebuild finding, so this override is indistinguishable in the log from an ordinary promotion of a healthy replica -- which is the only thing separating a considered decision from a mistake when somebody reads this domain a week later"

	if [ "$force_rc" = 0 ]; then
		# The window must go UNKNOWN, not merely wide. A number here would be
		# computed from a checkpoint describing the copy this rebuild
		# replaced and printed beside disks holding something else: a
		# measurement of a replica that does not exist, which reads as
		# reassurance.
		if grep -q 'data_loss=unknown' "$force_log" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the forced promotion reports the data-loss window as UNKNOWN" "$fo_ok" \
			"no 'data_loss=unknown' in $force_log -- a figure derived from the replaced replica's own checkpoint would be a measurement of something that is no longer on these disks"
	else
		results_row "$CSV" "$sc" forced_window "" "" "" "" "" "" "SKIP the forced promotion did not happen"
	fi

	retarget_promoted_target "$sc" role-back-after-force "$local_uri"
	if [ "$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)" = target ]; then
		IR_PROMOTED=no
	else
		warn_target_still_promoted
	fi

	# Undoing the promotion must not take the finding with it. -update-role
	# clears the promotion record, which is its job; if it cleared this too,
	# one command that repairs nothing would make the next -promote accept the
	# same half-written disks.
	marker="$(replica_incomplete "$TARGET_URI" "$TARGET_DOMAIN")"
	if [ -n "$marker" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "undoing the promotion does not clear the finding" "$fo_ok" \
		"replica_incomplete is gone after -update-role=target, so the one record saying these disks are half-written can be erased by a command that does not touch them"

	# --- the recovery the refusal names -------------------------------------
	local fingerprints_broken fingerprints_restored
	fingerprints_broken="$(target_base_fingerprints)"
	if [ "$fingerprints_broken" != "$fingerprints_before" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the interrupted rebuild really did displace the replica" "$fo_ok" \
		"every target base is exactly where it was before the rebuild started, so nothing was replaced and the recovery below would be putting back a replica that never left. Before: $(printf '%s' "$fingerprints_before" | tr '\n' '|') now: $(printf '%s' "$fingerprints_broken" | tr '\n' '|')"

	local restore_failed=0
	for path in "${disks[@]}"; do
		ssh_host_cmd "$TARGET_HOST" "mv -f '${path}.vmsync-replaced-${stamp}' '${path}'" >/dev/null 2>&1 \
			|| restore_failed=$((restore_failed + 1))
	done
	if [ "$restore_failed" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the aside files can be put back over the disks" "$fo_ok" \
		"$restore_failed of ${#disks[@]} rename(s) failed on $TARGET_HOST -- this single line is what the refusal exists to be able to print"

	# Same inode moved back, so mtime, size and allocated blocks all return to
	# their exact previous values -- which the base created seconds later
	# cannot match by accident. It is the same evidence stage 15 rests on, and
	# it is why no digest of a multi-gigabyte disk is needed here.
	fingerprints_restored="$(target_base_fingerprints)"
	if [ "$fingerprints_restored" = "$fingerprints_before" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "putting the aside files back restores the complete replica exactly" "$fo_ok" \
		"after: $(printf '%s' "$fingerprints_restored" | tr '\n' '|') before: $(printf '%s' "$fingerprints_before" | tr '\n' '|')"

	# And it is STILL refused, which is the honest half of this recovery and
	# worth asserting rather than glossing over. A rename on the target host
	# tells vmsync nothing: the field is cleared by the write that records a
	# successful copy and by nothing else. So an operator who needs the
	# replica live during the outage forces the promotion -- and now boots the
	# COMPLETE copy rather than a half-written one -- while an operator with
	# time re-runs the rebuild.
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" refuse-after-restore \
		-promote -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN" \
		-promote-mode forced -promoted-by bench-harness
	if [ "$RUN_RC" != 0 ] \
		&& grep -q 'a full copy of this replica was STARTED and never recorded as finished' "$RUN_LOG" 2>/dev/null; then
		fo_ok=0
	else
		fo_ok=1
	fi
	fo_check "$sc" "restoring the files by hand does not clear the record" "$fo_ok" \
		"a plain -promote came back with exit $RUN_RC after the aside files were renamed back. Nothing on this host can tell that a rename happened, so a record a rename could clear would be a record any rename could clear -- see $RUN_LOG"

	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" force-promote-restored \
		-promote -force-promote -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN" \
		-promote-mode forced -promoted-by bench-harness
	[ "$RUN_RC" = 0 ] && IR_PROMOTED=yes
	if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the restored replica can still be promoted during the outage" "$fo_ok" \
		"-force-promote failed (exit $RUN_RC) against the complete replica the aside files just put back, so the documented recovery ends with a copy nobody can boot -- see $RUN_LOG"

	retarget_promoted_target "$sc" role-back-after-recovery "$local_uri"
	if [ "$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)" = target ]; then
		IR_PROMOTED=no
	else
		warn_target_still_promoted
	fi

	# --- and the rebuild, run to completion ---------------------------------
	# The refusal has to lift itself. There is deliberately no flag that
	# clears this field, so a rebuild that finishes is the whole cure -- and a
	# field that outlived one would leave every replica in the estate
	# force-only from its first interrupted run onwards, which turns a safety
	# refusal into noise operators learn to pass -force-promote past.
	sweep_replaced_disks
	bench_sync "$sc" heal -reinit
	local heal_sync_rc
	heal_sync_rc="$RUN_RC"
	if [ "$heal_sync_rc" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "an interrupted rebuild can simply be re-run" "$fo_ok" \
		"the repeat -reinit failed (exit $heal_sync_rc)$(bench_sync_hint) -- see $RUN_LOG"

	marker="$(replica_incomplete "$TARGET_URI" "$TARGET_DOMAIN")"
	if [ -z "$marker" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "a completed rebuild clears the record" "$fo_ok" \
		"replica_incomplete is still '$marker' after a sync that reported success, so this replica stays force-only for ever"

	local heal_log heal_rc
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" promote-after-heal \
		-promote -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN" \
		-promote-mode forced -promoted-by bench-harness
	heal_log="$RUN_LOG"
	heal_rc="$RUN_RC"
	[ "$heal_rc" = 0 ] && IR_PROMOTED=yes

	# The false-positive guard, and the reason this sub-test is not optional:
	# a refusal that fired on every replica would pass every assertion above
	# and cost a real failover the one thing it needs.
	if [ "$heal_rc" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "a plain promote is accepted again once the rebuild finished" "$fo_ok" \
		"-promote was refused (exit $heal_rc) against a replica a full sync had just completed -- see $heal_log"

	if [ "$heal_rc" = 0 ]; then
		if grep -q 'data_loss=unknown' "$heal_log" 2>/dev/null; then fo_ok=1; else fo_ok=0; fi
		fo_check "$sc" "and its data-loss window is measured rather than unknown" "$fo_ok" \
			"the promotion reported an unknown window, so something is still being treated as an evidence problem on a replica that was rebuilt successfully moments ago -- see $heal_log"
	else
		results_row "$CSV" "$sc" healed_window "" "" "" "" "" "" "SKIP the promotion after the heal did not happen"
	fi

	retarget_promoted_target "$sc" role-back "$local_uri"
	if [ "$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)" = target ]; then
		IR_PROMOTED=no
	else
		warn "the way back did not take, so this stage is leaving the pair unusable rather than as it found it"
		warn_target_still_promoted
	fi

	# The aside set this stage's own heal left behind: full-size copies that
	# nothing reaps, on a filesystem the following stages still need.
	sweep_replaced_disks

	if [ "$FAILOVER_FAILURES" -eq 0 ]; then
		log "=== Stage 16: every interrupted-rebuild assertion passed ==="
	fi

	interrupted_reinit_cleanup
	trap - EXIT
	return 0
}

# --- Stage 17: the action journal --------------------------------------------
#
# Cheap, and in the DEFAULT list because of what it guards rather than because
# of what it proves.
#
# The journal's whole value is the record of an action that DIED: a run killed
# by SIGTERM (whose handler calls os.Exit, so nothing deferred runs), a dropped
# link or a power cut writes its intent and then nothing at all, and that
# unmatched intent IS the evidence. Nothing branches on it -- it is evidence,
# never an input to a decision -- which is exactly why nothing else in this
# harness would notice it quietly ceasing to be written. An estate whose
# journals had stopped would look identical to one where nothing ever crashed,
# and every other stage here would keep reporting PASS.
#
# So this asserts the ordinary case, which is the one that CAN be asserted
# from outside: a sync that finishes leaves an intent and a matching outcome,
# in that order, joined by (aid, seq) -- and leaves no replica_incomplete
# behind. The interrupted case is stage 16's, where the fault provides the
# death this one cannot stage.
stage_journal() {
	log "=== Stage 17: the action journal ==="
	local sc=journal
	local fo_ok=0

	if [ "$DRY_RUN" = yes ]; then
		bench_sync "$sc" sync
		fo_check "$sc" "the sync writes an intent record beside the replica" 0
		fo_check "$sc" "the intent has a matching outcome recording success" 0
		fo_check "$sc" "the intent is written BEFORE the outcome" 0
		fo_check "$sc" "a finished sync leaves no replica_incomplete behind" 0
		return 0
	fi

	stage_needs_target_shutoff "$CSV" "$sc" "stage journal" || return 0

	local jfile
	jfile="$(journal_file)"
	if [ -z "$jfile" ]; then
		warn "SKIP stage 17: could not work out which directory $TARGET_DOMAIN's disks live in on $TARGET_HOST, so there is no journal path to look at. Set TARGET_DISK_PATH in $CONF."
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP journal path unresolved"
		return 0
	fi
	log "journal: $jfile on $TARGET_HOST"

	# Read past the file, never truncate it. It is append-only and shared with
	# every other stage in this run, so the assertions below are made against
	# the lines THIS sync added -- and a harness that deleted a journal to
	# make its own test easier to write would be deleting the one thing the
	# feature exists to keep.
	local before
	before="$(ssh_host_cmd "$TARGET_HOST" "wc -l < '$jfile' 2>/dev/null || echo 0" | tr -d '[:space:]')"
	case "$before" in '' | *[!0-9]*) before=0 ;; esac

	bench_sync "$sc" sync
	if [ "$RUN_RC" != 0 ]; then
		warn "FAIL: the ordinary sync this stage is built on did not succeed (see $RUN_LOG)$(bench_sync_hint)"
		results_row "$CSV" "$sc" sync-result 1 "" "" "" "" "" "FAIL the sync failed"
		return 1
	fi

	local added intent outcome aid seq res intent_line outcome_line
	added="$(ssh_host_cmd "$TARGET_HOST" "tail -n +$((before + 1)) '$jfile' 2>/dev/null || true")"

	# awk over the added lines rather than grep per record, so both line
	# numbers come out of one reading of one text and cannot disagree about
	# which record came first -- which is the whole point of the third
	# assertion below.
	intent_line="$(printf '%s\n' "$added" | awk '/"k":"intent"/ && /"verb":"sync"/ { n = NR } END { print n + 0 }')"
	intent=""
	[ "${intent_line:-0}" -gt 0 ] && intent="$(printf '%s\n' "$added" | sed -n "${intent_line}p")"

	if [ -n "$intent" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the sync writes an intent record beside the replica" "$fo_ok" \
		"that run appended nothing with k=intent and verb=sync to $jfile on $TARGET_HOST. A sync that records no intent leaves an interrupted run indistinguishable from one that never started -- see $RUN_LOG"

	if [ -z "$intent" ]; then
		results_row "$CSV" "$sc" journal_dependent_checks "" "" "" "" "" "" "SKIP no intent record to join against"
		return 0
	fi

	aid="$(json_str "$intent" aid)"
	seq="$(printf '%s' "$intent" | grep -o '"seq":[0-9]*' | head -1 | sed 's/.*://')"
	if [ -n "$aid" ] && [ -n "$seq" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the intent carries the (aid,seq) join" "$fo_ok" \
		"aid='$aid' seq='$seq' in: $intent -- the join is the PAIR, never aid alone, because one process can perform two actions under one id and joining on the id would pair one run's intent with the other's outcome"

	# The trailing comma in the seq match is not decoration: "seq":1 is a
	# prefix of "seq":10, and the next key is always "at", so anchoring on the
	# separator is what stops a tenth action's outcome being read as the
	# first's.
	outcome_line="$(printf '%s\n' "$added" \
		| awk -v a="\"aid\":\"$aid\"" -v s="\"seq\":$seq," '/"k":"outcome"/ && index($0, a) && index($0, s) { n = NR } END { print n + 0 }')"
	outcome=""
	[ "${outcome_line:-0}" -gt 0 ] && outcome="$(printf '%s\n' "$added" | sed -n "${outcome_line}p")"
	res="$(json_str "$outcome" res)"

	if [ -n "$outcome" ] && [ "$res" = ok ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the intent has a matching outcome recording success" "$fo_ok" \
		"no outcome with aid=$aid seq=$seq and res=ok among the lines that run added (got res='$res'). An unmatched intent is what the reader reports as a run that died mid-action, so a sync that finished and left one would manufacture the exact finding this journal exists to report"

	if [ "${outcome_line:-0}" -gt "${intent_line:-0}" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the intent is written BEFORE the outcome" "$fo_ok" \
		"intent on line ${intent_line:-0} and outcome on line ${outcome_line:-0} of what that run appended. Order is the mechanism, not a formatting detail: an intent flushed at exit would be removed by precisely the events worth recording"

	local marker
	marker="$(replica_incomplete "$TARGET_URI" "$TARGET_DOMAIN")"
	if [ -z "$marker" ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "a finished sync leaves no replica_incomplete behind" "$fo_ok" \
		"the target still carries replica_incomplete='$marker' after a sync that reported success. It is cleared by the same write that records the success, so one standing here means every -promote of this replica is refused until another sync clears it"

	return 0
}

# Stage 18: two target domains in one directory.
#
# Restore points are keyed by the target DOMAIN, not by the DIRECTORY the
# replica's disks live in. Keying them by the directory would give every domain
# replicating into one -target-disk-path a single shared store, with every
# policy decision taken over the union of their points. Four distinct failures
# follow from that, and this stage proves each is closed:
#
#   1. STARVATION. An interval floor measured against the newest point in the
#      whole directory lets one domain's point satisfy another's floor. With a
#      staggered cron the same domain loses every time: measured at one replica
#      taking zero restore points in seventy-two hours.
#   2. CROSS-EVICTION. A retention count applied to the union spreads a count
#      meant for one machine across several, so a busy domain's churn evicts a
#      quiet one's history.
#   3. STAGING THEFT. A sweep of abandoned ".incomplete-" directories running
#      over the shared directory removes a concurrently running sibling's
#      in-flight set -- failing a sync whose data had already landed, and
#      counting that failure toward -reinit-after-failures.
#   4. CROSS-SWEEP. A -reinit rm -rf'ing the whole shared directory destroys
#      every co-located domain's entire history in one command.
#
# The co-located domain is PLANTED rather than replicated. A second real pair
# would race the first, so a failure could be a race rather than the defect --
# and what has to be proved is only that this domain's sync never reads, prunes
# or deletes directories that are not its own, which a planted set establishes
# deterministically. It also plants the one thing a real second pair could not
# conveniently produce: an in-flight staging directory, which is what sub-test 3
# needs and which a live run would only have for the seconds it was copying.
stage_colocated() {
	log "=== Stage 18: two target domains in one directory ==="
	local sc=colocated
	local fo_ok=0

	if [ "$DRY_RUN" = yes ]; then
		bench_sync "$sc" baseline -reinit "-retention=2,0"
		fo_check "$sc" "the sync keeps this domain's restore points in its own subdirectory" 0
		fo_check "$sc" "a co-located domain's point does not satisfy this domain's interval floor" 0
		fo_check "$sc" "pruning leaves a co-located domain's restore points alone" 0
		fo_check "$sc" "pruning still enforces the count on this domain's own points" 0
		fo_check "$sc" "the staging sweep leaves a co-located domain's in-flight set alone" 0
		fo_check "$sc" "the staging sweep leaves the shared directory alone" 0
		fo_check "$sc" "-reinit removes only this domain's restore points" 0
		return 0
	fi

	if [ -z "${TARGET_DISK_PATH:-}" ]; then
		warn "SKIP stage 18: TARGET_DISK_PATH is not set, so there is no shared directory to put two domains' stores in. Set it in $CONF."
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP TARGET_DISK_PATH unset"
		return 0
	fi
	stage_needs_target_shutoff "$CSV" "$sc" "stage colocated" || return 0
	reset_pair_state "$sc" || return 0
	require_target_syncable "$sc" || return 0

	local root mine theirs
	root="$(rp_root)"
	mine="$(rp_store_dir)"
	# A name no real domain on a bench target will have, so nothing else in
	# this run can be confused with it -- and a legal domain name, so the
	# encoding is exercised rather than sidestepped.
	theirs="$(rp_store_dir bench-colocated-peer)"

	rp_clear_store
	ssh_host_cmd "$TARGET_HOST" "rm -rf '$theirs'" >/dev/null 2>&1 || true

	# Does the target support restore points at all? Same gate the retention
	# stage uses, and for the same reason: without reflink every copy here
	# would be a full one.
	bench_sync "$sc" baseline -reinit "-retention=3,0"
	if [ "$RUN_RC" != 0 ]; then
		if grep -q "does not support reflink copies" "$RUN_LOG" 2>/dev/null; then
			warn "SKIP stage colocated: $TARGET_DISK_PATH on $TARGET_HOST does not support reflink copies (needs XFS with reflink=1, or btrfs)"
			results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP target filesystem has no reflink support"
			return 0
		fi
		warn "FAIL: the baseline sync this stage is built on did not succeed (see $RUN_LOG)$(bench_sync_hint)"
		results_row "$CSV" "$sc" sync-result 1 "" "" "" "" "" "FAIL the baseline sync failed"
		return 1
	fi

	# The layout itself, asserted end to end: vmsync must have created exactly
	# the directory this harness computes. That is what pins the shell copy of
	# the domain-name encoding against the engine's, without a second table of
	# cases to keep in step.
	if ssh_host_cmd "$TARGET_HOST" "test -d '$mine'" >/dev/null 2>&1; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the sync keeps this domain's restore points in its own subdirectory" "$fo_ok" \
		"expected $mine on $TARGET_HOST after a sync with -retention, and it is not there. Either restore points are not being kept per target domain, or this harness and vmsync disagree about how a domain name becomes a directory name -- in which case every count in this stage is reading the wrong directory"
	if [ "$fo_ok" != 0 ]; then
		return 1
	fi

	# --- 1. starvation -------------------------------------------------------
	#
	# THE conditions that make this red against a pre-change binary, and they
	# have to be exactly these:
	#
	#   * this domain's own store is EMPTY, so its own floor cannot have been
	#     satisfied by anything of its own;
	#   * the co-located domain has a point dated NOW;
	#   * the interval is NON-ZERO.
	#
	# A floor measured over the shared directory then finds that fresh foreign
	# point, answers "not due", and takes nothing -- count stays 0. A floor
	# measured over this domain's store finds nothing, answers "due", and takes
	# one -- count becomes 1. An interval of 0 would make every sync due under
	# either rule and the sub-test could not tell them apart, which is what the
	# first version of it got wrong.
	rp_clear_store
	local now_ts
	now_ts="$(ssh_host_cmd "$TARGET_HOST" "date +%s" | tr -d '[:space:]')"
	case "$now_ts" in '' | *[!0-9]*) now_ts=2000000000 ;; esac
	rp_plant_point "$theirs" "${now_ts}-vmsync-cpt-000099" "bench01:bench-colocated-peer"

	local before after
	before="$(rp_count "$mine")"
	bench_sync "$sc" not-starved "-retention=3,6h"
	after="$(rp_count "$mine")"
	if [ "${before:-1}" = 0 ] && [ "${after:-0}" = 1 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "a co-located domain's point does not satisfy this domain's interval floor" "$fo_ok" \
		"$mine went from ${before:-0} to ${after:-0} restore points; it must go from 0 to 1. Its own store was emptied first and $theirs holds a point dated now, so a run that took none measured its six-hour floor against the OTHER domain's history. That is how one replica goes for days taking no restore points while a busier co-located one keeps resetting the clock -- with every sync still reporting success"

	# --- 2. cross-eviction ---------------------------------------------------
	#
	# The foreign store gets three points; this domain's retention keeps one. A
	# prune over the shared directory would see four and delete three.
	rp_plant_point "$theirs" "1700000001-vmsync-cpt-000001" "bench01:bench-colocated-peer"
	rp_plant_point "$theirs" "1700000002-vmsync-cpt-000002" "bench01:bench-colocated-peer"
	rp_plant_point "$theirs" "1700000003-vmsync-cpt-000003" "bench01:bench-colocated-peer"
	local theirs_before theirs_after mine_after
	theirs_before="$(rp_count "$theirs")"
	bench_sync "$sc" no-cross-prune "-retention=1,0"
	theirs_after="$(rp_count "$theirs")"
	if [ "$theirs_before" = "$theirs_after" ] && [ "${theirs_after:-0}" -gt 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "pruning leaves a co-located domain's restore points alone" "$fo_ok" \
		"$theirs held ${theirs_before:-0} restore points before a sync of $TARGET_DOMAIN with -retention=1,0 and ${theirs_after:-0} after. A retention count applied to the union of several domains' points means one machine's churn silently deletes another's recovery history"

	# This domain's own prune must still work, or the assertion above would
	# pass on a build that had simply stopped pruning.
	mine_after="$(rp_count "$mine")"
	if [ "$mine_after" = 1 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "pruning still enforces the count on this domain's own points" "$fo_ok" \
		"expected exactly 1 restore point in $mine under -retention=1,0, found ${mine_after:-0}. Without this the check above would pass on a build that had stopped pruning altogether"

	# --- 3. staging theft ----------------------------------------------------
	#
	# An in-flight set in the foreign store, and one left flat in the shared
	# root by a pre-change run. Neither is this domain's to sweep.
	local peer_staging root_staging
	peer_staging="$theirs/.incomplete-1700000009-vmsync-cpt-000009"
	root_staging="$root/.incomplete-1700000008-vmsync-cpt-000008"
	ssh_host_cmd "$TARGET_HOST" "mkdir -p '$peer_staging' '$root_staging'" >/dev/null
	bench_sync "$sc" no-staging-theft "-retention=2,0"
	if ssh_host_cmd "$TARGET_HOST" "test -d '$peer_staging'" >/dev/null 2>&1; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the staging sweep leaves a co-located domain's in-flight set alone" "$fo_ok" \
		"the staging directory planted in $theirs is gone after a sync of $TARGET_DOMAIN. On a real target that set belongs to a run still copying into it: its own rename then fails, so a sync whose data had already landed is reported as a failure -- and that failure counts toward -reinit-after-failures"
	if ssh_host_cmd "$TARGET_HOST" "test -d '$root_staging'" >/dev/null 2>&1; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "the staging sweep leaves the shared directory alone" "$fo_ok" \
		"the staging directory planted directly in $root is gone. Nothing may sweep the shared directory: whose interrupted run left an entry there cannot be determined from its name, which is the whole reason restore points are now kept per domain"

	# --- 4. cross-sweep (last: it discards this domain's store) --------------
	local reinit_before reinit_after
	reinit_before="$(rp_count "$theirs")"
	bench_sync "$sc" reinit-scope -reinit -replaced-disk-action=delete "-retention=2,0"
	reinit_after="$(rp_count "$theirs")"
	fo_ok=0
	[ "$reinit_after" = "$reinit_before" ] || fo_ok=1
	fo_check "$sc" "-reinit removes only this domain's restore points" "$fo_ok" \
		"a -reinit of $TARGET_DOMAIN with -replaced-disk-action=delete removed restore points that are not its own: $theirs went from ${reinit_before:-0} to ${reinit_after:-0}. This used to be an rm -rf of $root, which destroyed every co-located replica's entire history in one command while logging that it had removed the replaced replica's"

	ssh_host_cmd "$TARGET_HOST" "rm -rf '$theirs' '$root_staging'" >/dev/null 2>&1 || true
	return 0
}

stage_leftovers() {
	log "=== Stage 19: displaced sets are reported, and reclaimed only when asked ==="
	local sc=leftovers
	local fo_ok=0

	if [ "$DRY_RUN" = yes ]; then
		# rename, not the harness's usual delete: this stage is about the files
		# a rename LEAVES, so a dry run printing `delete` would advertise a
		# command line that cannot test what the stage tests. Same reason
		# stage 16 overrides it.
		LO_SAVED_REPLACED_DISK_ACTION="$REPLACED_DISK_ACTION"
		REPLACED_DISK_ACTION=rename
		bench_sync "$sc" baseline
		bench_sync "$sc" rebuild -reinit
		bench_sync "$sc" report
		bench_sync "$sc" under-floor "-reclaim-leftovers-after=1h"
		bench_sync "$sc" too-new "-reclaim-leftovers-after=8760h"
		bench_sync "$sc" reclaim "-reclaim-leftovers-after=24h"
		bench_sync "$sc" incomplete -reinit "-test=$VMSYNC_TEST_DIE_WRITING_BASE"
		bench_sync "$sc" refused "-reclaim-leftovers-after=24h"
		bench_sync "$sc" repaired -reinit
		REPLACED_DISK_ACTION="$LO_SAVED_REPLACED_DISK_ACTION"
		fo_check "$sc" "a rebuild leaves one aside file per disk" 0
		fo_check "$sc" "a run with no duration reports the sets and removes none" 0
		fo_check "$sc" "a duration under the floor is refused at startup" 0
		fo_check "$sc" "a fresh aside is not reclaimed" 0
		fo_check "$sc" "an aside older than the duration is reclaimed" 0
		fo_check "$sc" "a co-located domain's aside is left alone" 0
		fo_check "$sc" "a restore point store moved aside is reclaimed" 0
		fo_check "$sc" "nothing is reclaimed while the replica is marked incomplete" 0
		fo_check "$sc" "the refusal does not fail the sync" 0
		return 0
	fi

	if [ -z "${TARGET_DISK_PATH:-}" ]; then
		warn "SKIP stage 19: TARGET_DISK_PATH is not set in $CONF. Every path this stage inspects is derived from it, and guessing where the replica's disks live is how a sweep test deletes something else."
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP TARGET_DISK_PATH unset"
		return 0
	fi
	stage_needs_target_shutoff "$CSV" "$sc" "stage leftovers" || return 0

	# rename for the whole stage, restored on every exit path below.
	local saved_action="$REPLACED_DISK_ACTION"
	REPLACED_DISK_ACTION=rename
	# shellcheck disable=SC2064
	trap "REPLACED_DISK_ACTION='$saved_action'" RETURN

	local replica="${TARGET_DISK_PATH%/}/${TARGET_DOMAIN}.qcow2"
	# aside_count -> how many aside files sit beside the replica right now.
	aside_count() {
		ssh_host_cmd "$TARGET_HOST" "ls -1d '${replica}'.vmsync-replaced-* 2>/dev/null | wc -l" \
			2>/dev/null | tr -d '[:space:]' || true
	}

	# --- 19a. a rebuild leaves an aside set -----------------------------------
	bench_sync "$sc" baseline || { bench_sync_hint; fo_check "$sc" "a rebuild leaves one aside file per disk" 1 "the baseline sync itself failed, see $RUN_LOG"; return 0; }
	bench_sync "$sc" rebuild -reinit || { fo_check "$sc" "a rebuild leaves one aside file per disk" 1 "the rebuild failed, see $RUN_LOG"; return 0; }

	local n
	n="$(aside_count)"
	if [ "${n:-0}" -ge 1 ]; then
		fo_check "$sc" "a rebuild leaves one aside file per disk" 0
	else
		fo_check "$sc" "a rebuild leaves one aside file per disk" 1 "no ${replica}.vmsync-replaced-* on $TARGET_HOST after a -reinit with -replaced-disk-action=rename; the rest of this stage has nothing to act on"
		return 0
	fi

	# --- 19b. no duration: reported, not removed ------------------------------
	#
	# The reporting half is the one that matters on an estate that has been
	# running for a year: the files are already there, and until a run said so
	# nothing named them at all.
	bench_sync "$sc" report || fo_ok=1
	if [ "$(aside_count)" = "$n" ] && grep -q "displaced sets beside the replica" "$RUN_LOG" 2>/dev/null; then
		fo_check "$sc" "a run with no duration reports the sets and removes none" 0
	else
		fo_check "$sc" "a run with no duration reports the sets and removes none" 1 "expected $n aside files still present and a report line in $RUN_LOG, found $(aside_count)"
	fi

	# --- 19c. under the floor is refused at startup ---------------------------
	#
	# Refused BEFORE anything is touched, and that is the point: "0h" is what
	# somebody writes meaning "off", and a sweep that obeyed it would delete an
	# aside made minutes earlier.
	bench_sync "$sc" under-floor "-reclaim-leftovers-after=1h" || true
	if [ "${RUN_RC:-0}" != 0 ] && [ "$(aside_count)" = "$n" ]; then
		fo_check "$sc" "a duration under the floor is refused at startup" 0
	else
		fo_check "$sc" "a duration under the floor is refused at startup" 1 "the run exited ${RUN_RC:-0} and left $(aside_count) of $n aside files; an under-floor duration must stop the run before it acts"
	fi

	# --- 19d. old enough is the only thing that gets swept -------------------
	#
	# A year's duration against a set made a minute ago, so the age test is the
	# only thing that can keep it.
	bench_sync "$sc" too-new "-reclaim-leftovers-after=8760h" || fo_ok=1
	if [ "$(aside_count)" = "$n" ]; then
		fo_check "$sc" "a fresh aside is not reclaimed" 0
	else
		fo_check "$sc" "a fresh aside is not reclaimed" 1 "$(aside_count) of $n aside files left after a run whose duration was a year"
	fi

	# Age them by rewriting the stamp in the name rather than by touching a
	# clock: the stamp is what vmsync reads, and moving the host's time would
	# invalidate every other timestamp this replica carries.
	local old_stamp=1000000000
	ssh_host_cmd "$TARGET_HOST" "for f in '${replica}'.vmsync-replaced-*; do [ -e \"\$f\" ] || continue; mv -n \"\$f\" \"\${f%.vmsync-replaced-*}.vmsync-replaced-${old_stamp}\"; done" >/dev/null 2>&1 || true

	# A co-located domain's aside, planted beside ours in the same directory.
	# This is the check that a sweep reading the DIRECTORY rather than this
	# domain's disks would fail, and failing it means pointing an operator at
	# another machine's only good copy.
	local theirs="${TARGET_DISK_PATH%/}/not-${TARGET_DOMAIN}.qcow2.vmsync-replaced-${old_stamp}"
	ssh_host_cmd "$TARGET_HOST" "dd if=/dev/zero of='$theirs' bs=1k count=8 2>/dev/null" >/dev/null 2>&1 || true

	bench_sync "$sc" reclaim "-reclaim-leftovers-after=24h" || fo_ok=1
	if [ "$(aside_count)" = 0 ]; then
		fo_check "$sc" "an aside older than the duration is reclaimed" 0
	else
		fo_check "$sc" "an aside older than the duration is reclaimed" 1 "$(aside_count) aside files older than the duration were left behind; see $RUN_LOG"
	fi
	if ssh_host_cmd "$TARGET_HOST" "test -e '$theirs'" >/dev/null 2>&1; then
		fo_check "$sc" "a co-located domain's aside is left alone" 0
	else
		fo_check "$sc" "a co-located domain's aside is left alone" 1 "$theirs was removed although it belongs to another domain -- the sweep is reading the directory instead of this domain's own disks, so its bytes are charged to the wrong machine and the path it offers as reclaimable is another machine's data"
	fi
	ssh_host_cmd "$TARGET_HOST" "rm -f '$theirs'" >/dev/null 2>&1 || true

	# --- 19e. the store a rebuild moved aside --------------------------------
	#
	# The sub-case no listing can reach: these directories sit BESIDE the
	# per-domain stores, so -list-restore-points cannot see them and the points
	# inside them are outside every store.
	local rp_parent="${TARGET_DISK_PATH%/}/.vmsync-rp"
	bench_sync "$sc" store-baseline "-retention=2,0" || fo_ok=1
	bench_sync "$sc" store-rebuild -reinit "-retention=2,0" || fo_ok=1
	local asides
	asides="$(ssh_host_cmd "$TARGET_HOST" "ls -1d '$rp_parent'/.replaced-vm-${TARGET_DOMAIN}-* 2>/dev/null | wc -l" 2>/dev/null | tr -d '[:space:]' || true)"
	if [ "${asides:-0}" -ge 1 ]; then
		# Age it the same way, then sweep.
		ssh_host_cmd "$TARGET_HOST" "for d in '$rp_parent'/.replaced-vm-${TARGET_DOMAIN}-*; do [ -d \"\$d\" ] || continue; mv -n \"\$d\" \"${rp_parent}/.replaced-vm-${TARGET_DOMAIN}-${old_stamp}\"; done" >/dev/null 2>&1 || true
		bench_sync "$sc" store-reclaim "-reclaim-leftovers-after=24h" || fo_ok=1
		local left
		left="$(ssh_host_cmd "$TARGET_HOST" "ls -1d '$rp_parent'/.replaced-vm-${TARGET_DOMAIN}-* 2>/dev/null | wc -l" 2>/dev/null | tr -d '[:space:]' || true)"
		if [ "${left:-1}" = 0 ]; then
			fo_check "$sc" "a restore point store moved aside is reclaimed" 0
		else
			fo_check "$sc" "a restore point store moved aside is reclaimed" 1 "$left aside stores remain under $rp_parent; nothing else in vmsync can name these, so what this sweep leaves is invisible again"
		fi
	else
		# Not a failure of the sweep: this build's -reinit may not have needed
		# to move a store aside (no points existed yet). Recorded rather than
		# passed, because a PASS here would claim the invisible sub-case was
		# tested when nothing was there to test.
		log "   SKIP: -reinit left no aside restore point store to reclaim"
		results_row "$CSV" "$sc" a_restore_point_store_moved_aside_is_reclaimed SKIP "" "" "" "" "" "SKIP no aside store was created"
	fi

	# --- 19f. an incomplete replica is never swept ---------------------------
	#
	# The interlock the feature could not ship without. A fault-injected rebuild
	# leaves replica_incomplete AND a fresh aside set, and that aside set is the
	# complete replica the rebuild replaced -- putting it back is the documented
	# recovery. A sweep that took it would leave a half-written image whose
	# metadata still reads healthy.
	if bench_sync "$sc" incomplete -reinit "-test=$VMSYNC_TEST_DIE_WRITING_BASE"; then
		warn "the injected failure did not take effect: -test=$VMSYNC_TEST_DIE_WRITING_BASE exited 0. Is $VMSYNC_BIN older than the flag?"
	fi
	n="$(aside_count)"
	if [ "${n:-0}" -lt 1 ]; then
		log "   SKIP: the interrupted rebuild left no aside set, so there is nothing for the refusal to protect"
		results_row "$CSV" "$sc" nothing_is_reclaimed_while_the_replica_is_marked_incomplete SKIP "" "" "" "" "" "SKIP no aside set after the injected failure"
	else
		ssh_host_cmd "$TARGET_HOST" "for f in '${replica}'.vmsync-replaced-*; do [ -e \"\$f\" ] || continue; mv -n \"\$f\" \"\${f%.vmsync-replaced-*}.vmsync-replaced-${old_stamp}\"; done" >/dev/null 2>&1 || true
		# The sync is EXPECTED to run here -- re-running it is the documented
		# repair for replica_incomplete -- so what is being tested is that the
		# sweep inside it stood down, not that the run was refused.
		bench_sync "$sc" refused "-reclaim-leftovers-after=24h"
		local rc_refused="${RUN_RC:-0}" log_refused="$RUN_LOG"
		if [ "$(aside_count)" = "$n" ]; then
			fo_check "$sc" "nothing is reclaimed while the replica is marked incomplete" 0
		else
			fo_check "$sc" "nothing is reclaimed while the replica is marked incomplete" 1 "$(aside_count) of $n aside files survived; these ARE the complete replica while replica_incomplete is set, so the sweep has just deleted the recovery it is documented to leave alone"
		fi
		# And the refusal must not be an error: a sweep standing down is a
		# warning on a run whose job is to repair the replica.
		if grep -q "REFUSING to reclaim displaced sets" "$log_refused" 2>/dev/null; then
			fo_check "$sc" "the refusal does not fail the sync" "$rc_refused" "the run that repaired the replica exited $rc_refused; the sweep standing down must be a warning, not a failure"
		else
			fo_check "$sc" "the refusal does not fail the sync" 1 "no refusal line in $log_refused, so it cannot be told from a sweep that simply found nothing"
		fi
	fi

	# Leave the replica complete and clean, whatever happened above: the next
	# stage inherits this target.
	bench_sync "$sc" repaired -reinit || warn "stage 19 could not leave $TARGET_DOMAIN with a complete replica -- the next stage starts from a replica marked incomplete"
	ssh_host_cmd "$TARGET_HOST" "rm -f '${replica}'.vmsync-replaced-* ; rm -rf '$rp_parent'/.replaced-vm-${TARGET_DOMAIN}-*" >/dev/null 2>&1 || true

	unset -f aside_count
	return 0
}

stage_reinit_order() {
	log "=== Stage 20: a reinit refused for a running target must have destroyed nothing ==="
	local sc=reinit-order
	local fo_ok=0

	if [ "$DRY_RUN" = yes ]; then
		# rename, like stages 16 and 19: this stage asserts that NOTHING was
		# displaced, so a dry run printing the harness's usual `delete` would
		# advertise a command line that cannot test what the stage tests.
		RO_SAVED_REPLACED_DISK_ACTION="$REPLACED_DISK_ACTION"
		REPLACED_DISK_ACTION=rename
		bench_sync "$sc" baseline -reinit
		bench_sync "$sc" running-reinit -reinit
		bench_sync "$sc" running-force-clean -force-clean
		bench_sync "$sc" repaired -reinit
		REPLACED_DISK_ACTION="$RO_SAVED_REPLACED_DISK_ACTION"
		fo_check "$sc" "-reinit over a running target is refused" 0
		fo_check "$sc" "the refused -reinit left the source's checkpoint chain intact" 0
		fo_check "$sc" "the refused -reinit did not arm replica_incomplete" 0
		fo_check "$sc" "-force-clean over a running target is refused" 0
		fo_check "$sc" "the refused -force-clean left the source's checkpoint chain intact" 0
		fo_check "$sc" "the refused -force-clean did not arm replica_incomplete" 0
		fo_check "$sc" "the refused -force-clean left the target domain defined" 0
		return 0
	fi

	# This is the ONE stage that deliberately starts a target domain, so it
	# checks it can put things back before it touches anything.
	if ! domain_exists "$TARGET_URI" "$TARGET_DOMAIN"; then
		warn "SKIP stage 20: $TARGET_DOMAIN does not exist on the target${VIRSH_ERR:+: $VIRSH_ERR}. The whole stage is about what a reinit does while the target is RUNNING, and there is nothing to run."
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP target domain absent"
		return 0
	fi
	stage_needs_target_shutoff "$CSV" "$sc" "stage reinit-order" || return 0

	local saved_action="$REPLACED_DISK_ACTION"
	REPLACED_DISK_ACTION=rename
	# shellcheck disable=SC2064
	trap "REPLACED_DISK_ACTION='$saved_action'" RETURN

	# target_is_persistent -> 0 when the target domain still has a definition.
	#
	# Not domain_exists: dominfo succeeds for a TRANSIENT domain too, which is
	# exactly the state a -force-clean that undefined a running domain leaves
	# behind, and telling those apart is the point of sub-test 20f.
	target_is_persistent() {
		virsh_uri "$TARGET_URI" dominfo "$TARGET_DOMAIN" 2>/dev/null \
			| grep -qE '^Persistent:[[:space:]]+yes'
	}

	# A baseline so there IS a checkpoint chain to protect and a replica to boot.
	bench_sync "$sc" baseline -reinit || {
		bench_sync_hint
		fo_check "$sc" "-reinit over a running target is refused" 1 "the baseline sync itself failed, see $RUN_LOG"
		return 0
	}

	local cpts_before
	cpts_before="$(vmsync_checkpoint_count "$SOURCE_URI" "$SOURCE_DOMAIN")"
	if [ "${cpts_before:-0}" -lt 1 ]; then
		# Every assertion below compares against this number. Zero would make
		# them all pass without proving anything, which is worse than skipping.
		warn "SKIP stage 20: $SOURCE_DOMAIN carries no vmsync checkpoints after a baseline sync, so \"the chain survived\" cannot be told from \"there was no chain\"."
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP no source checkpoint to protect"
		return 0
	fi

	# --- boot the replica, PAUSED --------------------------------------------
	#
	# Paused rather than running, and that is the whole reason this stage is
	# safe to ship. libvirtsync.DomainActive is `state != SHUTOFF`, so a paused
	# domain trips the guard exactly like a running one -- while the guest never
	# executes a single instruction, so it cannot write to the replica or start
	# talking on the network as a second copy of a production machine. qemu does
	# open the images, so the qcow2 headers are touched; the stage rewrites the
	# replica at the end for that reason.
	if ! virsh_uri "$TARGET_URI" start "$TARGET_DOMAIN" --paused >/dev/null 2>&1; then
		warn "SKIP stage 20: could not start $TARGET_DOMAIN in paused mode on the target. A replica's definition comes from its source, so it can name a bridge, a CPU model or a host device that does not exist on the DR host -- which is a legitimate state, not a failure. Nothing was changed."
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP target would not start paused"
		return 0
	fi
	log "   started $TARGET_DOMAIN paused on the target; the guest is not executing"

	# From here on the domain MUST be put back, on every path.
	local started_paused=yes
	ro_cleanup() {
		[ "$started_paused" = yes ] || return 0
		started_paused=no
		if ! target_is_persistent; then
			# Only reachable when a sub-test below already failed: destroying a
			# transient domain removes it outright. Said out loud, with the way
			# back, because a silent vanishing target is how a bench run gets
			# blamed on the harness.
			warn "$TARGET_DOMAIN is TRANSIENT -- something undefined it while it was running, which is the defect 20f tests for. Destroying it now removes the domain entirely; the final -reinit below defines it again from $SOURCE_DOMAIN."
		fi
		virsh_uri "$TARGET_URI" destroy "$TARGET_DOMAIN" >/dev/null 2>&1 || true
	}

	# ro_assert_nothing_destroyed VERB PHASE -- the three things a refused run
	# must not have done, checked in the order they would have happened.
	ro_assert_nothing_destroyed() {
		local verb="$1" phase="$2"
		local rc="${RUN_RC:-0}"

		if [ "$rc" != 0 ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "$verb over a running target is refused" "$fo_ok" \
			"the run exited $rc; a target whose qemu holds the disks open must never be reinitialised -- the replica ends up an unlinked inode a running guest is still writing to"

		local cpts_after
		cpts_after="$(vmsync_checkpoint_count "$SOURCE_URI" "$SOURCE_DOMAIN")"
		if [ "${cpts_after:-0}" = "$cpts_before" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the refused $verb left the source's checkpoint chain intact" "$fo_ok" \
			"$SOURCE_DOMAIN went from $cpts_before to ${cpts_after:-?} vmsync checkpoints on a run that REFUSED -- the chain is dropped before the running-target check, so forgetting to shut the replica down costs a full recopy of the whole machine"

		local ri
		ri="$(replica_incomplete "$TARGET_URI" "$TARGET_DOMAIN")"
		if [ -z "$ri" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the refused $verb did not arm replica_incomplete" "$fo_ok" \
			"$TARGET_DOMAIN carries replica_incomplete=$ri after a run that refused and touched nothing; that record refuses every later promotion until a sync clears it"

		# Only -force-clean can undefine, so only it is asked.
		if [ "$verb" = "-force-clean" ]; then
			if target_is_persistent; then fo_ok=0; else fo_ok=1; fi
			fo_check "$sc" "the refused $verb left the target domain defined" "$fo_ok" \
				"$TARGET_DOMAIN is no longer persistent: -force-clean undefined it and only then noticed it was running. DomainExists uses LookupDomainByName, which still finds a RUNNING domain after its definition is gone, so the guard fired one step too late and left the replica as a transient domain with no definition at all"
		fi
		[ -n "$phase" ] || true
	}

	# --- 20a-c. a plain -reinit ----------------------------------------------
	bench_sync "$sc" running-reinit -reinit || true
	ro_assert_nothing_destroyed "-reinit" running-reinit

	# --- 20d-f. and -force-clean, which has one more way to be wrong ---------
	#
	# Separate from the above rather than folded into it: -force-clean reaches
	# the same guard through forceCleanTargetDomain, which runs before it and would
	# undefine the target on the way there if the guard sat any later.
	bench_sync "$sc" running-force-clean -force-clean || true
	ro_assert_nothing_destroyed "-force-clean" running-force-clean

	ro_cleanup

	# Leave the replica clean. qemu opened the images to start the domain, so
	# their headers have been written even though the guest never ran, and the
	# next stage inherits this target.
	bench_sync "$sc" repaired -reinit \
		|| warn "stage 20 could not leave $TARGET_DOMAIN with a freshly written replica; the next stage starts from a replica whose qcow2 headers were touched by a paused boot"

	unset -f target_is_persistent ro_cleanup ro_assert_nothing_destroyed
	return 0
}

stage_lock_lease() {
	log "=== Stage 21: a dead driver must not hold the target's lock ==="
	local sc=lock-lease
	local fo_ok=0

	if [ "$DRY_RUN" = yes ]; then
		bench_sync "$sc" baseline -reinit
		log "   (a background sync would be SIGSTOPped here, then the lock watched)"
		fo_check "$sc" "a leased lock is taken when the helper is there" 0
		fo_check "$sc" "a silent driver still holds the lock before the lease is up" 0
		fo_check "$sc" "the target releases a leased lock once the driver goes silent" 0
		fo_check "$sc" "an unleased lock is still held long after the lease" 0
		fo_check "$sc" "-break-target-lock refuses a lock it cannot attribute" 0
		fo_check "$sc" "the resumed driver refuses to commit after losing its lock" 0
		return 0
	fi

	if [ -z "${TARGET_HOST:-}" ]; then
		warn "SKIP stage 21: TARGET_HOST is not set, and every assertion here reads the lock file on the target over ssh."
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP TARGET_HOST unset"
		return 0
	fi
	stage_needs_target_shutoff "$CSV" "$sc" "stage lock-lease" || return 0

	local lock_path="/run/vmsync-locks/target-${TARGET_DOMAIN}.lock"
	local lease=10
	# A lease far shorter than the 90 s default, so the stage takes seconds
	# rather than minutes. The number under test is the MECHANISM, not the
	# default: whether B's clock releases the lock at all.
	local grace=$((lease * 3))

	# lock_free -> 0 when nothing holds the lock on the target.
	#
	# flock -n on the same path the engine uses. A read of the file would not do:
	# the lock lives on the open file description, so the only way to ask whether
	# it is held is to try to take it.
	lock_free() {
		ssh_host_cmd "$TARGET_HOST" "flock -n 9 9>'$lock_path' -c true" >/dev/null 2>&1
	}

	# start_stalled_sync PATH_TO_HELPER -> sets STALLED_PID, or returns 1.
	#
	# SIGSTOP, not SIGKILL, and that is the whole point of this stage. Killing a
	# process closes its sockets, so the target gets a FIN and releases the lock
	# promptly -- which is the case that always worked. A stopped process holds
	# every socket open and sends nothing, which is exactly what a partitioned or
	# powered-off driver looks like from the target: no heartbeat, and no FIN
	# either. It needs no firewall rule and it is fully reversible.
	start_stalled_sync() {
		local helper="$1"
		vmsync_common_args "${IO_DEPTH:-8}"
		local -a args=("${VMSYNC_ARGS[@]}" -bridge-helper-path "$helper" -remote-lock-lease "${lease}s")
		"$VMSYNC_BIN" "${args[@]}" >"$RUN_DIR/logs/${sc}.stalled.log" 2>&1 &
		STALLED_PID=$!

		# Wait for it to actually hold the lock before stopping it; stopping it
		# earlier would prove nothing about a lock it never took.
		local i=0
		while [ $i -lt 120 ]; do
			if ! lock_free; then
				kill -STOP "$STALLED_PID" 2>/dev/null || return 1
				return 0
			fi
			# It may also have exited already, which is a failure to set up.
			kill -0 "$STALLED_PID" 2>/dev/null || return 1
			sleep 0.5
			i=$((i + 1))
		done
		return 1
	}

	cleanup_stalled() {
		[ -n "${STALLED_PID:-}" ] || return 0
		kill -CONT "$STALLED_PID" 2>/dev/null || true
		kill -9 "$STALLED_PID" 2>/dev/null || true
		wait "$STALLED_PID" 2>/dev/null || true
		STALLED_PID=""
	}

	bench_sync "$sc" baseline -reinit || {
		bench_sync_hint
		fo_check "$sc" "a leased lock is taken when the helper is there" 1 "the baseline sync failed, see $RUN_LOG"
		return 0
	}

	# --- 21a-c. with the helper: the target's own clock frees the lock --------
	local helper
	helper="$(bridge_helper_path)"
	if ! ssh_host_cmd "$TARGET_HOST" "test -x '$helper'" >/dev/null 2>&1; then
		warn "SKIP stage 21's leased half: no executable $helper on $TARGET_HOST, so there is nothing to hold a lease. This is exactly the configuration the unleased half below tests."
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP no bridge helper on target"
	elif ! start_stalled_sync "$helper"; then
		warn "SKIP stage 21's leased half: could not get a sync to hold the lock and then stall it. See $RUN_DIR/logs/${sc}.stalled.log"
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP could not stall a lock-holding sync"
		cleanup_stalled
	else
		if grep -q "holding the target-side run lock under a lease" "$RUN_DIR/logs/${sc}.stalled.log" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "a leased lock is taken when the helper is there" "$fo_ok" \
			"the run did not report a leased lock, so it fell back to the shell and this half tests nothing -- see $RUN_DIR/logs/${sc}.stalled.log"

		# Before the lease is up the lock must still be held: a lock that goes
		# early can be handed to a promotion while its driver is still writing.
		if lock_free; then fo_ok=1; else fo_ok=0; fi
		fo_check "$sc" "a silent driver still holds the lock before the lease is up" "$fo_ok" \
			"the lock was already free within a second of the driver going silent, well inside its ${lease}s lease"

		sleep "$grace"
		if lock_free; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the target releases a leased lock once the driver goes silent" "$fo_ok" \
			"the lock on $TARGET_HOST was still held ${grace}s after the driver stopped beating (lease ${lease}s) -- this is CI-10: -promote and -restore there stay blocked until sshd expires the session"

		# --- 21f. and the driver, on resuming, must not write ----------------
		#
		# The other half of the same defect. The lock is gone and may belong to a
		# promotion by now, so the run that lost it must refuse rather than
		# commit into disks that are no longer its own.
		kill -CONT "$STALLED_PID" 2>/dev/null || true
		local i=0
		while kill -0 "$STALLED_PID" 2>/dev/null && [ $i -lt 120 ]; do sleep 0.5; i=$((i + 1)); done
		if grep -qE "can no longer prove it holds the target-side run lock" "$RUN_DIR/logs/${sc}.stalled.log" 2>/dev/null; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the resumed driver refuses to commit after losing its lock" "$fo_ok" \
			"the run resumed and said nothing about having lost its lock; if a promotion took that lock, committing into those disks corrupts what the promoted guest has written -- see $RUN_DIR/logs/${sc}.stalled.log"
		cleanup_stalled
	fi

	# --- 21d-e. without a usable helper: the lock has no clock --------------
	#
	# Asserted rather than assumed, because the WARNING every such run prints is
	# only worth trusting if the consequence it names is real.
	if ! start_stalled_sync "/nonexistent/vmsync-bridge-helper"; then
		warn "SKIP stage 21's unleased half: could not get a sync to hold the lock and then stall it."
		results_row "$CSV" "$sc" precondition "" "" "" "" "" "" "SKIP could not stall an unleased sync"
		cleanup_stalled
	else
		sleep "$grace"
		if lock_free; then fo_ok=1; else fo_ok=0; fi
		fo_check "$sc" "an unleased lock is still held long after the lease" "$fo_ok" \
			"the lock was released without a helper holding a lease, which means something other than this stage's mechanism freed it and the leased half above proves less than it appears to"

		# And the escape refuses, because the shell lock records no holder. The
		# refusal is the designed answer, not a gap: breaking a lock that cannot
		# be attributed is how two writers end up on one replica.
		local out="" rc=0
		if [ -z "${TARGET_VMSYNC_BIN:-}" ]; then
			log "   SKIP: TARGET_VMSYNC_BIN is not set, so -break-target-lock cannot be run on $TARGET_HOST"
			results_row "$CSV" "$sc" break_target_lock_refuses_a_lock_it_cannot_attribute SKIP "" "" "" "" "" "SKIP TARGET_VMSYNC_BIN unset"
			rc=-1
		else
			out="$(ssh_host_cmd "$TARGET_HOST" "'$TARGET_VMSYNC_BIN' -break-target-lock -target-uri qemu:///system -target-domain '$TARGET_DOMAIN'" 2>&1)" && rc=0 || rc=$?
		fi
		if [ "$rc" = -1 ]; then
			: # already recorded as a skip above
		elif [ "$rc" != 0 ] && printf '%s' "$out" | grep -q "records no holder"; then
			fo_check "$sc" "-break-target-lock refuses a lock it cannot attribute" 0
		else
			fo_check "$sc" "-break-target-lock refuses a lock it cannot attribute" 1 \
				"it exited $rc saying: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200). An unleased lock records no holder, so nothing can prove its holder is gone and breaking it must be refused"
		fi
		cleanup_stalled
	fi

	# The stalled runs were killed mid-copy, so leave a complete replica behind.
	bench_sync "$sc" repaired -reinit \
		|| warn "stage 21 could not leave $TARGET_DOMAIN with a complete replica; the next stage starts from a replica a killed sync was part way through"

	unset -f lock_free start_stalled_sync cleanup_stalled
	return 0
}

stage_redefine_probe() {
	log "=== Stage 22: PROBE -- can libvirt remove an orphaned bitmap on a LIVE domain? ==="
	local sc=redefine-probe
	local fo_ok=0

	# A PROBE, not a test of vmsync. vmsync does not implement any of this yet;
	# the stage asks libvirt and qemu directly whether it CAN be implemented, so
	# the answer decides a design rather than grading one. Every check therefore
	# reports what happened even when it is not what was hoped for.
	#
	# The question: -reinit cannot clear a vmsync-cpt-* bitmap that libvirt has no
	# checkpoint for while the source is running, because qemu holds the image and
	# qemu-img cannot write it. The proposed way out is to ADOPT the orphan --
	# redefine checkpoint metadata naming the existing bitmap, then delete that
	# checkpoint normally so qemu removes the bitmap through its own path. Whether
	# libvirt accepts a redefine whose XML vmsync built by hand, and whether the
	# delete then actually takes the bitmap away, is unverifiable without hardware.
	#
	# Every step prints what libvirt and qemu-img said before any verdict is
	# derived from it, and a check whose evidence is unreadable on this host is
	# recorded SKIP rather than passed. The first run of this stage reported only
	# "the fixture did not appear", which named neither of the two very different
	# causes; the discriminator below ("visible in the live image") is what tells
	# them apart, and is a finding in its own right -- vmsync's orphan audit
	# reads bitmaps the same way, so a no there means the audit is blind exactly
	# where it is needed.

	if [ "$DRY_RUN" = yes ]; then
		log "   (would plant an orphan bitmap on $SOURCE_DOMAIN and try to adopt it)"
		fo_check "$sc" "qemu-img cannot write a live image" 0
		fo_check "$sc" "qemu-img reports the bitmap of a checkpoint libvirt lists" 0
		fo_check "$sc" "a checkpoint's bitmap is visible in the live image" 0
		fo_check "$sc" "a metadata-only delete leaves the bitmap behind" 0
		fo_check "$sc" "redefine from libvirt's own dumped xml is accepted" 0
		fo_check "$sc" "redefine from a hand-built xml is accepted" 0
		fo_check "$sc" "deleting the adopted checkpoint removes the bitmap" 0
		fo_check "$sc" "the same works while the domain is paused" 0
		return 0
	fi

	if [ -z "${SOURCE_HOST:-}" ] || [ -z "${TAMPER_DISK_DEV:-}" ]; then
		warn "SKIP stage 22: needs SOURCE_HOST and TAMPER_DISK_DEV in $CONF -- it inspects the source's own image file."
		results_row "$CSV" "$sc" stood-down "" "" "" "" "" "" "SKIP SOURCE_HOST/TAMPER_DISK_DEV unset"
		return 0
	fi
	if [ "$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN")" != "running" ]; then
		warn "SKIP stage 22: $SOURCE_DOMAIN is not running, and the whole question is what can be done while qemu holds the image. Start it and re-run."
		results_row "$CSV" "$sc" stood-down "" "" "" "" "" "" "SKIP source not running"
		return 0
	fi

	local active
	active="$(disk_source_path "$SOURCE_URI" "$SOURCE_DOMAIN" "$TAMPER_DISK_DEV")" || true
	if [ -z "$active" ]; then
		warn "SKIP stage 22: could not resolve $SOURCE_DOMAIN's active path for dev='$TAMPER_DISK_DEV'."
		results_row "$CSV" "$sc" stood-down "" "" "" "" "" "" "SKIP source disk path unresolved"
		return 0
	fi
	log "   probing against $SOURCE_DOMAIN dev=$TAMPER_DISK_DEV path=$active on $SOURCE_HOST"

	# A name that satisfies vmsync's own IsManagedCheckpointName prefix test, so
	# the probe exercises the same classification the engine would -- but far
	# outside the range a real chain counts into, so it cannot be mistaken for
	# one and is obvious in any leftover state.
	local probe=vmsync-cpt-099001

	# bitmap_present -> 0 when the active image carries $1.
	#
	# -U on the read, which is the documented way to inspect an image another
	# process has open. It is NOT used on any write below: whether a write is
	# refused is the first thing being probed.
	# bitmap_json prints the image's info once, whitespace stripped, so every
	# reader below works off the same text and they cannot disagree about what
	# qemu-img said.
	bitmap_json() {
		run_shell_on "$SOURCE_HOST" "$SOURCE_LOCAL" \
			"qemu-img info -U --output=json -f qcow2 '$active' 2>&1" 2>&1 | tr -d ' \n'
	}

	# names_in extracts the bitmap names from the json on stdin.
	#
	# Two traps, both of which have bitten this stage on hardware.
	#
	# Scoped to the bitmaps array, because qemu-img also names the protocol
	# child ({"name": "file"}): grepping the whole document reported a bitmap
	# called "file" that does not exist.
	#
	# And "flags":[...] is deleted FIRST, because it lives inside each bitmap
	# object and the [^]]* below stops at the first closing bracket it meets --
	# the flags array's, not the bitmaps array's. Every in-use bitmap carries
	# flags, so this failed on exactly the bitmaps that exist: the 2026-10-04
	# run read "no bitmap of that name" for two qemu-img had listed, and failed
	# a check over it.
	names_in() {
		sed 's/"flags":\[[^]]*\]//g' \
			| sed -n 's/.*"bitmaps":\[\([^]]*\)\].*/\1/p' \
			| grep -oE '"name":"[^"]*"' | sed 's/"name":"//; s/"$//'
	}
	bitmap_names() {
		bitmap_json | names_in
	}
	bitmap_present() {
		bitmap_names | grep -qx "$1"
	}
	cp_listed() {
		virsh_uri "$SOURCE_URI" checkpoint-list "$SOURCE_DOMAIN" --name 2>/dev/null | grep -qx "$1"
	}

	# cp_covers_disk NAME -> 0 when checkpoint NAME declares a bitmap for
	# $TAMPER_DISK_DEV.
	#
	# Checked rather than assumed, because a checkpoint covering other disks and
	# not this one has no bitmap here to find -- and reading that as "libvirt and
	# qemu-img disagree" would invent a finding out of a disk the checkpoint
	# never claimed. Elements are split onto their own lines so both attributes
	# must appear on the SAME disk, not merely somewhere in the document.
	cp_covers_disk() {
		virsh_uri "$SOURCE_URI" checkpoint-dumpxml "$SOURCE_DOMAIN" "$1" 2>/dev/null \
			| tr -d '\n' | tr '<' '\n' | grep '^disk ' \
			| grep -E "name=['\"]$TAMPER_DISK_DEV['\"]" \
			| grep -qE "checkpoint=['\"]bitmap['\"]"
	}

	# raw_bitmaps prints every bitmap name qemu-img reports, or says plainly that
	# it reported none, so every verdict below has its evidence beside it in the
	# log. A probe that only says PASS or FAIL cannot be acted on when the answer
	# is surprising, and the answers here are expected to be surprising.
	raw_bitmaps() {
		local j n
		j="$(bitmap_json)"
		case "$j" in
		*'"bitmaps"'*)
			n="$(printf '%s' "$j" | names_in | tr '\n' ' ')"
			if [ -n "$n" ]; then
				printf '%s' "$n"
			else
				# An array that parsed to nothing is a parser fault, not an
				# empty image, and saying so is the difference between noticing
				# that and not: this line printed EMPTY for two bitmaps that
				# were there, and an empty evidence line reads like good news.
				printf 'UNPARSED bitmaps array, which is a fault in this stage rather than an answer: %s' \
					"$(printf '%s' "$j" | sed -n 's/.*\("bitmaps":.\{0,200\}\).*/\1/p')"
			fi
			;;
		*'qemu-img:'*)
			# Said apart from "no bitmaps", because they are different answers
			# and bitmap_present cannot tell them apart -- it discards stderr
			# and the exit status, so a read that never ran looks exactly like a
			# clean read of an image with nothing in it.
			printf 'READ FAILED: %s' "$(printf '%s' "$j" | cut -c1-200)"
			;;
		*)
			# Summarised rather than truncated. A modern qemu-img nests the
			# protocol child first, so the first few hundred characters are all
			# file-layer noise -- the hardware run printed exactly that and said
			# nothing about the qcow2 layer. What matters is which format layers
			# it reported and that no bitmaps key appears in any of them.
			printf 'NO bitmaps key in %d bytes of json; format-specific layers: %s' \
				"${#j}" \
				"$(printf '%s' "$j" | grep -oE '"format-specific":\{"type":"[a-z0-9]+"' \
					| sed 's/.*"type":"//; s/"$//' | sort -u | tr '\n' ' ')"
			;;
		esac
	}

	# The fixture xml, written once here rather than in P2, because the live
	# existence oracle below needs it too and is defined alongside it.
	local xml="$RUN_DIR/${sc}.checkpoint.xml"
	cat > "$xml" <<XML
<domaincheckpoint>
  <name>$probe</name>
  <disks>
    <disk name='$TAMPER_DISK_DEV' checkpoint='bitmap'/>
  </disks>
</domaincheckpoint>
XML

	# qemu_holds_probe -> 0 when qemu refuses to create $probe because it
	# already has a bitmap of that name, 1 when the create is accepted (so it
	# did not have one), 2 when the answer is neither.
	#
	# The existence check that asks the component which actually knows, with the
	# domain still running. The two alternatives are both compromised: qemu-img
	# reads the file, which can lag what qemu holds, and stopping the domain
	# changes the very thing being measured, because closing the image is when a
	# persistent bitmap gets written into it. qemu refusing a create for "Bitmap
	# already exists" is qemu's own answer about its own list, taken live.
	#
	# Not read-only: on 1 it has made a checkpoint, which it then deletes the
	# normal way -- the same delete the adopt route under test relies on. A
	# failure to undo returns 2 and says so loudly, because it would mean the
	# probe has just manufactured a fresh orphan.
	qemu_holds_probe() {
		local o r
		o="$(virsh_uri "$SOURCE_URI" checkpoint-create "$SOURCE_DOMAIN" --xmlfile "$xml" 2>&1)" && r=0 || r=$?
		case "$o" in
		*"Bitmap already exists"*) return 0 ;;
		esac
		if [ "$r" != 0 ]; then
			log "     existence probe inconclusive: checkpoint-create exited $r: $(printf '%s' "$o" | tr '\n' ' ' | cut -c1-160)"
			return 2
		fi
		if virsh_uri "$SOURCE_URI" checkpoint-delete "$SOURCE_DOMAIN" --checkpointname "$probe" >/dev/null 2>&1; then
			return 1
		fi
		warn "the existence probe created checkpoint $probe on $SOURCE_DOMAIN and could not delete it again -- that is a NEW orphan, and the stage made it. Clear it as described below."
		return 2
	}

	# settle_stopped NAME -> 0 when NAME is in the image with the domain shut
	# off, 1 when it genuinely is not there, 2 when the question was not asked
	# (the operator has not allowed stopping this domain, or it would not stop).
	#
	# The last resort on a host where qemu-img reports no bitmaps at all while
	# qemu holds the image: with the domain down the file is readable, so
	# "invisible or absent" has an answer. It stops and restarts a source, which
	# is a real intervention, so it happens only on PROBE_MAY_STOP_SOURCE=yes --
	# and then twice, once to confirm the orphan and once to confirm it went.
	settle_stopped() {
		if [ "${PROBE_MAY_STOP_SOURCE:-no}" != yes ]; then
			return 2
		fi
		log "   PROBE_MAY_STOP_SOURCE=yes: shutting $SOURCE_DOMAIN down to read $active directly"
		# graceful_shutdown rather than a shutdown and a poll: libvirt does not
		# retry an ACPI request a guest was not ready for, and a hand-rolled
		# poll therefore watches a healthy VM for the whole timeout. It reports
		# shutoff, not "shut off" -- dom_state strips whitespace, so comparing
		# against the spelling virsh prints never matches.
		local found=1 state
		if graceful_shutdown "$SOURCE_URI" "$SOURCE_DOMAIN" 180 "source $SOURCE_DOMAIN"; then
			log "     qemu-img bitmaps with the domain SHUT OFF: $(raw_bitmaps)"
			bitmap_present "$1" && found=0
		else
			warn "$SOURCE_DOMAIN did not shut down within 180s (${SHUTDOWN_ATTEMPTS:-?} request(s) sent), so $active could not be read directly"
			found=2
		fi
		# Attempted whatever the shutdown did, because this function asked a
		# running source to stop and owns getting it back. Starting a domain
		# that is already up fails harmlessly; leaving a source down does not.
		state="$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN" 2>/dev/null || true)"
		if [ "$state" != running ]; then
			virsh_uri "$SOURCE_URI" start "$SOURCE_DOMAIN" >/dev/null 2>&1 || \
				warn "could not restart $SOURCE_DOMAIN (state=$state) -- start it by hand ('virsh -c $SOURCE_URI start $SOURCE_DOMAIN')"
		fi
		return "$found"
	}

	# Evidence before anything is decided or changed, so a surprising verdict has
	# its grounds beside it in the log.
	local pre_list
	pre_list="$(virsh_uri "$SOURCE_URI" checkpoint-list "$SOURCE_DOMAIN" --name 2>/dev/null | grep -v '^$' | tr '\n' ' ' || true)"
	log "   EVIDENCE before planting anything:"
	log "     virsh checkpoint-list --name: ${pre_list:-(none)}"
	log "     qemu-img bitmaps in the LIVE image: $(raw_bitmaps)"

	# The cleanest measurement in the stage, and it needs no fixture at all.
	#
	# A checkpoint libvirt lists HAS a bitmap -- that is what a checkpoint is on
	# disk -- so when libvirt names one and qemu-img reports no bitmap of that
	# name, the two disagree about a bitmap nobody has tampered with.
	#
	# Both answers have been seen on the same host. 2026-10-02: libvirt listed
	# vmsync-cpt-000001, created an hour and a half earlier, and qemu-img
	# reported no bitmaps at all. 2026-10-04, after that domain had been
	# restarted: qemu-img reported it, flagged in-use. So the disagreement is
	# not permanent blindness, it is a bitmap that has not reached the file yet,
	# and the run that measures it has to say which of the two it saw.
	if [ -n "$pre_list" ]; then
		log "   note: $SOURCE_DOMAIN already carries a vmsync checkpoint chain (${pre_list}), so this stage is probing alongside live replication state"
		local listed missing= covered=
		for listed in $pre_list; do
			cp_covers_disk "$listed" || continue
			covered="${covered}${listed} "
			bitmap_present "$listed" || missing="${missing}${listed} "
		done
		if [ -z "$covered" ]; then
			log "   no listed checkpoint declares a bitmap for $TAMPER_DISK_DEV, so there is nothing to cross-check on this disk"
			results_row "$CSV" "$sc" "qemu-img_reports_the_bitmap_of_a_checkpoint_libvirt_lists" SKIP "" "" "" "" "" "SKIP no listed checkpoint covers this disk"
		elif [ -z "$missing" ]; then
			fo_check "$sc" "qemu-img reports the bitmap of a checkpoint libvirt lists" 0
		else
			fo_check "$sc" "qemu-img reports the bitmap of a checkpoint libvirt lists" 1 \
				"libvirt lists checkpoint(s) ${missing}covering $TAMPER_DISK_DEV on $SOURCE_DOMAIN and qemu-img reports no bitmap of those names in $active. A checkpoint IS a bitmap, so these are not orphans and nothing has tampered with them: their bitmaps have not reached the image yet, and a restart of this domain is what puts them there. That is the read vmsync's orphan audit is built on (disk.BitmapNames over qemu-img info --force-share, cmd/vmsync/bitmaps.go), so until this domain restarts that audit cannot see an orphan made during this qemu's life -- which is the orphan a failed run just made"
		fi
	fi

	# probe_reset makes the starting point rather than hoping for one. Returns 0
	# when the domain is left with no checkpoints and no vmsync-named bitmaps.
	#
	# The stage used to stand down on whatever it found, which made it useless
	# exactly when it was needed most: its own previous run leaves an orphan
	# behind, so every run after the first refused to start. A probe that cannot
	# establish its own preconditions is not a probe, and "remove it by hand
	# first" is the stage asking the operator to do its job.
	#
	# DESTRUCTIVE, deliberately: it removes the pair's whole checkpoint chain,
	# so the next sync for this pair is a FULL one. That is the price of a known
	# starting point, and this stage is documented for disposable test pairs.
	# Removing a leftover BITMAP needs the domain stopped, so that half stays
	# behind PROBE_MAY_STOP_SOURCE and the stage stands down without it rather
	# than starting from a state it cannot describe.
	# sweep_offline NAMES -> 0 once every name is gone from the image, verified
	# with the domain SHUT OFF and the domain running again afterwards.
	#
	# The shutdown is load-bearing twice over: it releases the write lock
	# qemu-img needs, and it is also when a bitmap qemu was holding but had not
	# written reaches the image at all -- so a name invisible before the stop
	# becomes both visible and removable after it.
	#
	# The verification read happens while the domain is still off, because that
	# is the only read in this stage nothing else holds the image during. The
	# domain is started again on every path out, including the failing ones.
	sweep_offline() {
		local names="$1" b out rc i left
		if [ "${PROBE_MAY_STOP_SOURCE:-no}" != yes ]; then
			warn "SKIP stage 22: ${names}must come out of $active and qemu holds the image, so nothing can remove them while $SOURCE_DOMAIN runs. Re-run with PROBE_MAY_STOP_SOURCE=yes to have this stage stop the domain and sweep them, or do it by hand: 'virsh -c $SOURCE_URI shutdown $SOURCE_DOMAIN', then 'qemu-img bitmap --remove -f qcow2 $active <name>' for each of ${names}then start it again."
			return 1
		fi
		log "   PROBE_MAY_STOP_SOURCE=yes: stopping $SOURCE_DOMAIN to sweep $names"
		if ! graceful_shutdown "$SOURCE_URI" "$SOURCE_DOMAIN" 180 "source $SOURCE_DOMAIN"; then
			warn "SKIP stage 22: $SOURCE_DOMAIN did not shut down within 180s, so ${names}could not be removed"
			virsh_uri "$SOURCE_URI" start "$SOURCE_DOMAIN" >/dev/null 2>&1 || true
			return 1
		fi
		log "     bitmaps with the domain SHUT OFF, before the sweep: $(raw_bitmaps)"
		for b in $names; do
			out="$(run_shell_on "$SOURCE_HOST" "$SOURCE_LOCAL" "qemu-img bitmap --remove -f qcow2 '$active' '$b' 2>&1" 2>&1)" && rc=0 || rc=$?
			[ "$rc" = 0 ] || warn "   could not remove bitmap $b from $active: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
		done
		log "     bitmaps with the domain SHUT OFF, after the sweep: $(raw_bitmaps)"
		left="$(bitmap_names | grep '^vmsync-cpt' | tr '\n' ' ' || true)"

		virsh_uri "$SOURCE_URI" start "$SOURCE_DOMAIN" >/dev/null 2>&1 || true
		i=0
		while [ "$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN" 2>/dev/null || true)" != running ] && [ "$i" -lt 120 ]; do
			sleep 1
			i=$((i + 1))
		done
		if [ "$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN" 2>/dev/null || true)" != running ]; then
			warn "SKIP stage 22: $SOURCE_DOMAIN did not come back up after the sweep -- start it by hand ('virsh -c $SOURCE_URI start $SOURCE_DOMAIN'). The whole question this stage asks is what can be done while qemu holds the image, so there is nothing to ask of a stopped domain."
			return 1
		fi
		if [ -n "$left" ]; then
			warn "SKIP stage 22: the sweep did not clear ${left}from $active, read with $SOURCE_DOMAIN shut off. Remove them by hand ('qemu-img bitmap --remove -f qcow2 $active <name>') before re-running."
			return 1
		fi
		return 0
	}

	probe_reset() {
		local cps c left
		cps="$(virsh_uri "$SOURCE_URI" checkpoint-list "$SOURCE_DOMAIN" --name 2>/dev/null | grep -v '^$' | tr '\n' ' ' || true)"
		if [ -n "$cps" ]; then
			warn "stage 22 is clearing $SOURCE_DOMAIN's checkpoints to start from a known state: ${cps}-- the next sync for this pair will be a FULL one"
			for c in $cps; do
				case "$c" in
				vmsync-cpt*) ;;
				*) warn "   $c is not a vmsync checkpoint and this stage is removing it anyway" ;;
				esac
				# Normal delete first, so qemu takes the bitmap with it. The
				# --metadata fallback leaves an orphan, which is what the sweep
				# below is for.
				if ! virsh_uri "$SOURCE_URI" checkpoint-delete "$SOURCE_DOMAIN" --checkpointname "$c" >/dev/null 2>&1; then
					log "   normal delete of $c was refused; dropping its metadata and leaving its bitmap to the sweep"
					virsh_uri "$SOURCE_URI" checkpoint-delete "$SOURCE_DOMAIN" --checkpointname "$c" --metadata >/dev/null 2>&1 || true
				fi
			done
		fi

		left="$(bitmap_names | grep '^vmsync-cpt' | tr '\n' ' ' || true)"
		if [ -n "$left" ]; then
			log "   vmsync bitmaps still in $active after clearing the checkpoints: $left"
			sweep_offline "$left" || return 1
		fi

		# And then ask qemu, because the read above cannot answer on its own.
		#
		# A bitmap made during this qemu's life need not be in the image yet, so
		# a leftover can be invisible to the file read and the reset would
		# announce a clean start it has not got -- which is precisely how two
		# hardware runs went: "clean", then checkpoint-create refused for
		# "Bitmap already exists". qemu's own answer is what decides whether the
		# probe name is free. At most one sweep, then it is asked again: a name
		# still held after a sweep is not a starting point to build on.
		local attempt=0 held
		while :; do
			qemu_holds_probe && held=0 || held=$?
			case "$held" in
			1) break ;;
			0)
				if [ "$attempt" != 0 ]; then
					warn "SKIP stage 22: qemu still holds a bitmap named $probe after the sweep, so this run has no clean starting point"
					return 1
				fi
				attempt=1
				log "   qemu holds a bitmap named $probe that $active does not show; stopping the domain is what puts it in the image, which is what makes it removable"
				sweep_offline "$probe " || return 1
				;;
			*)
				warn "SKIP stage 22: could not establish whether qemu holds a bitmap named $probe, so this run has no known starting point"
				return 1
				;;
			esac
		done

		# Asserted, not assumed. Everything below reads "there was nothing of
		# this name beforehand" into its verdicts, so a reset that half worked
		# would turn each of them into a guess.
		cps="$(virsh_uri "$SOURCE_URI" checkpoint-list "$SOURCE_DOMAIN" --name 2>/dev/null | grep -v '^$' | tr '\n' ' ' || true)"
		left="$(bitmap_names | grep '^vmsync-cpt' | tr '\n' ' ' || true)"
		if [ -z "$cps" ] && [ -n "$left" ]; then
			# The oracle above asks qemu whether the name is free by creating a
			# checkpoint and deleting it again. On a host where a normal delete
			# does NOT take the bitmap with it, that answer costs a bitmap --
			# the check leaves behind exactly the thing it was checking for. One
			# sweep, then the assertion stands or the stage does not run.
			log "   a bitmap is in $active that no checkpoint accounts for: $left -- the free-name check leaves one on a host whose deletes do not remove them"
			sweep_offline "$left" || return 1
			left="$(bitmap_names | grep '^vmsync-cpt' | tr '\n' ' ' || true)"
		fi
		if [ -n "$cps" ] || [ -n "$left" ]; then
			warn "SKIP stage 22: could not reach a clean starting point on $SOURCE_DOMAIN -- checkpoints: ${cps:-none}, vmsync bitmaps: ${left:-none}"
			return 1
		fi
		log "   clean starting point: no checkpoints on $SOURCE_DOMAIN, no vmsync bitmaps in $active, and qemu accepts the name $probe"
		return 0
	}

	if ! probe_reset; then
		results_row "$CSV" "$sc" stood-down "" "" "" "" "" "" "SKIP no clean starting point"
		return 0
	fi

	probe_cleanup() {
		virsh_uri "$SOURCE_URI" checkpoint-delete "$SOURCE_DOMAIN" --checkpointname "$probe" >/dev/null 2>&1 \
			|| virsh_uri "$SOURCE_URI" checkpoint-delete "$SOURCE_DOMAIN" --checkpointname "$probe" --metadata >/dev/null 2>&1 || true
		if bitmap_present "$probe"; then
			# Said loudly, because this is the state the whole feature exists to
			# prevent and the stage has just manufactured one.
			warn "stage 22 LEFT $probe IN $active on $SOURCE_HOST. Nothing here can remove it while the domain runs -- that is the finding. To clear it: shut $SOURCE_DOMAIN down, then 'qemu-img bitmap --remove -f qcow2 $active $probe'. Until then a -reinit of this pair will refuse, naming this bitmap."
			results_row "$CSV" "$sc" cleanup 1 "" "" "" "" "" "FAIL left $probe behind"
		elif [ "${visible_live:-}" = no ]; then
			# The image cannot answer for a bitmap made during this qemu's
			# life, so qemu is asked instead. This used to be a bare "cannot
			# confirm", which was the honest answer only while the stage had no
			# way to ask -- and it left the operator to check by hand after
			# every run, including the ones that cleaned up perfectly.
			local held
			qemu_holds_probe && held=0 || held=$?
			case "$held" in
			1) ;;
			0)
				warn "stage 22 LEFT $probe ON $SOURCE_DOMAIN: qemu still holds that bitmap, though $active does not show it. Nothing can remove it while the domain runs -- that is the finding. To clear it: shut $SOURCE_DOMAIN down (which is also what puts it in the image), then 'qemu-img bitmap --remove -f qcow2 $active $probe'."
				results_row "$CSV" "$sc" cleanup 1 "" "" "" "" "" "FAIL left $probe behind"
				;;
			*)
				warn "stage 22 could not establish whether it left $probe behind on $SOURCE_DOMAIN: neither $active nor qemu gave a usable answer. Check with the domain shut off ('qemu-img info --output=json -f qcow2 $active')."
				results_row "$CSV" "$sc" cleanup SKIP "" "" "" "" "" "SKIP cleanup unverifiable"
				;;
			esac
		fi
	}

	# --- P1. can qemu-img write a live image at all? ------------------------
	#
	# The constraint the design is built on. If this SUCCEEDS the simplest fix is
	# available after all and the rest of the probe is moot, so it is worth
	# knowing either way.
	local out rc
	out="$(run_shell_on "$SOURCE_HOST" "$SOURCE_LOCAL" "qemu-img bitmap --add -f qcow2 '$active' vmsync-probe-writelock 2>&1" 2>&1)" && rc=0 || rc=$?
	if [ "$rc" != 0 ]; then
		fo_check "$sc" "qemu-img cannot write a live image" 0
		log "   qemu-img refused as expected: $(printf '%s' "$out" | head -1 | cut -c1-120)"
	else
		fo_check "$sc" "qemu-img cannot write a live image" 1 \
			"qemu-img ADDED a bitmap to an image a running qemu holds open. That contradicts the premise of the whole design -- it means the image lock is not being taken, which is itself worth investigating before relying on either route"
		run_shell_on "$SOURCE_HOST" "$SOURCE_LOCAL" "qemu-img bitmap --remove -f qcow2 '$active' vmsync-probe-writelock" >/dev/null 2>&1 || \
			warn "could not remove the probe bitmap vmsync-probe-writelock from $active -- remove it by hand"
	fi

	# --- P2. manufacture an orphan the way reality does ---------------------
	#
	# create, dump the xml, then delete METADATA_ONLY. That is precisely the
	# sequence vmsync's own DeleteCheckpointIfExists documents as the orphan
	# factory, so the fixture is the real defect rather than an approximation.
	#
	# Evidence first, verdict second. The interesting failure here is not "the
	# fixture did not appear" but WHICH of two very different things happened:
	# libvirt created no bitmap at all, or it created one that qemu-img cannot
	# see while qemu holds the image. The second would mean vmsync's own orphan
	# audit is blind on a running source, since it reads bitmaps exactly this
	# way -- a bigger finding than the one this stage set out to test.
	#
	# The fixture xml was written with the helpers above, alongside the oracle
	# that shares it.
	# visible_live, orphan_proven and inherited carry what is known about the
	# FIXTURE down to P4, which cannot interpret its own reading without them: on
	# a host where the bitmap is unreadable while live, "not there now" means the
	# delete worked only if something was there to begin with. Without that the
	# stage reports the adopt route working on a host that never made a bitmap.
	local visible_live=no orphan_proven=unknown inherited=no settled=
	local dumped="$RUN_DIR/${sc}.dumped.xml"
	: > "$dumped"

	out="$(virsh_uri "$SOURCE_URI" checkpoint-create "$SOURCE_DOMAIN" --xmlfile "$xml" 2>&1)" && rc=0 || rc=$?

	# Matched on qemu's own words, not libvirt's. virsh_uri runs under LC_ALL=C,
	# but that only localises the client: libvirtd formats its errors server-side
	# in the daemon's locale, so on a French host this arrives as "erreur interne
	# : Impossible d'executer la commande QEMU 'transaction' : Bitmap already
	# exists: ...". The qemu substring comes through untranslated because libvirt
	# appends it verbatim, which is why the test is on that half.
	case "$out" in
	*"Bitmap already exists"*)
		# qemu has just said a bitmap of this name exists, while the evidence
		# printed above shows qemu-img reporting none and libvirt listing no
		# checkpoint for it. That is not a capability problem and not a reason to
		# stand down: it IS the fixture, already present, left by an earlier run
		# that could not clear it. It also settles the discriminator from the
		# error text alone, without stopping the domain -- qemu's refusal is
		# proof the bitmap exists, and the evidence above is proof qemu-img
		# cannot see it.
		inherited=yes
		# qemu, not yes. The refusal proves qemu holds SOME bitmap of that name
		# -- its lookup does not filter by persistence -- so it does not by
		# itself prove a persistent bitmap is in the image. What follows may
		# rely on qemu's own view, which is what this establishes, and must not
		# claim anything about the file.
		orphan_proven=qemu
		# Reaching this at all is now a finding in its own right: probe_reset
		# asked qemu for this exact name and was told it was free. A refusal
		# here means qemu accepted a create of it, accepted the delete, and
		# then refused the next create of the same name -- so the stage carries
		# on with the orphan it has rather than pretending the reset held.
		warn "qemu reported $probe free during the reset and has now refused it as already existing; the reset's own check does not hold on this host"
		fo_check "$sc" "a checkpoint's bitmap is visible in the live image" 1 \
			"qemu refused to create $probe because it already holds a bitmap of that name, while qemu-img reported none and libvirt listed no checkpoint for it. The read vmsync's own audit uses (disk.BitmapNames over qemu-img info --force-share, from cmd/vmsync/bitmaps.go) therefore does not see what qemu will refuse the next sync over. Whether the bitmap is in the FILE is a separate question this does not answer -- qemu's lookup does not filter by persistence"
		log "   INHERITED FIXTURE: $probe is already an orphan on $SOURCE_DOMAIN, so the stage adopts that one rather than manufacturing another. Clearing it is now the same experiment as proving the adopt route."
		results_row "$CSV" "$sc" a_metadata_only_delete_leaves_the_bitmap_behind SKIP "" "" "" "" "" "SKIP fixture inherited from an earlier run"
		;;
	esac

	if [ "$inherited" = no ] && [ "$rc" != 0 ]; then
		warn "SKIP stage 22: checkpoint-create failed on $SOURCE_DOMAIN: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200). Does this libvirt/qemu support checkpoints (libvirt >= 5.6, qemu >= 4.2)?"
		results_row "$CSV" "$sc" stood-down "" "" "" "" "" "" "SKIP checkpoint-create failed"
		probe_cleanup
		return 0
	fi

	if [ "$inherited" = no ]; then
		# Dumped while the checkpoint still exists, because P3a needs libvirt's
		# own description of it and after the --metadata delete below there is
		# nowhere left to get one -- that absence is what makes an orphan an
		# orphan, and why P3b rather than P3a is the check that matters.
		virsh_uri "$SOURCE_URI" checkpoint-dumpxml "$SOURCE_DOMAIN" "$probe" > "$dumped" 2>/dev/null || true

		# What each side says it has, right now, verbatim. These lines are the
		# stage's real output: every verdict below is derived from them, so when
		# a verdict is surprising its evidence is already in the log.
		log "   EVIDENCE after checkpoint-create:"
		log "     virsh checkpoint-list --name: $(virsh_uri "$SOURCE_URI" checkpoint-list "$SOURCE_DOMAIN" --name 2>&1 | tr '\n' ' ' | cut -c1-200)"
		log "     qemu-img bitmaps in the LIVE image: $(raw_bitmaps)"
		log "     virsh checkpoint-dumpxml: $(tr -d '\n' < "$dumped" | tr -s ' ' | cut -c1-300)"

		if ! cp_listed "$probe"; then
			warn "SKIP stage 22: checkpoint-create returned success but $probe is not in checkpoint-list, so there is nothing to orphan. That is a libvirt-side question rather than the one this stage asks."
			results_row "$CSV" "$sc" stood-down "" "" "" "" "" "" "SKIP checkpoint not listed after create"
			probe_cleanup
			return 0
		fi

		# THE discriminator, and a finding in its own right either way.
		bitmap_present "$probe" && visible_live=yes
		[ "$visible_live" = yes ] && orphan_proven=yes
		if [ "$visible_live" = yes ]; then
			fo_check "$sc" "a checkpoint's bitmap is visible in the live image" 0
		else
			fo_check "$sc" "a checkpoint's bitmap is visible in the live image" 1 \
				"libvirt made the checkpoint but qemu-img cannot see its bitmap while $SOURCE_DOMAIN runs. vmsync's own orphan audit reads bitmaps exactly this way (cmd/vmsync/bitmaps.go via disk.BitmapNames), so on a running source that audit sees nothing -- it cannot detect the orphan it exists to refuse on, and the \"Bitmap already exists\" failure comes from qemu's own state instead. Set PROBE_MAY_STOP_SOURCE=yes to have this stage shut the domain down and settle whether the bitmap is merely INVISIBLE or genuinely ABSENT"
		fi

		virsh_uri "$SOURCE_URI" checkpoint-delete "$SOURCE_DOMAIN" --checkpointname "$probe" --metadata >/dev/null 2>&1 || true
		log "   EVIDENCE after checkpoint-delete --metadata:"
		log "     virsh checkpoint-list --name: $(virsh_uri "$SOURCE_URI" checkpoint-list "$SOURCE_DOMAIN" --name 2>&1 | tr '\n' ' ' | cut -c1-200)"
		log "     qemu-img bitmaps in the LIVE image: $(raw_bitmaps)"

		if cp_listed "$probe"; then
			fo_check "$sc" "a metadata-only delete leaves the bitmap behind" 1 \
				"the checkpoint is still listed after a --metadata delete, so the orphan factory did not run"
			probe_cleanup
			return 0
		fi

		if [ "$visible_live" = yes ]; then
			orphan_proven=file
			fo_check "$sc" "a metadata-only delete leaves the bitmap behind" 0
			log "   orphan manufactured: $probe is in $active with no libvirt checkpoint"
		else
			# qemu-img cannot answer here, so ask qemu -- live, with no
			# shutdown. This replaces the old "stop the domain and look in the
			# file" discriminator, which could not be read either way round: a
			# clean shutdown is itself when a persistent bitmap gets written
			# into the image, so finding one afterwards never distinguished
			# "it was already there" from "closing the image put it there".
			qemu_holds_probe && settled=0 || settled=$?
			case "$settled" in
			0)
				orphan_proven=qemu
				fo_check "$sc" "a metadata-only delete leaves the bitmap behind" 0
				log "   orphan manufactured, and qemu says so itself: it refuses to create $probe again while qemu-img reports no such bitmap in $active and libvirt lists no checkpoint for it"
				;;
			1)
				orphan_proven=no
				fo_check "$sc" "a metadata-only delete leaves the bitmap behind" 1 \
					"qemu accepted a fresh $probe right after the metadata-only delete, so it is not holding a bitmap of that name: the delete took the bitmap with it, or none was made. Either way this host does not produce the orphan the stage needs, and nothing below would be testing one"
				probe_cleanup
				return 0
				;;
			*)
				log "   NOTE: neither qemu-img nor qemu gave a usable answer about $probe; what follows cannot lean on the fixture existing"
				results_row "$CSV" "$sc" a_metadata_only_delete_leaves_the_bitmap_behind SKIP "" "" "" "" "" "SKIP existence of the bitmap unresolved"
				;;
			esac

			# Only when qemu could not answer either, and only on request: this
			# is the one reading that speaks about the FILE rather than about
			# qemu's list, and it costs a stop and a start.
			if [ "$orphan_proven" = unknown ]; then
				settle_stopped "$probe" && settled=0 || settled=$?
				case "$settled" in
				0)
					orphan_proven=file
					fo_check "$sc" "a persistent bitmap of that name is in the image once the domain is down" 0
					log "   FINDING: with the domain down the bitmap is in $active. Note what this does NOT say: the clean shutdown is itself when qemu writes a persistent bitmap out, so this does not establish the bitmap was in the file while the domain ran -- only that it is there now, which is enough for it to block every later sync"
					;;
				1)
					orphan_proven=no
					fo_check "$sc" "a persistent bitmap of that name is in the image once the domain is down" 1 \
						"with the domain shut off there is still no $probe in $active, and a clean shutdown is exactly when a persistent bitmap would have been written there -- so none was made, and the orphan this stage tried to build does not exist"
					;;
				*) log "   set PROBE_MAY_STOP_SOURCE=yes to read $active directly (it stops and restarts $SOURCE_DOMAIN)" ;;
				esac
			fi
		fi
	fi

	# --- P3a. redefine from libvirt's own dumped xml ------------------------
	#
	# There is nothing to dump for an inherited orphan -- no checkpoint, which is
	# what makes it one -- so reporting a refusal here would be a failure
	# invented out of a description the stage never had. P3b is the check for
	# that case, and it is the one that matters anyway.
	if [ "$inherited" = yes ]; then
		log "   SKIP P3a: $probe was already an orphan, so libvirt has no description of it to redefine from. That is the real case, and P3b is the check for it."
		results_row "$CSV" "$sc" "redefine_from_libvirt's_own_dumped_xml_is_accepted" SKIP "" "" "" "" "" "SKIP no dumped xml for an inherited orphan"
	elif [ -s "$dumped" ] && virsh_uri "$SOURCE_URI" checkpoint-create "$SOURCE_DOMAIN" --xmlfile "$dumped" --redefine >/dev/null 2>&1 && cp_listed "$probe"; then
		fo_check "$sc" "redefine from libvirt's own dumped xml is accepted" 0
		# Put it back to orphaned so P3b tests the case that matters.
		virsh_uri "$SOURCE_URI" checkpoint-delete "$SOURCE_DOMAIN" --checkpointname "$probe" --metadata >/dev/null 2>&1 || true
	else
		fo_check "$sc" "redefine from libvirt's own dumped xml is accepted" 1 \
			"even libvirt's own checkpoint description was refused on redefine, so the adopt route is not available at all on this libvirt"
	fi

	# --- P3b. redefine from the xml VMSYNC would have to build --------------
	#
	# THE check this stage exists for. A real orphan has no dumped xml anywhere --
	# that is what makes it an orphan -- so vmsync would have to construct the
	# metadata from the bitmap name and the disk it sits on. If libvirt refuses
	# this but accepted P3a, the adopt route is only usable where the metadata
	# was never actually lost, which is not the reported case.
	# Four candidate shapes, cheapest first, because "libvirt refused" and
	# "libvirt refused THIS description" are different answers and only the
	# second one is useful. The first hardware run was refused for
	# "missing creationTime from existing checkpoint" and the stage reported
	# that the adopt route needed a different answer -- a conclusion the error
	# did not support. What vmsync has to construct is whichever shape libvirt
	# takes, so the stage reports exactly that, and the error for each shape it
	# would not take.
	local built="$RUN_DIR/${sc}.built"
	local now shape accepted= adopted_xml=
	now="$(date +%s)"

	# 1. name and the disk. What a caller would try first.
	cat > "$built.1.xml" <<XML
<domaincheckpoint>
  <name>$probe</name>
  <disks>
    <disk name='$TAMPER_DISK_DEV' checkpoint='bitmap' bitmap='$probe'/>
  </disks>
</domaincheckpoint>
XML
	# 2. plus creationTime, which REDEFINE demands of an existing checkpoint.
	cat > "$built.2.xml" <<XML
<domaincheckpoint>
  <name>$probe</name>
  <creationTime>$now</creationTime>
  <disks>
    <disk name='$TAMPER_DISK_DEV' checkpoint='bitmap' bitmap='$probe'/>
  </disks>
</domaincheckpoint>
XML
	# 3. plus the domain definition, which libvirt's own checkpoint dumps carry
	# and may require back. --inactive for the persistent config: the live one
	# carries a runtime id that is not part of the definition.
	local domxml="$RUN_DIR/${sc}.domain.xml"
	virsh_uri "$SOURCE_URI" dumpxml --inactive "$SOURCE_DOMAIN" 2>/dev/null \
		| grep -v '^<?xml' > "$domxml" || true
	{
		printf '<domaincheckpoint>\n  <name>%s</name>\n  <creationTime>%s</creationTime>\n' "$probe" "$now"
		printf '  <disks>\n    <disk name=%s checkpoint=%s bitmap=%s/>\n  </disks>\n' \
			"'$TAMPER_DISK_DEV'" "'bitmap'" "'$probe'"
		cat "$domxml"
		printf '</domaincheckpoint>\n'
	} > "$built.3.xml"
	# 4. plus a parent, for a libvirt that wants the orphan placed in the chain
	# it already has. Only built when there IS a leaf to parent it to.
	local leaf
	leaf="$(virsh_uri "$SOURCE_URI" checkpoint-list "$SOURCE_DOMAIN" --name 2>/dev/null | grep -v '^$' | tail -1 || true)"
	if [ -n "$leaf" ] && [ "$leaf" != "$probe" ]; then
		{
			printf '<domaincheckpoint>\n  <name>%s</name>\n  <creationTime>%s</creationTime>\n' "$probe" "$now"
			printf '  <parent>\n    <name>%s</name>\n  </parent>\n' "$leaf"
			printf '  <disks>\n    <disk name=%s checkpoint=%s bitmap=%s/>\n  </disks>\n' \
				"'$TAMPER_DISK_DEV'" "'bitmap'" "'$probe'"
			cat "$domxml"
			printf '</domaincheckpoint>\n'
		} > "$built.4.xml"
	fi

	for shape in "1:name and disk only" "2:plus creationTime" "3:plus creationTime and the domain definition" "4:plus creationTime, parent $leaf and the domain definition"; do
		local n="${shape%%:*}" what="${shape#*:}"
		[ -s "$built.$n.xml" ] || continue
		out="$(virsh_uri "$SOURCE_URI" checkpoint-create "$SOURCE_DOMAIN" --xmlfile "$built.$n.xml" --redefine 2>&1)" && rc=0 || rc=$?
		if [ "$rc" = 0 ] && cp_listed "$probe"; then
			accepted="$what"
			adopted_xml="$built.$n.xml"
			log "   libvirt ACCEPTED the redefine from shape $n ($what)"
			break
		fi
		log "   libvirt refused shape $n ($what): $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
	done

	if [ -n "$accepted" ]; then
		fo_check "$sc" "redefine from a hand-built xml is accepted" 0
		log "   WHAT VMSYNC MUST BUILD: $accepted. The xml is in $built.*.xml."
	else
		fo_check "$sc" "redefine from a hand-built xml is accepted" 1 \
			"libvirt refused every shape tried, the richest being a full dumped domain definition with creationTime and a parent -- so on this libvirt there is no description vmsync could construct for an orphan it has no metadata for, and the live-source case needs a different answer. The attempted xml and each refusal are above and in $built.*.xml"
		probe_cleanup
		return 0
	fi

	# --- P4. does deleting the adopted checkpoint remove the bitmap? --------
	out="$(virsh_uri "$SOURCE_URI" checkpoint-delete "$SOURCE_DOMAIN" --checkpointname "$probe" 2>&1)" && rc=0 || rc=$?
	log "   EVIDENCE after deleting the adopted checkpoint: exit $rc, bitmaps: $(raw_bitmaps)"
	if [ "$visible_live" != yes ]; then
		# The removal has to be confirmed through whichever oracle established
		# the fixture in the first place. Asking a different one invites the
		# vacuous pass: a live qemu-img read reports "gone" for a bitmap it
		# could never see, and a post-shutdown read reports "gone" for one that
		# was only ever in qemu's memory. The adopt and the delete themselves
		# still ran against a RUNNING domain, which is the part under test.
		if [ "$rc" = 0 ] && ! cp_listed "$probe" && [ "$orphan_proven" = qemu ]; then
			log "   libvirt accepted the delete of the adopted checkpoint; asking qemu whether the bitmap went with it"
			qemu_holds_probe && settled=0 || settled=$?
			case "$settled" in
			1)
				fo_check "$sc" "deleting the adopted checkpoint removes the bitmap" 0
				log "   ADOPT ROUTE WORKS: adopt and delete both ran against a RUNNING domain, and qemu now accepts $probe again -- it is no longer holding that bitmap"
				;;
			0)
				fo_check "$sc" "deleting the adopted checkpoint removes the bitmap" 1 \
					"libvirt accepted both the redefine and the delete, but qemu still refuses to create $probe, so it still holds that bitmap. The adopt route clears libvirt's record and leaves the orphan it was supposed to remove"
				# Stops here, as the file-oracle branch does. The paused repeat
				# only asks whether the same sequence also works paused, which
				# is not a question worth more state churn once it is known not
				# to work running.
				probe_cleanup
				return 0
				;;
			*)
				log "   qemu gave no usable answer about whether the bitmap went"
				results_row "$CSV" "$sc" deleting_the_adopted_checkpoint_removes_the_bitmap SKIP "" "" "" "" "" "SKIP removal unresolved"
				;;
			esac
		elif [ "$rc" = 0 ] && ! cp_listed "$probe" && [ "$orphan_proven" = file ]; then
			log "   libvirt accepted the delete of the adopted checkpoint; the file half needs a read with the domain down"
			settle_stopped "$probe" && settled=0 || settled=$?
			case "$settled" in
			1)
				fo_check "$sc" "deleting the adopted checkpoint removes the bitmap" 0
				log "   ADOPT ROUTE WORKS: adopt and delete both ran against a RUNNING domain and the bitmap is gone from $active"
				;;
			0)
				fo_check "$sc" "deleting the adopted checkpoint removes the bitmap" 1 \
					"libvirt accepted both the redefine and the delete, but with the domain shut off $probe is still in $active -- the adopt route clears libvirt's record and leaves the bitmap, which is the same orphan it set out to remove"
				;;
			*)
				log "   whether the bitmap went with it is not readable while the domain runs; PROBE_MAY_STOP_SOURCE=yes settles it"
				results_row "$CSV" "$sc" deleting_the_adopted_checkpoint_removes_the_bitmap SKIP "" "" "" "" "" "SKIP bitmap not readable while live"
				;;
			esac
		elif [ "$rc" = 0 ] && ! cp_listed "$probe"; then
			# libvirt took the delete, but no bitmap was ever shown to exist, so
			# a later read finding none would mean nothing. Reading "gone" off an
			# orphan that was never there is how this check passed on a host
			# where the fixture had failed.
			log "   libvirt accepted the delete of the adopted checkpoint, but no bitmap was ever shown to exist (orphan_proven=$orphan_proven), so its removal cannot be claimed"
			results_row "$CSV" "$sc" deleting_the_adopted_checkpoint_removes_the_bitmap SKIP "" "" "" "" "" "SKIP no bitmap proven to remove"
		else
			fo_check "$sc" "deleting the adopted checkpoint removes the bitmap" 1 \
				"exit $rc, still listed=$(cp_listed "$probe" && echo yes || echo no): $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200). Either libvirt will not delete a checkpoint it has just accepted a redefine for, or there was no bitmap to delete and the redefine was taken on description alone -- which of the two cannot be told apart from here, since the bitmap was never readable. PROBE_MAY_STOP_SOURCE=yes settles it"
			probe_cleanup
			return 0
		fi
	elif [ "$rc" = 0 ] && ! bitmap_present "$probe" && ! cp_listed "$probe"; then
		fo_check "$sc" "deleting the adopted checkpoint removes the bitmap" 0
		log "   ADOPT ROUTE WORKS on a running domain: the bitmap is gone from $active"
	else
		fo_check "$sc" "deleting the adopted checkpoint removes the bitmap" 1 \
			"exit $rc, bitmap_present=$(bitmap_present "$probe" && echo yes || echo no): $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200). Adoption succeeded but the delete did not take the bitmap away, which is the half that matters"
		probe_cleanup
		return 0
	fi

	# --- P5. and the same while PAUSED -------------------------------------
	#
	# -reinit -start brings a shut-off source up PAUSED before the chain drop, so
	# the paused case is the maintainer's exact failing scenario rather than a
	# variation on it.
	if virsh_uri "$SOURCE_URI" suspend "$SOURCE_DOMAIN" >/dev/null 2>&1; then
		# Step by step rather than one chained condition, because a paused qemu
		# still holds the image: when the bitmap is unreadable the chain would
		# break at the first link and report "paused does not work" when what
		# actually happened is "this host cannot see the bitmap either way".
		local p_adopt=1 p_del=1
		virsh_uri "$SOURCE_URI" checkpoint-create "$SOURCE_DOMAIN" --xmlfile "$xml" >/dev/null 2>&1 || true
		virsh_uri "$SOURCE_URI" checkpoint-delete "$SOURCE_DOMAIN" --checkpointname "$probe" --metadata >/dev/null 2>&1 || true
		if virsh_uri "$SOURCE_URI" checkpoint-create "$SOURCE_DOMAIN" --xmlfile "$adopted_xml" --redefine >/dev/null 2>&1; then
			p_adopt=0
			if virsh_uri "$SOURCE_URI" checkpoint-delete "$SOURCE_DOMAIN" --checkpointname "$probe" >/dev/null 2>&1; then
				p_del=0
			fi
		fi
		log "   EVIDENCE while PAUSED: redefine accepted=$p_adopt, delete accepted=$p_del (0 = accepted), bitmaps: $(raw_bitmaps)"
		if [ "$visible_live" != yes ] && [ "$p_del" = 0 ]; then
			# qemu answers while paused just as it does while running, so the
			# paused case gets a real verdict rather than the SKIP it used to
			# get for being unreadable through qemu-img.
			qemu_holds_probe && settled=0 || settled=$?
			case "$settled" in
			1) fo_check "$sc" "the same works while the domain is paused" 0 ;;
			0)
				fo_check "$sc" "the same works while the domain is paused" 1 \
					"libvirt accepted the adopt and the delete while $SOURCE_DOMAIN was paused, but qemu still holds the bitmap afterwards -- and paused is the state -reinit -start puts a source into"
				;;
			*)
				log "   libvirt's half of the paused sequence: redefine=$p_adopt delete=$p_del (0 = accepted); qemu gave no usable answer about the bitmap"
				results_row "$CSV" "$sc" the_same_works_while_the_domain_is_paused SKIP "" "" "" "" "" "SKIP paused removal unresolved"
				;;
			esac
		elif [ "$visible_live" != yes ]; then
			log "   libvirt refused part of the paused sequence: redefine=$p_adopt delete=$p_del (0 = accepted)"
			fo_check "$sc" "the same works while the domain is paused" 1 \
				"the adopt-then-delete sequence was not even accepted while $SOURCE_DOMAIN was paused (redefine accepted=$p_adopt, delete accepted=$p_del, 0 = accepted) -- and paused is the state -reinit -start puts a source into"
		elif [ "$p_del" = 0 ] && ! bitmap_present "$probe"; then
			fo_check "$sc" "the same works while the domain is paused" 0
		else
			fo_check "$sc" "the same works while the domain is paused" 1 \
				"the adopt-then-delete sequence did not clear $probe while $SOURCE_DOMAIN was paused (redefine accepted=$p_adopt, delete accepted=$p_del, 0 = accepted) -- and paused is the state -reinit -start puts a source into"
		fi
		virsh_uri "$SOURCE_URI" resume "$SOURCE_DOMAIN" >/dev/null 2>&1 || \
			warn "could not resume $SOURCE_DOMAIN after the paused probe -- resume it by hand ('virsh -c $SOURCE_URI resume $SOURCE_DOMAIN')"
	else
		log "   SKIP: could not suspend $SOURCE_DOMAIN, so the paused case is untested"
		results_row "$CSV" "$sc" the_same_works_while_the_domain_is_paused SKIP "" "" "" "" "" "SKIP suspend failed"
	fi

	probe_cleanup
	unset -f bitmap_names bitmap_present cp_listed cp_covers_disk raw_bitmaps qemu_holds_probe sweep_offline probe_reset settle_stopped probe_cleanup
	return 0
}

stage_pattern() {
	case "$1" in
	# Anchored to the named sub-tests rather than a bare ^verify- , so a
	# run doing both stages does not fold stage 8's rows into stage 2's
	# verdict. "precondition" is in the list because a stage that stands
	# down records its reason under that name, and a reason nothing matches
	# is a reason nobody reads: the verdict would say "nothing recorded"
	# rather than why the stage skipped.
	verify) printf '^verify-(guard|fast|full|qemu-img|fast-bytes|full-bytes|cross|oracle|precondition)$' ;;
	checksum) printf '^checksum$' ;;
	reinit) printf '^reinit-after-failures$' ;;
	snapshot) printf '^ext-snapshot$' ;;
	define) printf '^define-(uuid-collision|rollback|metadata|precondition)$' ;;
	failover) printf '^failover$' ;;
	fence-agent) printf '^fence-agent$' ;;
	verify-long) printf '^verify-long$' ;;
	retention) printf '^retention$' ;;
	restore) printf '^restore$' ;;
	invert) printf '^invert$' ;;
	wedge) printf '^wedge$' ;;
	# Anchored to the exact scenario name for stage 2's reason: a bare
	# ^verify- would fold all of stage 2's rows into this stage's verdict.
	verify-failure) printf '^verify-failure$' ;;
	commit-barrier) printf '^commit-barrier$' ;;
	# Anchored whole, so stage 3's ^reinit-after-failures$ and this one cannot
	# fold into each other on a run doing both.
	interrupted-reinit) printf '^interrupted-reinit$' ;;
	journal) printf '^journal$' ;;
	# "precondition" is in the list because this stage stands down on a target
	# whose filesystem has no reflink support, and that row has to belong to
	# stage 18's verdict rather than to whichever stage ran before it.
	colocated) printf '^(colocated|precondition)$' ;;
	# Same shape as stage 18: this one stands down on a target with no
	# TARGET_DISK_PATH, and that row belongs to stage 19's verdict.
	leftovers) printf '^(leftovers|precondition)$' ;;
	# Stage 20 stands down on several preconditions (no target domain, a target
	# that will not boot, no source chain to protect), and those rows belong to
	# its own verdict rather than to whichever stage ran before it.
	reinit-order) printf '^(reinit-order|precondition)$' ;;
	# Stage 21 stands down on several preconditions (no TARGET_HOST, no helper on
	# the target, a sync it could not stall), and those rows belong to its verdict.
	lock-lease) printf '^(lock-lease|precondition)$' ;;
	# Stage 22 is a PROBE rather than a test: it asks libvirt a question whose
	# answer decides a design. Its precondition rows belong to its own verdict.
	redefine-probe) printf '^(redefine-probe|precondition|cleanup)$' ;;
	*) printf '$^' ;; # matches nothing
	esac
}

# stage_verdict STAGE -> "STATUS<TAB>DETAIL". Never fails; an unknown stage
# reports SKIPPED rather than inventing a result.
stage_verdict() {
	local stage="$1"
	if [ "$stage" = matrix ]; then
		# DRYRUN is counted apart from both. A dry run prints command
		# lines and executes nothing, so its rows are evidence of nothing
		# -- folding them in with the zero exit codes made --dry-run
		# report "BENCH RESULT: PASS" against hosts never contacted.
		awk -F, -v non="$NON_MATRIX_SCENARIOS" '
			NR>1 && $1 !~ non { n++; if ($3 == "DRYRUN") d++; else if ($3 != "0") f++ }
			END {
				if (n == 0)      printf "SKIPPED\tnothing ran"
				else if (f > 0)  printf "FAIL\t%d of %d vmsync runs exited non-zero", f, n
				else if (d == n) printf "SKIPPED\t%d command lines printed, nothing executed", n
				else             printf "PASS\t%d vmsync runs, all exited 0", n
			}' "$CSV"
		return 0
	fi
	# A `stood-down` row counted apart from the rest, because a stage that gave
	# up is not a stage that passed. Stage 22 reported "PASS -- 1 checks passed,
	# 1 skipped" after answering none of its seven questions: it had got as far
	# as its first check, then stopped, and p>0 && s>0 landed in the PASS
	# branch. Such a row reads SKIPPED whatever passed before it, with the count
	# kept so the detail still says how far it got.
	#
	# A distinct phase, not `precondition`, because the two are not the same
	# event: stage 21 writes a precondition SKIP when ONE HALF of it cannot run
	# and then goes on to pass the other half, and that run is a pass. Only a
	# row written on the way out counts here.
	awk -F, -v pat="$(stage_pattern "$stage")" '
		NR>1 && $1 ~ pat {
			if ($2 == "stood-down" && $9 ~ /^SKIP/) stood_down++
			if      ($9 ~ /^FAIL/) f++
			else if ($9 ~ /^PASS/) p++
			else if ($9 ~ /^SKIP/) s++
		}
		END {
			if (f > 0)                  printf "FAIL\t%d of %d checks failed", f, f+p
			else if (stood_down > 0)    printf "SKIPPED\tstood down after %d check(s) passed", p
			else if (p > 0 && s > 0)    printf "PASS\t%d checks passed, %d skipped", p, s
			else if (p > 0)             printf "PASS\t%d checks passed", p
			else if (s > 0)             printf "SKIPPED\t%d checks skipped", s
			else                        printf "SKIPPED\tnothing recorded"
		}' "$CSV"
}

# announce_stage_verdict logs one stage's outcome as a banner.
#
# A banner because the thing being answered is "did that work", and a run
# emits hundreds of lines -- an outcome on one indistinguishable line among
# them is one nobody finds without searching for it.
announce_stage_verdict() {
	local stage="$1" rc="${2:-0}" verdict status detail
	verdict="$(stage_verdict "$stage")"
	status="${verdict%%	*}"
	detail="${verdict#*	}"

	# A stage that exited non-zero is a FAIL whatever its recorded checks
	# say, because it stopped before it could record the rest of them. The
	# checks it did record stay in the detail, so the banner reports both
	# what it found and that there was more it never got to.
	if [ "$rc" != 0 ]; then
		detail="stage exited with status $rc before finishing (recorded so far: $detail)"
		status=FAIL
	fi

	RAN_STAGES+=("$stage")
	RAN_VERDICTS+=("$status")
	RAN_DETAILS+=("$detail")

	log "------------------------------------------------------------"
	case "$status" in
	FAIL) warn "  stage $stage: FAIL -- $detail" ;;
	PASS) log "  stage $stage: PASS -- $detail" ;;
	*) log "  stage $stage: SKIPPED -- $detail" ;;
	esac
	log "------------------------------------------------------------"
}

RAN_STAGES=()
RAN_VERDICTS=()
RAN_DETAILS=()

# overall_verdict -> FAIL, PASS or NOTHING VERIFIED.
#
# One function so the banner, the report and the exit status cannot disagree
# about whether the run worked.
#
# Three states, not two, because a run where every stage skipped -- a dry
# run, or a config missing the binaries the opt-in stages need -- has proven
# nothing, and calling that PASS is the same false reassurance as a stage
# reporting "all assertions passed" when it ran none.
overall_verdict() {
	local i passed=no
	for i in "${!RAN_STAGES[@]}"; do
		case "${RAN_VERDICTS[$i]}" in
		FAIL)
			printf 'FAIL'
			return 0
			;;
		PASS) passed=yes ;;
		esac
	done
	if [ "$passed" = yes ]; then
		printf 'PASS'
	else
		printf 'NOTHING VERIFIED'
	fi
}

# final_verdict prints the run's overall outcome and returns 0 only if every
# stage that ran passed or was skipped.
#
# A run that ends without saying whether it worked is one whose result gets
# decided by whoever scrolls furthest, so this is the last thing printed --
# after the report, which is long.
final_verdict() {
	local i status overall
	overall="$(overall_verdict)"

	log "============================================================"
	case "$overall" in
	FAIL) warn "  BENCH RESULT: FAIL" ;;
	PASS) log "  BENCH RESULT: PASS" ;;
	*) warn "  BENCH RESULT: NOTHING VERIFIED -- every stage skipped, so this run proves nothing" ;;
	esac
	log "============================================================"
	for i in "${!RAN_STAGES[@]}"; do
		status="${RAN_VERDICTS[$i]}"
		printf '    %-12s %-8s %s\n' "${RAN_STAGES[$i]}" "$status" "${RAN_DETAILS[$i]}"
	done
	log "============================================================"
	log "results: $RUN_DIR"

	# FAIL is always non-zero. "Nothing verified" is non-zero too in a real
	# run -- being asked for stages and proving none of them is a problem,
	# usually a config one -- but not in a dry run, where verifying nothing
	# is the entire point and failing would make --dry-run useless as a
	# syntax check.
	case "$overall" in
	FAIL) return 1 ;;
	PASS) return 0 ;;
	*) [ "$DRY_RUN" = yes ] ;;
	esac
}

# --- report ------------------------------------------------------------------

generate_report() {
        local report="$RUN_DIR/report.md" i
        {
                echo "# vmsync benchmark report"
                echo
                echo "## Result: $(overall_verdict)"
                echo
                echo "| stage | result | |"
                echo "|---|---|---|"
                for i in "${!RAN_STAGES[@]}"; do
                        echo "| ${RAN_STAGES[$i]} | ${RAN_VERDICTS[$i]} | ${RAN_DETAILS[$i]} |"
                done
                echo
                echo "- Run: $RUN_ID"
                echo "- Source: $SOURCE_DOMAIN @ $SOURCE_URI"
                echo "- Target: $TARGET_DOMAIN @ $TARGET_URI"
                echo "- Dry run: $DRY_RUN"
                echo
                echo "## Stage 1: transport matrix"
                echo
                echo "| scenario | phase | exit | wall (s) | transferred (MiB) | throughput (MiB/s) |"
                echo "|---|---|---|---|---|---|"
                # Excludes Stage 2/3's own scenario names explicitly -- phase names
                # ("full", "incremental") are only unique WITHIN Stage 1; Stage 2's
                # baseline run also uses phase "full" and would otherwise leak into
                # this table too.
                awk -F, 'NR>1 && ($2=="full" || $2=="incremental") && $1 !~ /^verify-/ && $1 != "reinit-after-failures" {
                        mib = ($5+0) / 1048576
                        secs = $4+0
                        thr = (secs>0) ? mib/secs : 0
                        printf "| %s | %s | %s | %s | %.1f | %.2f |\n", $1, $2, $3, $4, mib, thr
                }' "$CSV"
                echo
                echo "## Stage 2: verify + tamper detection"
                echo
                echo "| mode | phase | exit | wall (s) | result |"
                echo "|---|---|---|---|---|"
                # Kept in step with stage_pattern's "verify" entry by hand:
                # a sub-test missing here is not an error anywhere, it just
                # silently drops out of the report while still counting
                # toward the stage verdict -- which is how the cross-check
                # and clean-oracle rows went unlisted when they were added.
                awk -F, 'NR>1 && $1 ~ /^verify-(guard|baseline|fast|full|qemu-img|fast-bytes|full-bytes|cross|oracle|precondition)/ { printf "| %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $9 }' "$CSV"
                echo
                echo "## Stage 13: pre-commit integrity check"
                echo
                if awk -F, 'NR>1 && $1=="checksum" { found=1 } END { exit !found }' "$CSV"; then
                        echo "| check | exit | wall (s) | result |"
                        echo "|---|---|---|---|"
                        awk -F, 'NR>1 && $1=="checksum" { printf "| %s | %s | %s | %s |\n", $2, $3, $4, $9 }' "$CSV"
                else
                        echo "_not run (opt in with \`--stages checksum\`; sub-test 13b deliberately fails a sync, and all four temporarily install helper shims on the target)_"
                fi
                echo
                echo "## Stage 14: a verification failure outliving its run"
                echo
                # Exact match on the scenario, so this cannot pick up stage 2's
                # or stage 8's verify- rows -- and stage 2's own awk above
                # cannot pick up these, since "failure" matches none of its
                # named sub-tests.
                if awk -F, 'NR>1 && $1=="verify-failure" { found=1 } END { exit !found }' "$CSV"; then
                        echo "| check | exit | wall (s) | result |"
                        echo "|---|---|---|---|"
                        awk -F, 'NR>1 && $1=="verify-failure" { printf "| %s | %s | %s | %s |\n", $2, $3, $4, $9 }' "$CSV"
                else
                        echo "_not run (opt in with \`--stages verify-failure\`; it corrupts the replica, four of its six sub-tests deliberately fail a sync, and one promotes the replica and puts the role straight back)_"
                fi
                echo
                echo "## Stage 15: the commit barrier"
                echo
                # Same exact match as stage 14's, for the same reason. The
                # SKIP rows matter more here than in most stages: this one
                # stands down on a single-disk source, and a stage that
                # silently vanished from the report would read as a stage
                # that passed.
                if awk -F, 'NR>1 && $1=="commit-barrier" { found=1 } END { exit !found }' "$CSV"; then
                        echo "| check | exit | wall (s) | result |"
                        echo "|---|---|---|---|"
                        awk -F, 'NR>1 && $1=="commit-barrier" { printf "| %s | %s | %s | %s |\n", $2, $3, $4, $9 }' "$CSV"
                else
                        echo "_not run (opt in with \`--stages commit-barrier\`; one sub-test deliberately fails a sync, and it needs a source domain with two or more qcow2 disks)_"
                fi
                echo
                echo "## Stage 16: an interrupted rebuild"
                echo
                # Same exact match, same reason. The SKIP rows are worth as
                # much as the PASS rows here: this stage stands down without
                # TARGET_VMSYNC_BIN and again when the fault does not fire,
                # and either one vanishing from the report would read as a
                # promotion refusal that had been proven when it had not.
                if awk -F, 'NR>1 && $1=="interrupted-reinit" { found=1 } END { exit !found }' "$CSV"; then
                        echo "| check | exit | wall (s) | result |"
                        echo "|---|---|---|---|"
                        awk -F, 'NR>1 && $1=="interrupted-reinit" { printf "| %s | %s | %s | %s |\n", $2, $3, $4, $9 }' "$CSV"
                else
                        echo "_not run (opt in with \`--stages interrupted-reinit\`; it kills a rebuild on purpose, leaves the target half-written, promotes it three times and rebuilds it, and needs TARGET_VMSYNC_BIN)_"
                fi
                echo
                echo "## Stage 17: the action journal"
                echo
                if awk -F, 'NR>1 && $1=="journal" { found=1 } END { exit !found }' "$CSV"; then
                        echo "| check | exit | wall (s) | result |"
                        echo "|---|---|---|---|"
                        awk -F, 'NR>1 && $1=="journal" { printf "| %s | %s | %s | %s |\n", $2, $3, $4, $9 }' "$CSV"
                else
                        echo "_not run (it is a default stage, so this means --stages named something else)_"
                fi
                echo
                echo "## Stage 18: two target domains in one directory"
                echo
                if awk -F, 'NR>1 && $1=="colocated" { found=1 } END { exit !found }' "$CSV"; then
                        echo "| check | exit | wall (s) | result |"
                        echo "|---|---|---|---|"
                        awk -F, 'NR>1 && $1=="colocated" { printf "| %s | %s | %s | %s |\n", $2, $3, $4, $9 }' "$CSV"
                else
                        echo "_not run (opt in with \`--stages colocated\`; it plants a second target domain's restore points beside this one's and checks that no sync, prune, staging sweep or reinit of this domain touches them, and needs TARGET_DISK_PATH set)_"
                fi
                echo
                echo "## Stage 8: verify after a long incremental chain"
                echo
                if awk -F, 'NR>1 && $1=="verify-long" { found=1 } END { exit !found }' "$CSV"; then
                        echo "Tamper placement: $TAMPER_MODE${TAMPER_MODE:+, seed \`$TAMPER_SEED\`}"
                        echo
                        echo "| phase | exit | wall (s) | transferred (MiB) | result |"
                        echo "|---|---|---|---|---|"
                        awk -F, 'NR>1 && $1=="verify-long" {
                                mib = ($5+0) / 1048576
                                printf "| %s | %s | %s | %.1f | %s |\n", $2, $3, $4, mib, $9
                        }' "$CSV"
                else
                        echo "_not run (opt in with \`--stages verify-long\`; it builds a $VERIFY_LONG_COPIES-deep chain per mode)_"
                fi
                echo
                echo "## Stage 9: retention (restore points on the target)"
                echo
                if awk -F, 'NR>1 && $1=="retention" { found=1 } END { exit !found }' "$CSV"; then
                        echo "| check | exit | result |"
                        echo "|---|---|---|"
                        awk -F, 'NR>1 && $1=="retention" { gsub(/_/, " ", $2); printf "| %s | %s | %s |\n", $2, $3, $9 }' "$CSV"
                else
                        echo "_not run (opt in with \`--stages retention\`; needs a reflink-capable target filesystem)_"
                fi
                echo
                echo "## Stage 3: reinit-after-failures"
                echo
                awk -F, 'NR>1 && $1=="reinit-after-failures" { printf "- %s/%s: exit=%s wall=%ss %s\n", $1, $2, $3, $4, $9 }' "$CSV"
                echo
                echo "## Stage 4: external snapshot lifecycle"
                echo
                echo "| phase | exit | wall (s) | notes |"
                echo "|---|---|---|---|"
                awk -F, 'NR>1 && $1=="ext-snapshot" { printf "| %s | %s | %s | %s |\n", $2, $3, $4, $9 }' "$CSV"
                echo
                echo "## Stage 5: DefineDomain redefine/rollback coverage"
                echo
                echo "| test | phase | exit | wall (s) | notes |"
                echo "|---|---|---|---|---|"
                awk -F, 'NR>1 && ($1=="define-uuid-collision" || $1=="define-rollback" || $1=="define-metadata") { printf "| %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $9 }' "$CSV"
                echo
                echo "## Stage 6: failover, fencing, and the way back"
                echo
                # Every row is one assertion, so the useful rendering is a
                # pass/fail list rather than timings -- nothing here is a
                # benchmark, and a wall-clock column would just be noise.
                # Only the ASSERTION rows, which are the ones whose notes start
                # with PASS/FAIL/SKIP. The stage also emits ordinary run rows
                # (baseline, resync, ...) through run_vmsync, and listing those
                # as though they were assertions would report a phase name as
                # a passing test.
                if awk -F, 'NR>1 && $1=="failover" && $9 ~ /^(PASS|FAIL|SKIP)/ { found=1 } END { exit !found }' "$CSV"; then
                        awk -F, 'NR>1 && $1=="failover" && $9 ~ /^(PASS|FAIL|SKIP)/ {
                                gsub(/_/, " ", $2)
                                if ($9 ~ /^FAIL/)      printf "- **FAIL** — %s (%s)\n", $2, substr($9, 6)
                                else if ($9 ~ /^SKIP/) printf "- _skipped_ — %s\n", $2
                                else                   printf "- **PASS** — %s\n", $2
                        }' "$CSV"
                        echo
                        awk -F, 'NR>1 && $1=="failover" && $9 ~ /^(PASS|FAIL|SKIP)/ {
                                        if ($9 ~ /^FAIL/) f++; else if ($9 ~ /^SKIP/) s++; else p++
                                }
                                END {
                                        if (f)      printf "**%d failover assertion(s) failed.**\n", f
                                        else if (p) printf "All %d failover assertion(s) passed.\n", p
                                        else        printf "_Nothing was executed (%d skipped)._\n", s
                                }' "$CSV"
                else
                        echo "_not run (opt in with \`--stages failover\`)_"
                fi
                echo
                echo "## Stage 7: fencing end to end, with real agents"
                echo
                if awk -F, 'NR>1 && $1=="fence-agent" && $9 ~ /^(PASS|FAIL|SKIP)/ { found=1 } END { exit !found }' "$CSV"; then
                        awk -F, 'NR>1 && $1=="fence-agent" && $9 ~ /^(PASS|FAIL|SKIP)/ {
                                gsub(/_/, " ", $2)
                                if ($9 ~ /^FAIL/)      printf "- **FAIL** — %s (%s)\n", $2, substr($9, 6)
                                else if ($9 ~ /^SKIP/) printf "- _skipped_ — %s\n", $2
                                else                   printf "- **PASS** — %s\n", $2
                        }' "$CSV"
                        echo
                        awk -F, 'NR>1 && $1=="fence-agent" && $9 ~ /^(PASS|FAIL|SKIP)/ {
                                        if ($9 ~ /^FAIL/) f++; else if ($9 ~ /^SKIP/) s++; else p++
                                }
                                END {
                                        if (f)      printf "**%d fencing assertion(s) failed.**\n", f
                                        else if (p) printf "All %d fencing assertion(s) passed.\n", p
                                        else        printf "_Nothing was executed (%d skipped)._\n", s
                                }' "$CSV"
                else
                        echo "_not run (opt in with \`--stages fence-agent\`; it stops the source VM)_"
                fi
                echo
                echo "Full machine-readable data: \`results.csv\`. Per-run logs: \`logs/\`. Raw prometheus textfiles: \`prom/\`."
        } >"$report"
        log "report written to $report"
        cat "$report"
        return 0
}

# --- main --------------------------------------------------------------------

preflight

IFS=',' read -ra stage_list <<<"$STAGES"
# Each stage's exit status is captured rather than left to `set -e`.
#
# A stage is a bare command in this case statement, so under `set -e` a
# non-zero return from one ends the entire script on the spot -- after the
# stage has done its work, before generate_report and final_verdict, and
# without a word about which stage it was or why. A run that stops between
# "the stage finished" and "here is what it found" is the single most
# confusing thing this harness can do: everything looks like it worked, and
# then there is simply no report.
#
# So the status is caught, named, and folded into that stage's verdict. The
# report always prints.
for s in "${stage_list[@]}"; do
        stage_rc=0
        case "$s" in
        matrix) stage_matrix || stage_rc=$? ;;
        verify) stage_verify_tamper || stage_rc=$? ;;
        checksum) stage_checksum || stage_rc=$? ;;
        reinit) stage_reinit_after_failures || stage_rc=$? ;;
        snapshot) stage_external_snapshot || stage_rc=$? ;;
        define) stage_define_domain || stage_rc=$? ;;
        failover) stage_failover || stage_rc=$? ;;
        fence-agent) stage_fence_agent || stage_rc=$? ;;
        verify-long) stage_verify_long || stage_rc=$? ;;
        retention) stage_retention || stage_rc=$? ;;
        restore) stage_restore || stage_rc=$? ;;
        invert) stage_invert || stage_rc=$? ;;
        wedge) stage_wedge || stage_rc=$? ;;
        verify-failure) stage_verify_failure || stage_rc=$? ;;
        commit-barrier) stage_commit_barrier || stage_rc=$? ;;
        interrupted-reinit) stage_interrupted_reinit || stage_rc=$? ;;
        journal) stage_journal || stage_rc=$? ;;
        colocated) stage_colocated || stage_rc=$? ;;
        leftovers) stage_leftovers || stage_rc=$? ;;
        reinit-order) stage_reinit_order || stage_rc=$? ;;
        lock-lease) stage_lock_lease || stage_rc=$? ;;
        redefine-probe) stage_redefine_probe || stage_rc=$? ;;
        *) die "unknown stage '$s' in --stages (want matrix,verify,checksum,reinit,snapshot,define,failover,fence-agent,verify-long,retention,restore,invert,wedge,verify-failure,commit-barrier,interrupted-reinit,journal,colocated,leftovers,reinit-order,lock-lease,redefine-probe)" ;;
        esac
        if [ "$stage_rc" != 0 ]; then
                warn "stage $s returned exit status $stage_rc -- it did not finish cleanly. Whatever it recorded before that point is in the report below; the run continues so the remaining stages and the report still happen."
        fi
        announce_stage_verdict "$s" "$stage_rc"
done

generate_report

# Last, and it decides the exit status: a harness that always exits 0 cannot
# be used by anything that would act on the answer, and "did that run work"
# should not require reading a report to find out.
final_verdict
