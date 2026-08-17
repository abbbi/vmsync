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
  --stages LIST           comma-separated subset of: matrix,verify,reinit,
                           snapshot,define (default: matrix,verify,reinit,
                           snapshot, in that order -- define is opt-in, see
                           below)
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

log "vmsync benchmark harness -- run id $RUN_ID"
log "config: $CONF"
log "scenarios: $SCENARIOS"
log "results directory: $RUN_DIR"
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
        local scenario="$1" phase="$2" lifetime="$3"
        shift 3
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
        if [ "${lifetime}" -gt 0 ]; then
                start_lifetime="$(date +%s)"
                log "I have been tasked to stop my run after $lifetime seconds"
                "$VMSYNC_BIN" "${args[@]}" >"$log_file" 2>&1 &
                RUN_PID=$!
                while true; do
                    end_lifetime="$(date +%s)"
                    if ! kill -0 $RUN_PID > /dev/null 2>&1; then
                        wait $RUN_PID
                        RUN_RC=$?
                        break
                    elif [ "$((end_lifetime-start_lifetime))" -ge "${lifetime}" ]; then
                        log "Now killing current run"
                        kill -SIGTERM ${RUN_PID}
                        [ $? -ne 0 ] && log "Failed to kill current run"
                    fi
                    sleep .5
                done
        else
                log "Standard vmsync run"
                "$VMSYNC_BIN" "${args[@]}" >"$log_file" 2>&1
                RUN_RC=$?
        fi
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
                                        run_vmsync "$name" full 0 -reinit "${SCEN_ARGS[@]}"
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
                                        run_vmsync "$name" incremental 0 "${SCEN_ARGS[@]}"
                                done
                        done
                done
        done
        return 0
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
        run_vmsync verify-baseline full 0 -reinit
        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                die "baseline full sync for verify testing failed (see $RUN_LOG) -- aborting stage 2"
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

        local mode
        for mode in compare fast online; do
                log "--- verify=$mode: tampering ${target_path:-<unresolved in --dry-run>} at offset $TAMPER_OFFSET, length $TAMPER_LENGTH ---"

                if [ "$DRY_RUN" != yes ]; then
                        require_dom_shutoff "$TARGET_URI" "$TARGET_DOMAIN" "target"
                        ssh_host_cmd "$TARGET_HOST" qemu-io -f qcow2 \
                                -c "'write -P 0xAA ${TAMPER_OFFSET} ${TAMPER_LENGTH}'" "'${target_path}'" \
                                || die "failed to inject test corruption into $target_path on $TARGET_HOST -- refusing to continue this sub-test"
                fi

                run_vmsync "verify-${mode}" tamper 0 "-verify=$mode"

                if [ "$DRY_RUN" = yes ]; then
                        :
                elif [ "$RUN_RC" != 0 ]; then
                        log "   PASS: -verify=$mode correctly reported a mismatch (exit=$RUN_RC)"
                        results_row "$CSV" "verify-${mode}" tamper-result 0 "" "" "" "" "" "PASS mismatch detected"
                else
                        warn "FAIL: -verify=$mode did NOT report a mismatch after target-side tampering (exit=0) -- see $RUN_LOG. Note: -verify=online can legitimately discard an in-window mismatch as inconclusive if the guest happened to rewrite that exact region during the compare -- see README.md before treating this as a confirmed bug."
                        results_row "$CSV" "verify-${mode}" tamper-result 1 "" "" "" "" "" "FAIL mismatch NOT detected"
                fi

                # Heal with a FULL resync unconditionally, regardless of the
                # outcome above, before testing the next mode. An incremental
                # sync would NOT fix this: it only re-copies blocks the source's
                # own dirty bitmap says changed, and the source never wrote to
                # the offset we just tampered with on the target -- only -reinit
                # (which re-copies everything) is guaranteed to overwrite it.
                log "   healing target with a full resync (-reinit)"
                run_vmsync "verify-${mode}" heal 0 -reinit
                if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                        die "heal-after-tamper resync for -verify=$mode did not succeed (see $RUN_LOG) -- STOP and inspect $target_path by hand before trusting this target replica or continuing"
                fi
        done
        return 0
}

# --- Stage 3: -reinit-after-failures -----------------------------------------

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
        run_vmsync reinit-after-failures baseline 0 -reinit -compress
        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                die "baseline full sync for reinit-after-failures testing failed (see $RUN_LOG) -- aborting stage 3"
        fi

        log "inducing $n consecutive failures against the real target"
        local i
        for i in $(seq 1 "$n"); do
                set +e
                log "Running consecutive run $i"
                run_vmsync reinit-after-failures "induce-$i" 5 -source-domain "${SOURCE_DOMAIN}" -compress "-reinit-after-failures=$n" 2>&1
                set -e
                if [ "$RUN_RC" = 0 ] && [ "$DRY_RUN" != yes ]; then
                        warn "induced-failure run #$i unexpectedly succeeded (exit=0) -- check $RUN_LOG, this test's assumptions may not hold in your environment"
                fi
        done

        log "running a real, correct sync with -reinit-after-failures=$n -- expecting it to force a full resync instead of the incremental one it would otherwise do"
        run_vmsync reinit-after-failures trigger 0 -source-domain "${SOURCE_DOMAIN}" -compress "-reinit-after-failures=$n"

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
        run_vmsync ext-snapshot baseline 0 -reinit
        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                die "baseline full sync for external-snapshot testing failed (see $RUN_LOG) -- aborting stage 4"
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

        log "--- syncing while the external snapshot exists (expect: checkpoint creation tolerantly blocked, sync+verify still succeed) ---"
        run_vmsync ext-snapshot during-snapshot 0 -verify=fast
        if [ "$DRY_RUN" != yes ]; then
                if [ "$RUN_RC" != 0 ]; then
                        warn "FAIL: sync+verify while an external snapshot existed did not succeed (exit=$RUN_RC) -- see $RUN_LOG"
                        results_row "$CSV" ext-snapshot during-result 1 "" "" "" "" "" "FAIL sync did not succeed with snapshot present"
                elif ! grep -q 'checkpoint creation blocked by an existing external snapshot' "$RUN_LOG" 2>/dev/null; then
                        warn "FAIL: sync+verify succeeded, but never logged the expected checkpoint-blocked-by-snapshot tolerance path -- see $RUN_LOG (did the snapshot actually take effect on this disk?)"
                        results_row "$CSV" ext-snapshot during-result 1 "" "" "" "" "" "FAIL checkpoint-blocked path not observed"
                else
                        local snap_count
                        snap_count="$(prom_sum "$RUN_PROM" vmsync_external_snapshot_count)"
                        local target_path_during=""
                        target_path_during="$(disk_source_path "$TARGET_URI" "$TARGET_DOMAIN" "$TAMPER_DISK_DEV")" || true
                        if [ -n "$target_path_before" ] && [ "$target_path_during" != "$target_path_before" ]; then
                                warn "FAIL: target disk path changed while the snapshot existed (before='$target_path_before' during='$target_path_during') -- RootSource-based naming should have kept this stable"
                                results_row "$CSV" ext-snapshot during-result 1 "" "" "" "" "" "FAIL target path drifted during snapshot"
                        else
                                log "   PASS: synced and verified correctly with the external snapshot present (vmsync_external_snapshot_count=$snap_count, target path unchanged)"
                                results_row "$CSV" ext-snapshot during-result 0 "" "" "" "" "" "PASS synced+verified, path stable, count=$snap_count"
                        fi
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
        run_vmsync ext-snapshot after-snapshot 0 -verify=fast
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

        run_vmsync define-uuid-collision baseline 0 -reinit
        if [ "$RUN_RC" != 0 ] && [ "$DRY_RUN" != yes ]; then
                die "baseline full sync for DefineDomain uuid-collision testing failed (see $RUN_LOG) -- aborting stage 5a"
        fi

        if [ "$DRY_RUN" = yes ]; then
                run_vmsync define-uuid-collision trigger 0 -reinit
                return 0
        fi

        local src_uuid target_uuid_before
        src_uuid="$(domain_uuid "$SOURCE_URI" "$SOURCE_DOMAIN")" || die "could not read source domain UUID via $SOURCE_URI: $VIRSH_ERR"
        target_uuid_before="$(domain_uuid "$TARGET_URI" "$TARGET_DOMAIN")" || die "could not read target domain UUID via $TARGET_URI: $VIRSH_ERR"
        [ "$target_uuid_before" = "$src_uuid" ] || warn "target UUID ($target_uuid_before) doesn't match source UUID ($src_uuid) right after a fresh -reinit baseline -- unexpected, but continuing"

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

        run_vmsync define-uuid-collision trigger 0 -reinit

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
                run_vmsync define-rollback baseline 0 -reinit
                run_vmsync define-rollback trigger 0 -reinit
                return 0
        fi

        require_dom_shutoff_or_absent "$TARGET_URI" "$TARGET_DOMAIN" "target"

        run_vmsync define-rollback baseline 0 -reinit
        if [ "$RUN_RC" != 0 ]; then
                die "baseline full sync for DefineDomain rollback testing failed (see $RUN_LOG) -- aborting stage 5b"
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

# --- report ------------------------------------------------------------------

generate_report() {
        local report="$RUN_DIR/report.md"
        {
                echo "# vmsync benchmark report"
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
                awk -F, 'NR>1 && $1 ~ /^verify-/ { printf "| %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $9 }' "$CSV"
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
        reinit) stage_reinit_after_failures ;;
        snapshot) stage_external_snapshot ;;
        define) stage_define_domain ;;
        *) die "unknown stage '$s' in --stages (want matrix,verify,reinit,snapshot,define)" ;;
        esac
done

generate_report
log "done. Results: $RUN_DIR"
