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
STAGES="matrix,verify,reinit,snapshot"

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
  --stages LIST           comma-separated subset of: matrix,verify,verify-long,
                           reinit,snapshot,define,failover,fence-agent -- runs in
                           whichever order LIST gives them, not a fixed
                           canonical order
                           (default: matrix,verify,reinit,snapshot, which just
                           happens to already be written in that order; define,
                           failover and fence-agent are opt-in, see below)
  --dry-run               print every vmsync command line; touch nothing
                           (no ssh/qemu-io/vmsync calls actually made)
  -h, --help              this text

Stages 2, 3, and 4 each start with their own baseline -reinit full sync,
so none of them actually require Stage 1 (or any prior sync) to have run
first -- each is safe to run standalone via --stages. Stage 4 additionally
requires the SOURCE domain to be running.

Stage 5 (define) is NOT included by default -- pass --stages ...,define
explicitly. Its second half (5b) temporarily manipulates an iptables rule
on TARGET_HOST to force a DefineDomain redefine failure and confirm its
rollback restores the target's prior definition; it's self-healing and
this script also backstops the cleanup itself, but it's a different kind
of risk than anything the other stages do (which never touch host-level
networking), so it's opt-in even though it's no more destructive to the
target VM itself than -reinit already is. See this file's own Stage 5
comment before enabling it.

Stage 7 (fence-agent) is opt-in too, and is the most intrusive thing here:
it STOPS THE SOURCE VM. It runs real vmsync-agents in --standalone mode and
proves a fence is actually acted on, not merely written. It restores the
source's power state and both roles afterwards (and on a crash, via an EXIT
trap), but no other stage touches the source's power state at all. Needs
SOURCE_AGENT_BIN and TARGET_VMSYNC_BIN.

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
in the
config (vmsync on the TARGET host): -promote refuses a remote libvirt URI by
design, so it must run where the domain is. Without that setting the stage
skips rather than failing. Note it runs THREE full syncs in total: its own
baseline plus two more the disk-ownership checks need, since each property
they test only happens when a disk file is created from scratch.
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

# --- tamper placement --------------------------------------------------------

TAMPER_MODE="${TAMPER_MODE:-random}"
TAMPER_BAND_START="${TAMPER_BAND_START:-$TAMPER_OFFSET}"
TAMPER_BAND_END="${TAMPER_BAND_END:-0}" # 0 = up to the disk's virtual size
# 64 KiB, not 512, and that floor is load-bearing rather than cautious.
# nbdsync reports mismatches at a 4096-byte granularity (mismatchScanGranularity
# in pkg/nbdsync/nbd.go), so anything under 4 KiB is indistinguishable from a
# 4 KiB tamper and buys no coverage at all. Above that, -verify=online discards
# a whole reported range when ANY byte of it overlaps a region the guest wrote
# during the compare window (overlapsAnyExtent in cmd/vmsync/main.go), and those
# regions come from a qcow2 dirty bitmap at qemu's default 64 KiB granularity.
# A tamper smaller than one granule can therefore be swallowed entire by a
# single unrelated guest write, which reads as "verify missed it".
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
# on. qemu-io accepts k/M/G suffixes and bench.conf.example used to advertise
# them, but $(( 100M )) is a bash syntax error, and under `set -e` that ends
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

# --- Stage 8 (verify-long) ---------------------------------------------------

VERIFY_LONG_COPIES="${VERIFY_LONG_COPIES:-20}"
VERIFY_LONG_ATTEMPTS="${VERIFY_LONG_ATTEMPTS:-5}"
VERIFY_LONG_RECOPIES="${VERIFY_LONG_RECOPIES:-3}"

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
        # -verify=compare shells out to `qemu-img compare` on the host running
        # vmsync, not on either hypervisor (pkg/disk/disk.go's CompareImages).
        # Missing it locally makes that one mode fail for a reason that has
        # nothing to do with the replica.
        command -v qemu-img >/dev/null 2>&1 || die "qemu-img not found locally -- -verify=compare runs it on this host to compare the two NBD exports"

        domain_exists "$SOURCE_URI" "$SOURCE_DOMAIN" || die "source domain '$SOURCE_DOMAIN' not found via $SOURCE_URI${VIRSH_ERR:+: $VIRSH_ERR}"
        if domain_exists "$TARGET_URI" "$TARGET_DOMAIN"; then
                require_dom_shutoff "$TARGET_URI" "$TARGET_DOMAIN" "target"
        elif ! virsh_err_is_not_found; then
                warn "could not query target domain '$TARGET_DOMAIN' via $TARGET_URI: $VIRSH_ERR"
        fi
        [ -n "$TARGET_DISK_PATH" ] || warn "TARGET_DISK_PATH is empty -- target disk path resolution (used for tampering) is only reliable when the source has no active external snapshot. Recommended: always set TARGET_DISK_PATH in $CONF."
        log "preflight OK"
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
        [ -n "${BRIDGE_HELPER_PATH:-}" ] && VMSYNC_ARGS+=(-bridge-helper-path "$BRIDGE_HELPER_PATH")
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
bench_sync_hint() {
	if [ ${#BENCH_SYNC_EXTRA[@]} -gt 0 ]; then
		printf ' -- this stage adds "%s" to every sync (BENCH_SYNC_ARGS in %s); -compress needs vmsync-bridge-helper on %s, so set BENCH_SYNC_ARGS="" if that is not installed there' \
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
        SCEN_ARGS=(-io-depth "$iodepth")
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
			|| die "could not read the mtime of $path on $TARGET_HOST before tampering -- refusing to corrupt a file whose state cannot be restored"
	fi

	ssh_host_cmd "$TARGET_HOST" qemu-io -f qcow2 \
		-c "'write -P ${TAMPER_PATTERN} ${TAMPER_OFF} ${TAMPER_LEN}'" "'${path}'" \
		|| die "failed to inject test corruption into $path on $TARGET_HOST -- refusing to continue this sub-test"

	# Read it back. A qemu-io write that reports success but lands somewhere
	# else, or gets swallowed, would otherwise turn into "verify missed it".
	ssh_host_cmd "$TARGET_HOST" qemu-io -r -f qcow2 \
		-c "'read -P ${TAMPER_PATTERN} ${TAMPER_OFF} ${TAMPER_LEN}'" "'${path}'" >/dev/null \
		|| die "the test corruption did not read back from $path at offset $TAMPER_OFF length $TAMPER_LEN -- the tamper did not take, so nothing below would be testing what it claims"

	if [ "$preserve" = yes ]; then
		# touch -d @N sets nanoseconds to zero, so the restored stamp is at
		# worst marginally OLDER than the original -- never newer, and the
		# original passed the guard on the previous run by construction.
		ssh_host_cmd "$TARGET_HOST" touch -d "@$mtime" "'$path'" \
			|| die "could not restore the mtime of $path on $TARGET_HOST -- the sync below would fail at vmsync's mtime guard instead of reaching -verify, and would be scored as if it had verified"
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
                require_dom_shutoff_or_absent "$TARGET_URI" "$TARGET_DOMAIN" "target"
        fi

        # A known-clean baseline under the plain, no-bridge transport -- verify
        # correctness shouldn't depend on which transport carried the
        # preceding sync, and holding it fixed makes the three modes below
        # directly comparable to each other.
        bench_sync verify-baseline full -reinit
        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                die "baseline full sync for verify testing failed (see $RUN_LOG) -- aborting stage 2$(bench_sync_hint)"
        fi

        local target_path=""
        if [ "$DRY_RUN" != yes ]; then
                # "|| true": disk_source_path's own failure (e.g. virsh dumpxml
                # erroring under `set -o pipefail`) must fall through to the
                # specific, actionable die() below instead of silently killing the
                # script here with no message at all.
                target_path="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
                [ -n "$target_path" ] || die "could not resolve target disk path for dev='$TAMPER_DISK_DEV' via virsh dumpxml -- check TAMPER_DISK_DEV in $CONF"
                log "target disk under test: dev=$TAMPER_DISK_DEV path=$target_path (on $TARGET_HOST)"
        fi

        local vsize=0
        if [ "$DRY_RUN" != yes ]; then
                vsize="$(target_virtual_size "$target_path")" || true
                [ -n "$vsize" ] && [ "$vsize" -gt 0 ] 2>/dev/null \
                        || die "could not read the virtual size of $target_path on $TARGET_HOST via qemu-img info -- needed to keep a tamper inside the disk"
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
        for mode in compare fast online; do
                verify_mode_subtest "$target_path" "$vsize" "$mode" "verify-${mode}" tamper
        done
        return 0
}

# verify_guard_subtest PATH VSIZE -- asserts the mtime guard refuses a target
# that was written to behind vmsync's back.
verify_guard_subtest() {
        local path="$1" vsize="$2"

        log "--- verify-guard: tampering WITHOUT restoring mtime, expecting vmsync to refuse the sync ---"

        if [ "$DRY_RUN" != yes ]; then
                require_dom_shutoff "$TARGET_URI" "$TARGET_DOMAIN" "target"
                if ! draw_tamper "$vsize"; then
                        warn "SKIP verify-guard: the configured tamper band does not fit inside a ${vsize}-byte disk -- lower TAMPER_BAND_START or TAMPER_LENGTH_MIN in $CONF"
                        results_row "$CSV" verify-guard tamper-result "" "" "" "" "" "" "SKIP tamper band does not fit the disk"
                        return 0
                fi
                log "   corrupting at offset $TAMPER_OFF length $TAMPER_LEN (seed $TAMPER_SEED)"
                tamper_target "$path" no
        fi

        bench_sync verify-guard tamper

        if [ "$DRY_RUN" = yes ]; then
                results_row "$CSV" verify-guard tamper-result DRYRUN "" "" "" "" "" "SKIP dry run"
        elif [ "$RUN_RC" = 0 ]; then
                warn "FAIL: vmsync accepted an incremental sync into a target whose disk had been modified since the last sync -- the mtime guard did not fire. See $RUN_LOG"
                results_row "$CSV" verify-guard tamper-result 1 "" "" "" "" "" "FAIL mtime guard did not fire"
        elif grep -q "Target file on system is newer" "$RUN_LOG" 2>/dev/null; then
                log "   PASS: the mtime guard refused the sync"
                results_row "$CSV" verify-guard tamper-result 0 "" "" "" "" "" "PASS mtime guard refused the sync"
        else
                warn "FAIL: the sync failed, but not at the mtime guard -- something else went wrong first. See $RUN_LOG"
                results_row "$CSV" verify-guard tamper-result 1 "" "" "" "" "" "FAIL failed for some other reason"
        fi

        heal_target verify-guard tamper-heal "$path"
}

# verify_mode_subtest PATH VSIZE MODE SCENARIO PHASE -- asserts -verify=MODE
# detects a corruption the mtime guard cannot see.
#
# SCENARIO and PHASE are passed rather than derived because two stages call
# this: stage 2 once per mode, and stage 8 several times per mode. SCENARIO is
# what stage_pattern matches on, and PHASE has to be unique within a run or the
# per-run log and Prometheus files (named ${SCENARIO}.${PHASE}) overwrite each
# other and the failing attempt's evidence is gone.
verify_mode_subtest() {
        local path="$1" vsize="$2" mode="$3" scenario="$4" phase="$5" outcome

        log "--- verify=$mode: tampering ${path:-<unresolved in --dry-run>} with the mtime preserved ---"

        if [ "$DRY_RUN" != yes ]; then
                require_dom_shutoff "$TARGET_URI" "$TARGET_DOMAIN" "target"
                if ! draw_tamper "$vsize"; then
                        warn "SKIP $scenario/$phase: the configured tamper band does not fit inside a ${vsize}-byte disk"
                        results_row "$CSV" "$scenario" "${phase}-result" "" "" "" "" "" "" "SKIP tamper band does not fit the disk"
                        return 0
                fi
                log "   corrupting at offset $TAMPER_OFF length $TAMPER_LEN (seed $TAMPER_SEED)"
                tamper_target "$path" yes
        fi

        bench_sync "$scenario" "$phase" "-verify=$mode"

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
                        warn "FAIL: -verify=$mode ran and found NOTHING after the target was corrupted at offset $TAMPER_OFF length $TAMPER_LEN (reproduce with TAMPER_SEED=$TAMPER_SEED). See $RUN_LOG. Note: -verify=online can legitimately discard an in-window mismatch as inconclusive if the guest rewrote that exact region during the compare -- see README.md before treating this as a confirmed bug."
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
# -reinit specifically, and unconditionally. An incremental sync would NOT fix
# this: it re-copies only blocks the SOURCE's dirty bitmap says changed, and the
# source never wrote to the offset that was corrupted on the target.
heal_target() {
        local scenario="$1" phase="$2" path="$3"
        log "   healing target with a full resync (-reinit)"
        bench_sync "$scenario" "$phase" -reinit
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
                die "baseline full sync for reinit-after-failures testing failed (see $RUN_LOG) -- aborting stage 3$(bench_sync_hint)"
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
                        || die "could not remove target vmsync metadata for reinit-after-failures testing -- aborting stage 3"
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
stage_external_snapshot() {
        log "=== Stage 4: external snapshot lifecycle (sync while a snapshot exists, then after it's removed) ==="

        if [ "$DRY_RUN" != yes ]; then
                require_dom_shutoff_or_absent "$TARGET_URI" "$TARGET_DOMAIN" "target"
                require_dom_running "$SOURCE_URI" "$SOURCE_DOMAIN" "source"
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
                die "baseline full sync for external-snapshot testing failed (see $RUN_LOG) -- aborting stage 4$(bench_sync_hint)"
        fi

        local target_path_before=""
        if [ "$DRY_RUN" != yes ]; then
                target_path_before="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
        fi

        local snap_name="vmsync-bench-extsnap-$$"
        local overlay_path=""
        if [ "$DRY_RUN" != yes ]; then
                log "creating an external, disk-only snapshot '$snap_name' on source disk $TAMPER_DISK_DEV"
                virsh_uri "$SOURCE_URI" snapshot-create-as --domain "$SOURCE_DOMAIN" --name "$snap_name" \
                        --diskspec "${TAMPER_DISK_DEV},snapshot=external" --disk-only --atomic \
                        || die "failed to create external snapshot '$snap_name' on $SOURCE_DOMAIN -- aborting stage 4 (--atomic means either it's fully created or fully rolled back; nothing should be left half-done)"
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
                        # normally -- a better outcome than the one this stage
                        # was originally written to expect. Treating "the
                        # fallback wasn't needed" as "the fallback is broken"
                        # is what this check used to do, and it made a healthy
                        # run report a regression.
                        #
                        # The assertions that DO still apply here -- the sync
                        # succeeded, the snapshot was genuinely visible to
                        # vmsync, and the target path did not drift -- are all
                        # checked above, which they previously were not: they
                        # sat behind the tolerance-log grep and were skipped
                        # entirely whenever it didn't match.
                        log "   PASS: synced and verified with the external snapshot present; this libvirt permitted the checkpoint, so the tolerance path was not exercised (vmsync_external_snapshot_count=$snap_count, target path unchanged)"
                        results_row "$CSV" ext-snapshot during-result 0 "" "" "" "" "" "PASS synced+verified, checkpoint not blocked by this libvirt, count=$snap_count"
                fi
        fi

        log "--- removing the external snapshot (blockcommit --active --pivot, then metadata cleanup) ---"
        if [ "$DRY_RUN" != yes ]; then
                virsh_uri "$SOURCE_URI" blockcommit "$SOURCE_DOMAIN" "$TAMPER_DISK_DEV" --active --pivot --wait \
                        || die "blockcommit --active --pivot failed for $SOURCE_DOMAIN/$TAMPER_DISK_DEV -- STOP: the source domain's disk chain may now be in an inconsistent state, inspect it by hand (virsh -c $SOURCE_URI blockjob $SOURCE_DOMAIN $TAMPER_DISK_DEV) before continuing"
                virsh_uri "$SOURCE_URI" snapshot-delete --domain "$SOURCE_DOMAIN" --snapshotname "$snap_name" --metadata \
                        || warn "removing snapshot metadata for '$snap_name' failed -- the disk merge itself (blockcommit) already succeeded, so this is just stale bookkeeping, but check 'virsh -c $SOURCE_URI snapshot-list --domain $SOURCE_DOMAIN'"
                if [ -n "$overlay_path" ]; then
                        maybe_ssh_cmd "$SOURCE_LOCAL" "$SOURCE_HOST" rm -f "$overlay_path" \
                                || warn "could not remove the now-unused overlay file $overlay_path on $SOURCE_HOST -- harmless (blockcommit --pivot already stopped referencing it) but worth cleaning up by hand"
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
#   5b (best-effort): a timed disruption aimed at making the actual
#   redefine call fail outright, to check that DefineDomain's rollback
#   genuinely restores the target's prior definition. This is a race
#   against a live, variable-duration sync with no way to synchronize on
#   the exact right instant from outside the vmsync process, so a missed
#   window is scored as a SKIP, never a FAIL -- only a landed disruption
#   proves anything either way.
#
# Not part of the default --stages list (see usage()): unlike every other
# stage, 5b temporarily manipulates TARGET_HOST's own iptables rules over
# the same SSH connection it's about to disrupt. The removal is scheduled
# to fire on the target host itself so it doesn't depend on a second ssh
# call the rule would also block, and this script additionally tries an
# explicit cleanup afterward as a backstop -- but only run this stage
# against the same fully disposable test host this whole harness already
# requires (see the SAFETY note at the top of this file).

# domain_definition_xml URI DOMAIN -> prints DOMAIN's current XML, or
# nothing (empty string, exit 0) when it doesn't exist at all -- callers
# that need to tell "genuinely undefined" apart from "query failed" should
# check domain_exists first.
domain_definition_xml() {
        virsh_uri "$1" dumpxml "$2" 2>/dev/null
}

stage_define_domain_uuid_collision() {
        log "--- Stage 5a: DefineDomain uuid-collision retry ---"

        if [ "$DRY_RUN" != yes ]; then
                require_dom_shutoff_or_absent "$TARGET_URI" "$TARGET_DOMAIN" "target"
        fi

        bench_sync define-uuid-collision baseline -reinit
        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                die "baseline full sync for DefineDomain uuid-collision testing failed (see $RUN_LOG) -- aborting stage 5a$(bench_sync_hint)"
        fi

        if [ "$DRY_RUN" = yes ]; then
                bench_sync define-uuid-collision trigger -reinit
                return 0
        fi

        local src_uuid target_uuid_before
        src_uuid="$(domain_uuid "$SOURCE_URI" "$SOURCE_DOMAIN")" || die "could not read source domain UUID via $SOURCE_URI: $VIRSH_ERR"
        target_uuid_before="$(domain_uuid "$TARGET_URI" "$TARGET_DOMAIN")" || die "could not read target domain UUID via $TARGET_URI: $VIRSH_ERR"
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
                || die "could not undefine target domain '$TARGET_DOMAIN' to free its uuid for the collision test -- aborting stage 5a"

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
                || die "failed to define throwaway uuid-collision domain '$collision_name' on target -- aborting stage 5a"

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

stage_define_domain_rollback() {
        log "--- Stage 5b: DefineDomain rollback-on-failure (best-effort, timing-dependent) ---"

        if [ "$DRY_RUN" = yes ]; then
                log "   (dry run: nothing to time or disrupt)"
                bench_sync define-rollback baseline -reinit
                bench_sync define-rollback trigger -reinit
                return 0
        fi

        require_dom_shutoff_or_absent "$TARGET_URI" "$TARGET_DOMAIN" "target"

        bench_sync define-rollback baseline -reinit
        if [ "$RUN_RC" != 0 ]; then
                die "baseline full sync for DefineDomain rollback testing failed (see $RUN_LOG) -- aborting stage 5b$(bench_sync_hint)"
        fi

        local xml_before
        xml_before="$(domain_definition_xml "$TARGET_URI" "$TARGET_DOMAIN")"
        [ -n "$xml_before" ] || die "could not capture target domain XML before the rollback test -- aborting stage 5b"

        local marker="Undefining domain on target system"
        local log_file="$RUN_DIR/logs/define-rollback.trigger.log"
        local prom_file="$RUN_DIR/prom/define-rollback.trigger.prom"
        RUN_LOG="$log_file"
        RUN_PROM="$prom_file"
        vmsync_common_args
        local args=("${VMSYNC_ARGS[@]}" -prometheus-textfile "$prom_file" -reinit)
        log "   $VMSYNC_BIN ${args[*]}"
        "$VMSYNC_BIN" "${args[@]}" >"$log_file" 2>&1 &
        local vmsync_pid=$!

        local max_wait="${DEFINE_ROLLBACK_WAIT_SECONDS:-300}"
        local seen=no
        SECONDS=0
        while [ "$SECONDS" -lt "$max_wait" ]; do
                if grep -q "$marker" "$log_file" 2>/dev/null; then
                        seen=yes
                        break
                fi
                kill -0 "$vmsync_pid" 2>/dev/null || break
                sleep 0.2
        done

        local ssh_port="${SSH_PORT:-22}"
        if [ "$seen" = yes ]; then
                log "   marker seen after ${SECONDS}s, disrupting target SSH reachability for 3s"
                ssh_host_cmd "$TARGET_HOST" \
                        "iptables -I INPUT -p tcp --dport ${ssh_port} -j REJECT --reject-with tcp-reset && (sleep 3; iptables -D INPUT -p tcp --dport ${ssh_port} -j REJECT --reject-with tcp-reset) </dev/null >/dev/null 2>&1 &" \
                        || warn "could not insert the disruption rule on $TARGET_HOST -- letting this run finish normally instead"
        else
                warn "SKIP: never saw '$marker' within ${max_wait}s (or vmsync exited first) -- too fast (or failed earlier) to time the disruption; raise DEFINE_ROLLBACK_WAIT_SECONDS in $CONF if your test VM's full sync legitimately takes longer than that"
        fi

        wait "$vmsync_pid" 2>/dev/null
        RUN_RC=$?

        # Backstop in case the self-scheduled removal above didn't fire (e.g.
        # this script's own ssh_host_cmd got cut before it could background
        # the removal) -- never fatal, and idempotent: -D on an already-
        # absent rule just errors harmlessly.
        ssh_host_cmd "$TARGET_HOST" "iptables -D INPUT -p tcp --dport ${ssh_port} -j REJECT --reject-with tcp-reset" >/dev/null 2>&1 || true

        if [ "$seen" != yes ]; then
                results_row "$CSV" define-rollback result 0 "" "" "" "" "" "SKIP disruption window missed"
                return 0
        fi

        if [ "$RUN_RC" = 0 ]; then
                warn "FAIL (inconclusive): the disruption landed but vmsync still exited 0 -- either the redefine completed before the block took effect, or it tolerated the disruption; see $log_file"
                results_row "$CSV" define-rollback result 1 "" "" "" "" "" "FAIL disruption landed but run still succeeded"
                return 0
        fi

        local xml_after
        xml_after="$(domain_definition_xml "$TARGET_URI" "$TARGET_DOMAIN")"
        if [ -z "$xml_after" ]; then
                warn "FAIL: target domain is gone/undefined after the disrupted run -- rollback did not restore it (see $log_file)"
                results_row "$CSV" define-rollback result 1 "" "" "" "" "" "FAIL target left undefined, rollback did not restore it"
        elif [ "$xml_after" = "$xml_before" ]; then
                log "   PASS: run failed as expected (exit=$RUN_RC) and the target's definition matches what it was before -- rollback held"
                results_row "$CSV" define-rollback result 0 "" "" "" "" "" "PASS rollback restored prior definition"
        else
                warn "FAIL: run failed (exit=$RUN_RC) but the target's definition differs from before -- rollback did not fully restore it (see $log_file)"
                results_row "$CSV" define-rollback result 1 "" "" "" "" "" "FAIL target definition differs from before the run"
        fi
        return 0
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
		# NOTES must not contain a comma (plain CSV, see results_row).
		results_row "$CSV" "$scenario" "${label// /_}" 1 "" "" "" "" "" "FAIL ${detail//,/;}"
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
	ssh_host_cmd "$TARGET_HOST" "$TARGET_VMSYNC_BIN" \
		-update-role target -target-uri qemu:///system -target-domain "$TARGET_DOMAIN" \
		>/dev/null 2>&1 || return 1
	[ "$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)" = target ]
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

# warn_target_still_promoted says what has to be done by hand, in full.
#
# Worth being explicit rather than terse: a target left `promoted` refuses
# EVERY later sync, including the next stage's baseline and every scheduled
# run in the estate, and the symptom is a sync failing for reasons that say
# nothing about a promotion.
warn_target_still_promoted() {
	warn "$TARGET_DOMAIN on $TARGET_HOST is still marked promoted. Every sync into it -- this harness's later stages included -- will be refused until that is cleared. Run this on $TARGET_HOST:  ${TARGET_VMSYNC_BIN:-vmsync} -update-role target -target-uri qemu:///system -target-domain $TARGET_DOMAIN"
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
	promoted | source)
		warn "$stage: the target's replication_role is still '$role' after resetting it, so every sync into it would be refused -- skipping."
		warn_target_still_promoted
		results_row "$CSV" "$stage" skipped 0 "" "" "" "" "" "SKIPPED target role is $role"
		return 1
		;;
	esac
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
		require_dom_shutoff_or_absent "$TARGET_URI" "$TARGET_DOMAIN" "target"
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
		die "baseline full sync failed (see $RUN_LOG) -- aborting stage 6 before anything is promoted$(bench_sync_hint)"
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
	vmsync_on_host "$TARGET_HOST" no "$TARGET_VMSYNC_BIN" "$sc" update-role-back \
		-update-role target -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN"
	if [ "$RUN_RC" = 0 ]; then fo_ok=0; else fo_ok=1; fi
	fo_check "$sc" "-update-role=target succeeds" "$fo_ok" "exit $RUN_RC see $RUN_LOG"

	if [ "$DRY_RUN" != yes ]; then
		local role_back fence_back promoted_back
		role_back="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" replication_role)"
		if [ "$role_back" = target ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the target is a target again" "$fo_ok" "got '$role_back'"

		fence_back="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" fence_source)"
		if [ -z "$fence_back" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "clearing the role takes the fence with it" "$fo_ok" \
			"fence_source is still '$fence_back'"

		promoted_back="$(vmsync_meta_field "$TARGET_URI" "$TARGET_DOMAIN" promoted_at)"
		if [ -z "$promoted_back" ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "clearing the role takes the promotion record with it" "$fo_ok" \
			"promoted_at is still '$promoted_back'"

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
	printf '{\n  "report_interval_seconds": 60,\n  "poll_wait_seconds": 30,\n  "schedule": [\n    { "vm": "%s", "interval_seconds": 86400, "enabled": false, "profile": {} }\n  ]\n}\n' "$1"
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
	# setsid so the agent survives this ssh session closing, which it
	# otherwise would not: without it the remote shell's exit takes the
	# whole process group with it and the fence sweep never happens.
	run_shell_on "$host" "$is_local" \
		"setsid nohup '$bin' --standalone '$dir/schedule.json' --state-dir '$dir' --vmsync-path '$vmsync_there' --debug >'$dir/agent.log' 2>&1 < /dev/null & echo \$!"
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
	# Where each agent finds vmsync on its OWN host. SOURCE_VMSYNC_BIN falls
	# back to VMSYNC_BIN, which is correct in the common SOURCE_LOCAL=yes
	# setup where this harness runs on the source host itself.
	local src_vmsync="${SOURCE_VMSYNC_BIN:-$VMSYNC_BIN}"

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
		|| die "cannot query the source domain's state${VIRSH_ERR:+: $VIRSH_ERR}"
	if [ "$src_state" != running ]; then
		warn "source domain '$SOURCE_DOMAIN' is '$src_state', not running -- skipping stage 7. A fence only acts on a running domain, and this harness does not start the source itself."
		results_row "$CSV" "$sc" skipped 0 "" "" "" "" "" "SKIPPED source not running"
		return 0
	fi

	require_dom_shutoff_or_absent "$TARGET_URI" "$TARGET_DOMAIN" "target"
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

		maybe_ssh_cmd "$SOURCE_LOCAL" "$SOURCE_HOST" "$src_vmsync" \
			-update-role none -target-uri "$local_uri" -target-domain "$SOURCE_DOMAIN" \
			>/dev/null 2>&1 \
			|| warn "could not clear the source's replication role -- do it by hand: vmsync -update-role none -target-uri $local_uri -target-domain $SOURCE_DOMAIN"
		ssh_host_cmd "$TARGET_HOST" "$TARGET_VMSYNC_BIN" \
			-update-role target -target-uri "$local_uri" -target-domain "$TARGET_DOMAIN" \
			>/dev/null 2>&1 \
			|| warn "could not put the target back to role=target -- do it by hand, or every later sync into it will be refused"

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
		die "baseline full sync failed (see $RUN_LOG) -- aborting stage 7 before anything is promoted$(bench_sync_hint)"
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
	if [ "$state" = shutoff ]; then
		local src_role
		src_role="$(vmsync_meta_field "$SOURCE_URI" "$SOURCE_DOMAIN" replication_role)"
		if [ "$src_role" = paused ]; then fo_ok=0; else fo_ok=1; fi
		fo_check "$sc" "the fenced source is left paused, not merely stopped" "$fo_ok" \
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

		case "$ledger" in
		*'"state": "done"'* | *'"state":"done"'*) fo_ok=0 ;;
		*) fo_ok=1 ;;
		esac
		fo_check "$sc" "the ledger records the fence as done" "$fo_ok" "ledger: $(printf '%s' "$ledger" | tr -d '\n' | cut -c1-200)"
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
NON_MATRIX_SCENARIOS='^(verify-|reinit-after-failures$|ext-snapshot$|define-uuid-collision$|define-rollback$|failover$|fence-agent$)'

# stage_pattern STAGE -> the regex matching that stage's scenario column.
# --- Stage 8: verify after a long incremental chain --------------------------
#
# Everything else here verifies a replica that was built moments ago by a
# single -reinit. This builds one the way a real deployment does -- dozens of
# incremental syncs, each carrying a real guest write -- and only then asks
# whether corruption is still detectable. Deliberately opt-in: it is the
# longest stage by a wide margin.
stage_verify_long() {
	log "=== Stage 8: -verify after a long incremental chain ==="

	local copies="${VERIFY_LONG_COPIES}" attempts="${VERIFY_LONG_ATTEMPTS}" recopies="${VERIFY_LONG_RECOPIES}"

	if [ "$DRY_RUN" != yes ]; then
		# The chain is only meaningful if the guest is writing between
		# copies, and that needs a RUNNING guest with a usable agent.
		# Without it this stage would spend an hour proving that vmsync can
		# copy nothing twenty times -- so it skips rather than reporting a
		# green result that verified nothing.
		local src_state
		src_state="$(dom_state "$SOURCE_URI" "$SOURCE_DOMAIN")" || src_state="unknown"
		if [ "$src_state" != running ]; then
			warn "SKIP stage verify-long: source domain '$SOURCE_DOMAIN' is '$src_state', but this stage needs a running guest to dirty its own disk between copies"
			results_row "$CSV" verify-long precondition "" "" "" "" "" "" "SKIP source domain not running"
			return 0
		fi
		if [ "$GUEST_DIRTY" = yes ] && ! guest_exec_available; then
			warn "SKIP stage verify-long: $GUEST_EXEC_WHY"
			results_row "$CSV" verify-long precondition "" "" "" "" "" "" "SKIP guest-exec unavailable"
			return 0
		fi
		require_dom_shutoff_or_absent "$TARGET_URI" "$TARGET_DOMAIN" "target"
	fi

	bench_sync verify-long baseline -reinit
	if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
		die "baseline full sync for verify-long failed (see $RUN_LOG) -- aborting stage 8$(bench_sync_hint)"
	fi

	local target_path="" vsize=0
	if [ "$DRY_RUN" != yes ]; then
		target_path="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
		[ -n "$target_path" ] || die "could not resolve target disk path for dev='$TAMPER_DISK_DEV' -- check TAMPER_DISK_DEV in $CONF"
		vsize="$(target_virtual_size "$target_path")" || true
		[ -n "$vsize" ] && [ "$vsize" -gt 0 ] 2>/dev/null \
			|| die "could not read the virtual size of $target_path on $TARGET_HOST via qemu-img info"
	fi

	verify_long_chain "$copies" chain
	verify_long_assert_chain "$copies"

	local attempt=1 mode
	while [ "$attempt" -le "$attempts" ]; do
		# Cycle the modes so a long run covers all three rather than
		# hammering one, and so which mode meets which random offset varies
		# with the seed rather than being fixed by position.
		case $((attempt % 3)) in
		1) mode=compare ;;
		2) mode=fast ;;
		*) mode=online ;;
		esac
		log "--- verify-long attempt $attempt/$attempts, mode=$mode ---"
		verify_mode_subtest "$target_path" "$vsize" "$mode" verify-long "attempt-${attempt}-${mode}"
		attempt=$((attempt + 1))
		[ "$attempt" -le "$attempts" ] && verify_long_chain "$recopies" recopy
	done
	return 0
}

# verify_long_chain N LABEL -- N incremental copies, each preceded by a real
# guest write. Sets VERIFY_LONG_MOVED to how many of them actually transferred
# something.
VERIFY_LONG_MOVED=0
verify_long_chain() {
	local n="$1" label="$2" i=1 moved
	VERIFY_LONG_MOVED=0
	while [ "$i" -le "$n" ]; do
		if [ "$DRY_RUN" != yes ] && [ "$GUEST_DIRTY" = yes ]; then
			guest_dirty || warn "guest write $i/$n failed -- this copy will carry less (or nothing) than intended"
		fi
		bench_sync verify-long "${label}-${i}"
		if [ "$DRY_RUN" != yes ]; then
			if [ "$RUN_RC" != 0 ]; then
				die "incremental copy ${label}-${i} of the verify-long chain failed (see $RUN_LOG) -- the chain is broken, so nothing after it would mean anything"
			fi
			moved="$(prom_sum "$RUN_PROM" vmsync_transferred_bytes)"
			[ "$moved" -gt 0 ] && VERIFY_LONG_MOVED=$((VERIFY_LONG_MOVED + 1))
		fi
		i=$((i + 1))
	done
	return 0
}

# verify_long_assert_chain N -- the chain has to have carried real data, or the
# verification below is being run against a replica no different from the
# baseline and the stage proves nothing.
verify_long_assert_chain() {
	local n="$1"
	if [ "$DRY_RUN" = yes ]; then
		results_row "$CSV" verify-long chain-result DRYRUN "" "" "" "" "" "SKIP dry run"
		return 0
	fi
	if [ "$VERIFY_LONG_MOVED" -eq 0 ]; then
		warn "FAIL: none of the $n incremental copies transferred a single byte -- the guest is not dirtying its disk, so this chain is $n no-ops and the verification below would test nothing. Check GUEST_DIRTY_PATH is writable in the guest and that dd reached the disk (conv=fsync)."
		results_row "$CSV" verify-long chain-result 1 "" "" "" "" "" "FAIL chain carried no data"
	else
		log "   PASS: $VERIFY_LONG_MOVED of $n incremental copies carried real data"
		results_row "$CSV" verify-long chain-result 0 "" "" "" "" "" "PASS chain carried real data"
	fi
}

stage_pattern() {
	case "$1" in
	# Anchored to the four named sub-tests rather than a bare ^verify- , so a
	# run doing both stages does not fold stage 8's rows into stage 2's
	# verdict.
	verify) printf '^verify-(guard|compare|fast|online)$' ;;
	verify-long) printf '^verify-long$' ;;
	reinit) printf '^reinit-after-failures$' ;;
	snapshot) printf '^ext-snapshot$' ;;
	define) printf '^define-(uuid-collision|rollback)$' ;;
	failover) printf '^failover$' ;;
	fence-agent) printf '^fence-agent$' ;;
	*) printf '$^' ;; # matches nothing
	esac
}

# stage_verdict STAGE -> "STATUS<TAB>DETAIL". Never fails; an unknown stage
# reports SKIPPED rather than inventing a result.
stage_verdict() {
	local stage="$1"
	if [ "$stage" = matrix ]; then
		awk -F, -v non="$NON_MATRIX_SCENARIOS" '
			NR>1 && $1 !~ non { n++; if ($3 != "0" && $3 != "DRYRUN") f++ }
			END {
				if (n == 0)      printf "SKIPPED\tnothing ran"
				else if (f > 0)  printf "FAIL\t%d of %d vmsync runs exited non-zero", f, n
				else             printf "PASS\t%d vmsync runs, all exited 0", n
			}' "$CSV"
		return 0
	fi
	awk -F, -v pat="$(stage_pattern "$stage")" '
		NR>1 && $1 ~ pat {
			if      ($9 ~ /^FAIL/) f++
			else if ($9 ~ /^PASS/) p++
			else if ($9 ~ /^SKIP/) s++
		}
		END {
			if (f > 0)                  printf "FAIL\t%d of %d checks failed", f, f+p
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
	local stage="$1" verdict status detail
	verdict="$(stage_verdict "$stage")"
	status="${verdict%%	*}"
	detail="${verdict#*	}"

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
                awk -F, 'NR>1 && $1 ~ /^verify-(guard|baseline|compare|fast|online)/ { printf "| %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $9 }' "$CSV"
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
                        echo "_not run (opt in with \`--stages verify-long\`; it makes ~$VERIFY_LONG_COPIES incremental copies before it starts)_"
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
                awk -F, 'NR>1 && ($1=="define-uuid-collision" || $1=="define-rollback") { printf "| %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $9 }' "$CSV"
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
for s in "${stage_list[@]}"; do
        case "$s" in
        matrix) stage_matrix ;;
        verify) stage_verify_tamper ;;
        verify-long) stage_verify_long ;;
        reinit) stage_reinit_after_failures ;;
        snapshot) stage_external_snapshot ;;
        define) stage_define_domain ;;
        failover) stage_failover ;;
        fence-agent) stage_fence_agent ;;
        *) die "unknown stage '$s' in --stages (want matrix,verify,verify-long,reinit,snapshot,define,failover,fence-agent)" ;;
        esac
        announce_stage_verdict "$s"
done

generate_report

# Last, and it decides the exit status: a harness that always exits 0 cannot
# be used by anything that would act on the answer, and "did that run work"
# should not require reading a report to find out.
final_verdict
