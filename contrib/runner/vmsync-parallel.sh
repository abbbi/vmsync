#!/usr/bin/env bash

# Parallel vmsync launcher
# Written in 2026 by Orsiris de Jong <ozy@netpower.fr> for vmsync by Michael Ablassmeier <abi@grinser.de>


#SCRIPT_BUILD=2026082401
LOG_FILE=/var/log/vmsync_parallel
TARGET_VM_PATH=/vm_data
VMSYNC=/usr/local/bin/vmsync

COMMAND_TO_LIST_VMS="virsh list --all --name"
IGNORE_VM_LIST=("someVMname" "othervm.local")

# Port accounting, so parallel replication (REPLICATION_CONCURRENCY > 1) never hands two
# VMs overlapping port ranges:
#   - Target: vmsync uses one real qemu-nbd port per disk (TargetNBDPort + i). With
#     -compress/-netbuffer, it *also* uses one bridge port per disk, placed right after all
#     the real ones (targetBridgePort := targetPort + len(qcowDisks), see
#     cmd/vmsync/main.go) -- so an N-disk VM needs 2*N ports when bridging is on, not N+1.
#   - Source: there is only ONE shared libvirt backup NBD export per VM (not per disk), plus
#     one more fixed bridge port when bridging is on (SourceNBDPort + 1) -- 1 or 2 ports
#     total, regardless of disk count.

SOURCE_BASE_PORT=10809
DESTINATION_HOST=hv02.replica.local
DESTINATION_SSH_KEY=/root/.ssh/hyper02p
DESTINATION_BASE_PORT=20809
LOCK_DIR="/run/vmsync.lock"
PID_FILE="${LOCK_DIR}/vmsync.pid"
SCRIPT_PID=$$

# Transfer options (need /usr/local/bin/vmsync-bridge-helper on target)
# Compress algo zstd or s2
COMPRESS_ALGO=s2
# Compress level 0-19 for zstd and default,better,best for s2. Nothing means disabled
COMPRESS_LEVEL=better
NETBUFFER=128k,1G

# Optional bandwidth limit on replication interface (in bps, 0 means no limit)
REPLICATION_INTERFACE=wg_replica0
MAX_BANDWIDTH=850M

# Reinitialize failed sync options (0 means no reinit after n failures)
# This is a dangerous option since it would overwrite target after 5 failures
# You'd lose all modifications on target. Only enable if you know exactly what you're doing
REINIT_AFTER_FAILURES=5

# Optional prometheus metrics
PROMETHEUS_TEXTFILE_PATH=/var/lib/node_exporter/textfile_collector

## Parallel task execution settings
PROGRAM=vmsync_parallel
REPLICATION_CONCURRENCY=4
SOFT_TIMEOUT_PER_VM=1800
HARD_TIMEOUT=86400 # Keep 0 for initial replica
SLEEP_TIME=.5
KEEP_LOGGING=1800
DEBUG=false


usage() {
        echo "Usage: $0 [--vm=NAME] [--reinit] [--verify] [--usage]"
        echo "  --vm=NAME         Restrict replication to this VM. Repeatable (--
vm=web01 --vm=web02) to whitelist several. Omit entirely to replicate every VM $COMMAND_TO_LIST_VMS lists (minus IGNORE_VM_LIST)."
        echo "  --reinit          Force vmsync's own -reinit (full resync from scratch) for every VM this run touches. Same destructive caveats as vmsync's -reinit itself, Off by default"
        echo "  --verify          Force vmsync's own -verify (check data integrity) for every VM this run touches. Off by default"
        echo "  --usage/--help    Show this help message and exit."

}
VM_WHITELIST=()
USE_REINIT=false
VERIFY=false

parse_args() {
        while [ $# -gt 0 ]; do
                case "$1" in
                        --vm=*)
                                VM_WHITELIST+=("${1#--vm=}")
                                ;;
                        --reinit)
                                USE_REINIT=true
                                ;;
                        --verify)
                                VERIFY=true
                                ;;
                        --usage|--help)
                                usage
                                exit 0
                                ;;
                        *)
                                log "Unknown argument [$1], ignoring" "WARN"
                                ;;
                esac
                shift
        done
}



##############################################################


if [ -w /tmp ]; then
        RUN_DIR=/tmp
elif [ -w /var/tmp ]; then
        RUN_DIR=/var/tmp
else
        RUN_DIR=.
fi

log() {
    __log_line="${1}"
    __log_level="${2:-INFO}"

    if [ "${__log_level}" == "DEBUG" ] && [ "$DEBUG" != true ]; then
        return
    fi
    __log_line="${__log_level}: ${__log_line}"
    echo "${__log_line}"
    echo "${__log_line}" >> "${LOG_FILE}.log"
}

log_quit() {
    log "${1}" "${2}"
    log "Exiting script"
    exit 1
}

release_lock() {
    rm -rf "${LOCK_DIR}"
    # Cleanup temp files -- deliberately unquoted so the "*" actually globs;
    # quoting the whole path suppresses expansion and silently removes
    # nothing.
    rm -f $RUN_DIR/$PROGRAM.*.$SCRIPT_PID.$TSTAMP
}

acquire_lock() {
        if mkdir "${LOCK_DIR}" 2>/dev/null; then
                echo "${SCRIPT_PID}" > "${PID_FILE}"
        else
                pid="$(cat "${PID_FILE}" 2>/dev/null)"
                if [ -n "${pid}" ] && ps -p "${pid}" > /dev/null 2>&1; then
                        log "$(date): Another instance of $0 script already running (pid ${pid}). exiting"
                        exit 0
                fi
                log "$(date): There's a stale lock from $0 (pid ${pid:-unknown}). Removing it and taking over."
                # Actually clear and re-claim the lock -- a stale PID that's
                # merely logged (not overwritten) leaves every subsequent
                # invocation seeing that same dead PID and *also* concluding
                # "stale, proceed", with nothing to stop them all running
                # concurrently until whichever one most recently took this
                # path happens to exit.
                rm -rf "${LOCK_DIR}"
                if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
                        # Lost the race: another instance claimed the lock
                        # between our rm and mkdir. Back off instead of running
                        # alongside it.
                        log "$(date): Another instance of $0 claimed the lock while it was being cleared. exiting"
                        exit 0
                fi
                echo "${SCRIPT_PID}" > "${PID_FILE}"
        fi
        trap 'release_lock; exit $?' INT QUIT TERM EXIT
}

replicate() {
        # Preparing options
        opts=""
        ports_increase=1
        bridging_enabled=false
        if [ "${COMPRESS_ALGO}" == "zstd" ] || [ "${COMPRESS_ALGO}" == "s2" ]; then
                bridging_enabled=true
                opts="${opts} -compress=${COMPRESS_ALGO}"
                if [ -n "${COMPRESS_LEVEL}" ]; then
                        opts="${opts} -compress-level ${COMPRESS_LEVEL}"
                fi
        fi
        if [ "${NETBUFFER}" != "" ]; then
                bridging_enabled=true
                opts="${opts} -netbuffer=${NETBUFFER}"
        fi
        if [ "${REINIT_AFTER_FAILURES}" -gt 0 ]; then
                opts="${opts} -reinit-after-failures ${REINIT_AFTER_FAILURES}"
        fi
        if [ "${USE_REINIT}" == true ]; then
                opts="${opts} -reinit"
        fi
        if [ "${VERIFY}" == true ]; then
                opts="${opts} -verify=full"
                ports_increse=2
        fi

        # Generating list of vmsync commands to execute

        vm_count=0
        source_base_port_start=${SOURCE_BASE_PORT:-10809}
        destination_base_port_start=${DESTINATION_BASE_PORT:-20809}
        cmd_list=""
        vm_list=""
        for vm in $($COMMAND_TO_LIST_VMS); do
                # Only replicate VMs named via --vm (repeatable); an empty
                # whitelist means every VM virsh lists, same as before.
                if [ ${#VM_WHITELIST[@]} -gt 0 ] && [ $(ArrayContains "$vm" "${VM_WHITELIST[@]}") -eq 0 ]; then
                        continue
                fi
                if [ $(ArrayContains "$vm" "${IGNORE_VM_LIST[@]}") -eq 1 ]; then
                        log "$(date): Skipping replication of ${vm} since it is in the ignore VM list"
                        continue
                fi
                target_disk_path=""
                disk_count=0
                for disk in $(virsh domblklist "${vm}" --details | grep "file" | grep "disk" | awk '{print $3"="$4}'); do
                        #disk_name="$(echo "${disk}" | awk -F'=' '{print $1}')"
                        if [ -n "${TARGET_VM_PATH}" ]; then
                                src_disk_path="$(echo "${disk}" | awk -F'=' '{print $2}')"
                                if [ -z "${target_disk_path}" ]; then
                                        target_disk_path="${TARGET_VM_PATH}/$(basename "$(dirname "${src_disk_path}")")"
                                fi
                        fi
                        disk_count=$((disk_count+1))
                done

                prom=""
                if [ -n "${PROMETHEUS_TEXTFILE_PATH}" ]; then
                        prom="-prometheus-textfile=${PROMETHEUS_TEXTFILE_PATH}/vmsync_${vm}.prom"
                else
                        prom=""
                fi


                # Target: one real qemu-nbd port per disk; with bridging, one additional
                # bridge port per disk too (2*disk_count total). Source: one shared NBD
                # export for the whole VM, plus one more if bridging (1 or 2 total,
                # independent of disk_count). See the port-accounting comment up top.
                if [ "${bridging_enabled}" == true ]; then
                        destination_ports_needed=$((disk_count*2))
                        source_ports_needed=2
                else
                        destination_ports_needed=$disk_count
                        source_ports_needed=1
                fi
                source_base_port_end=$((source_base_port_start+source_ports_needed-1))
                destination_base_port_end=$((destination_base_port_start+destination_ports_needed-1))
                if [ -n "${TARGET_VM_PATH}" ]; then
                        tgt_path="-target-disk-path \"${target_disk_path}\""
                else
                        tgt_path=""
                fi
                log "$(date): Preparing replication ${vm} to ${target_disk_path} from ports ${source_base_port_start}-${source_base_port_end} to ports ${destination_base_port_start}-${destination_base_port_end}"
                vmsync_cmd="\"${VMSYNC}\" -source-domain \"${vm}\" -source-uri qemu:///system -target-uri \"qemu+ssh://${DESTINATION_HOST}/system\" -ssh-key \"${DESTINATION_SSH_KEY}\" ${tgt_path} -source-nbd-port ${source_base_port_start} -target-nbd-port ${destination_base_port_start} -start ${opts} ${prom} >> \"${LOG_FILE}_${vm}.log\" 2>&1"
                if [ -z "${cmd_list}" ]; then
                        vm_list="${vm_list}"
                        cmd_list="${vmsync_cmd}"
                else
                        vm_list="${vm_list};${vm}"
                        cmd_list="$cmd_list;${vmsync_cmd}"
                fi
                vm_count=$((vm_count+1))
                source_base_port_start=$((source_base_port_end+ports_increase))
                destination_base_port_start=$((destination_base_port_end+ports_increase))
        done

        log "$(date): Total number of VMs to replicate; ${vm_count}, launching ${REPLICATION_CONCURRENCY} tasks"

        # Actual replication.
        #
        # The trailing '"" "" "" "0;75"' fills positional arguments 15-17 (unused
        # here) so that argument 18, validExitCodes, can be set. 75 is vmsync's
        # EX_TEMPFAIL: it stood down without touching anything because another
        # vmsync already held that domain's lock. This script's own lock (see
        # the top of the file) exists to make that overlap benign, so it must
        # not be treated as an error -- without this, every legitimate overlap
        # is logged as a failure and can reach SendAlert.
        exectasks "${cmd_list}" "${PROGRAM}" false "${SOFT_TIMEOUT_PER_VM}" 0 0 0 false .5 "${KEEP_LOGGING}" true "" "" "${REPLICATION_CONCURRENCY}" "" "" "" "0;75"
        result=$?
        if [ $result -ne 0 ]; then
                log "$(date): Global replication state failed" "ERROR"
        else
                log "$(date): Global replication state success for ${vm_count} VMs"
        fi

}

SendAlert() {
        # Put whatever you want here
        :
}

_OFUNCTIONS_SPINNER="|/-\\"
spinner() {
        printf " [%c]  \b\b\b\b\b\b" "$_OFUNCTIONS_SPINNER"
        _OFUNCTIONS_SPINNER=${_OFUNCTIONS_SPINNER#?}${_OFUNCTIONS_SPINNER%%???}
        return 0

}

# Array to string converter, see http://stackoverflow.com/questions/1527049/bash-join-elements-of-an-array
# usage: joinString separaratorChar Array
joinString() {
        local IFS="$1"; shift; echo "$*";
}

IsInteger() {
        local value="${1}"

        if type expr > /dev/null 2>&1; then
                expr "$value" : '^[0-9]\{1,\}$' > /dev/null 2>&1
                if [ $? -eq 0 ]; then
                        echo 1
                else
                        echo 0
                fi
        else
                if [[ $value =~ ^[0-9]+$ ]]; then
                        echo 1
                else
                        echo 0
                fi
        fi
}

ArrayContains() {
        local needle="${1}"
        local haystack="${2}"
        local e

        if [ "$needle" != "" ] && [ "$haystack" != "" ]; then
                for e in "${@:2}"; do
                        if [ "$e" == "$needle" ]; then
                                echo 1
                                return
                        fi
                done
        fi
        echo 0
        return
}

PoorMansRandomGenerator() {
        local digits="${1}" # The number of digits to generate
        local number

        # Some read bytes cannot be used, se we read twice the number of required bytes
        dd if=/dev/urandom bs=$digits count=2 2> /dev/null | while read -r -n1 char; do
                number=$number$(printf "%d" "'$char")
                if [ ${#number} -ge $digits ]; then
                        echo ${number:0:$digits}
                        break;
                fi
        done
}

# Portable child (and grandchild) kill function tester under Linux, BSD and MacOS X
KillChilds() {
	local pid="${1}" # Parent pid to kill children
	local self="${2:-false}" # Should parent be killed too ?

	# Paranoid checks, we can safely assume that $pid should not be 0 nor 1
	if [ $(IsInteger "$pid") -eq 0 ] || [ "$pid" == "" ] || [ "$pid" == "0" ] || [ "$pid" == "1" ]; then
		log "Bogus pid given [$pid]." "CRITICAL"
		return 1
	fi

	if kill -0 "$pid" > /dev/null 2>&1; then
		if children="$(pgrep -P "$pid")"; then
			if [[ "$pid" == *"$children"* ]]; then
				log "Bogus pgrep implementation." "CRITICAL"
				children="${children/$pid/}"
			fi
			for child in $children; do
				log "Launching KillChilds \"$child\" true" "DEBUG"	#__WITH_PARANOIA_DEBUG
				KillChilds "$child" true
			done
		fi
	fi

	# Try to kill nicely, if not, wait 15 seconds to let Trap actions happen before killing
	if [ "$self" == true ]; then
		# We need to check for pid again because it may have disappeared after recursive function call
		if kill -0 "$pid" > /dev/null 2>&1; then
			log "Sent SIGTERM to process [$pid]." "DEBUG"
                        kill -s TERM "$pid"
			if [ $? -ne 0 ]; then
				sleep 60 # Arbitrary wait time to let process terminate gracefully
				if kill -0 "$pid" > /dev/null 2>&1; then
					log "Sending SIGTERM to process [$pid] failed." "DEBUG"
					kill -9 "$pid"
					if [ $? -ne 0 ]; then
						log "Sending SIGKILL to process [$pid] failed." "DEBUG"
						return 1
					fi	# Simplify the return 0 logic here
				else
					return 0
				fi
                        fi
		else
			return 0
		fi
	else
		return 0
	fi
}

exectasks() {
        # exectasks 2025072401 from ofunctions

        # Mandatory arguments
        local mainInput="${1}"                          # Contains list of pids / commands separated by semicolons or filepath to list of pids / commands

        # Optional arguments
        local id="${2:-(undisclosed)}"                  # Optional ID in order to identify global variables from this run (only bash variable names, no '-'). Global variables are WAIT_FOR_TASK_COMPLETION_$id and HARD_MAX_EXEC_TIME_REACHED_$id
        local readFromFile="${3:-false}"                # Is mainInput / auxInput a semicolon separated list (true) or a filepath (false)
        local softPerProcessTime="${4:-0}"              # Max time (in seconds) a pid or command can run before a warning is logged, unless set to 0
        local hardPerProcessTime="${5:-0}"              # Max time (in seconds) a pid or command can run before the given command / pid is stopped, unless set to 0
        local softMaxTime="${6:-0}"                     # Max time (in seconds) for the whole function to run before a warning is logged, unless set to 0
        local hardMaxTime="${7:-0}"                     # Max time (in seconds) for the whole function to run before all pids / commands given are stopped, unless set to 0
        local counting="${8:-true}"                     # Should softMaxTime and hardMaxTime be accounted since function begin (true) or since script begin (false)
        local sleepTime="${9:-.5}"                      # Seconds between each state check. The shorter the value, the snappier ExecTasks will be, but as a tradeoff, more cpu power will be used (good values are between .05 and 1)
        local keepLogging="${10:-1800}"                 # Every keepLogging seconds, an alive message is logged. Setting this value to zero disables any alive logging
        local spinner="${11:-true}"                     # Show spinner (true) or do not show anything (false) while running
        local noTimeErrorLog="${12:-false}"             # Log errors when reaching soft / hard execution times (false) or do not log errors on those triggers (true)
        local noErrorLogsAtAll="${13:-false}"           # Do not log any errors at all (useful for recursive ExecTasks checks)

        # Parallelism specific arguments
        local numberOfProcesses="${14:-0}"              # Number of simulanteous commands to run, given as mainInput. Set to 0 by default (WaitForTaskCompletion mode). Setting this value enables ParallelExec mode.
        local auxInput="${15}"                          # Contains list of commands separated by semicolons or filepath for list of commands. Exit code of those commands decide whether main commands will be executed or not
        local validExitCodes="${18:-0}"                 # Semi colon separated list of valid main command exit codes which will not trigger errors

        local i

        # Expand validExitCodes into array
        IFS=';' read -r -a validExitCodes <<< "$validExitCodes"

        # ParallelExec specific variables
        local auxItemCount=0            # Number of conditional commands
        local commandsArray=()          # Array containing commands
        local commandsConditionArray=() # Array containing conditional commands
        local currentCommand            # Variable containing currently processed command
        local currentCommandCondition   # Variable containing currently processed conditional command
        local commandsArrayPid=()       # Array containing commands indexed by pids
        local commandsArrayOutput=()    # Array containing command results indexed by pids
        local temp

        # Common variables
        local pid                       # Current pid working on
        local pidState                  # State of the process
        local mainItemCount=0           # number of given items (pids or commands)
        local readFromFile              # Should we read pids / commands from a file (true)
        local counter=0
        local log_ttime=0               # local time instance for comparison

        local seconds_begin=$SECONDS    # Seconds since the beginning of the script
        local exec_time=0               # Seconds since the beginning of this function

        local retval=0                  # return value of monitored pid process
        local subRetval=0               # return value of condition commands
        local errorcount=0              # Number of pids that finished with errors
        local pidsArray                 # Array of currently running pids
        local newPidsArray              # New array of currently running pids for next iteration
        local pidsTimeArray             # Array containing execution begin time of pids
        local executeCommand            # Boolean to check if currentCommand can be executed given a condition
        local functionMode
        local softAlert=false           # Does a soft alert need to be triggered, if yes, send an alert once
        local failedPidsList            # List containing failed pids with exit code separated by semicolons (eg : 2355:1;4534:2;2354:3)
        local randomOutputName          # Random filename for command outputs
        local currentRunningPids        # String of pids running, used for debugging purposes only

        # Initialise global variable
        eval "WAIT_FOR_TASK_COMPLETION_$id=\"\""
        eval "HARD_MAX_EXEC_TIME_REACHED_$id=false"

        # Init function variables depending on mode

        if [ $numberOfProcesses -gt 0 ]; then
                functionMode=ParallelExec
        else
                functionMode=WaitForTaskCompletion
        fi

        if [ $readFromFile == false ]; then
                if [ $functionMode == "WaitForTaskCompletion" ]; then
                        IFS=';' read -r -a pidsArray <<< "$mainInput"
                        mainItemCount="${#pidsArray[@]}"
                else
                        IFS=';' read -r -a commandsArray <<< "$mainInput"
                        mainItemCount="${#commandsArray[@]}"
                        IFS=';' read -r -a commandsConditionArray <<< "$auxInput"
                        auxItemCount="${#commandsConditionArray[@]}"
                fi
        else
                if [ -f "$mainInput" ]; then
                        mainItemCount=$(wc -l < "$mainInput")
                        readFromFile=true
                else
                        log "Cannot read main file [$mainInput]." "WARN"
                fi
                if [ "$auxInput" != "" ]; then
                        if [ -f "$auxInput" ]; then
                                auxItemCount=$(wc -l < "$auxInput")
                        else
                                log "Cannot read aux file [$auxInput]." "WARN"
                        fi
                fi
        fi

        if [ $functionMode == "WaitForTaskCompletion" ]; then
                # Force first while loop condition to be true because we do not deal with counters but pids in WaitForTaskCompletion mode
                counter=$mainItemCount
        fi

        # soft / hard execution time checks that needs to be a subfunction since it is called both from main loop and from parallelExec sub loop
        function _ExecTasksTimeCheck {
                if [ $spinner == true ]; then
                        spinner
                fi
                if [ $counting == true ]; then
                        exec_time=$((SECONDS - seconds_begin))
                else
                        exec_time=$SECONDS
                fi

                if [ $keepLogging -ne 0 ]; then
                        # This log solely exists for readability purposes before having next set of logs
                        if [ ${#pidsArray[@]} -eq $numberOfProcesses ] && [ $log_ttime -eq 0 ]; then
                                log_ttime=$exec_time
                                log "There are $((mainItemCount-counter)) / $mainItemCount tasks in the queue. Currently, ${#pidsArray[@]} tasks running with pids [$(joinString , ${pidsArray[@]})]." "NOTICE"
                        fi
                        if [ $(((exec_time + 1) % keepLogging)) -eq 0 ]; then
                                if [ $log_ttime -ne $exec_time ]; then # Fix when sleep time lower than 1 second
                                        log_ttime=$exec_time
                                        if [ $functionMode == "WaitForTaskCompletion" ]; then
                                                log "Current tasks ID=$id still running with pids [$(joinString , ${pidsArray[@]})]." "NOTICE"
                                        elif [ $functionMode == "ParallelExec" ]; then
                                                log "There are $((mainItemCount-counter)) / $mainItemCount tasks in the queue. Currently, ${#pidsArray[@]} tasks running with pids [$(joinString , ${pidsArray[@]})]." "NOTICE"
                                        fi
                                fi
                        fi
                fi

                if [ $exec_time -gt $softMaxTime ]; then
                        if [ "$softAlert" != true ] && [ $softMaxTime -ne 0 ] && [ $noTimeErrorLog != true ]; then
                                log "Max soft execution time [$softMaxTime] exceeded for task [$id] with pids [$(joinString , ${pidsArray[@]})]." "WARN"
                                softAlert=true
                                SendAlert true
                        fi
                fi

                if [ $exec_time -gt $hardMaxTime ] && [ $hardMaxTime -ne 0 ]; then
                        if [ $noTimeErrorLog != true ]; then
                                log "Max hard execution time [$hardMaxTime] exceeded for task [$id] with pids [$(joinString , ${pidsArray[@]})]. Stopping task execution." "ERROR"
                        fi
                        for pid in "${pidsArray[@]}"; do
                                KillChilds $pid true
                                if [ $? -eq 0 ]; then
                                        log "Task with pid [$pid] stopped successfully." "NOTICE"
                                else
                                        if [ $noErrorLogsAtAll != true ]; then
                                                log "Could not stop task with pid [$pid]." "ERROR"
                                        fi
                                fi
                                errorcount=$((errorcount+1))
                        done
                        if [ $noTimeErrorLog != true ]; then
                                SendAlert true
                        fi
                        eval "HARD_MAX_EXEC_TIME_REACHED_$id=true"
                        if [ $functionMode == "WaitForTaskCompletion" ]; then
                                return $errorcount
                        else
                                return 129
                        fi
                fi
        }

        function _ExecTasksPidsCheck {
                newPidsArray=()

                if [ "$currentRunningPids" != "$(joinString " " ${pidsArray[@]})" ]; then
                        log "ExecTask running for pids [$(joinString " " ${pidsArray[@]})]." "DEBUG"
                        currentRunningPids="$(joinString " " ${pidsArray[@]})"
                fi

                for pid in "${pidsArray[@]}"; do
                        if [ $(IsInteger $pid) -eq 1 ]; then
                                if kill -0 $pid > /dev/null 2>&1; then
                                        # Handle uninterruptible sleep state or zombies by omitting them from running process array (How to kill that is already dead ? :)
                                        pidState="$(eval $PROCESS_STATE_CMD)"
                                        if [ "$pidState" != "D" ] && [ "$pidState" != "Z" ]; then

                                                # Check if pid has not run more than soft/hard perProcessTime
                                                pidsTimeArray[$pid]=$((SECONDS - seconds_begin))
                                                if [ ${pidsTimeArray[$pid]} -gt $softPerProcessTime ]; then
                                                        if [ "$softAlert" != true ] && [ $softPerProcessTime -ne 0 ] && [ $noTimeErrorLog != true ]; then
                                                                log "Max soft execution time [$softPerProcessTime] exceeded for pid [$pid]." "WARN"
                                                                if [ "${commandsArrayPid[$pid]}]" != "" ]; then
                                                                        log "Command was [${commandsArrayPid[$pid]}]]." "WARN"
                                                                fi
                                                                softAlert=true
                                                                SendAlert true
                                                        fi
                                                fi


                                                if [ ${pidsTimeArray[$pid]} -gt $hardPerProcessTime ] && [ $hardPerProcessTime -ne 0 ]; then
                                                        if [ $noTimeErrorLog != true ] && [ $noErrorLogsAtAll != true ]; then
                                                                log "Max hard execution time [$hardPerProcessTime] exceeded for pid [$pid]. Stopping command execution." "ERROR"
                                                                if [ "${commandsArrayPid[$pid]}]" != "" ]; then
                                                                        log "Command was [${commandsArrayPid[$pid]}]]." "WARN"
                                                                fi
                                                        fi
                                                        KillChilds $pid true
                                                        if [ $? -eq 0 ]; then
                                                                 log "Command with pid [$pid] stopped successfully." "NOTICE"
                                                        else
                                                                if [ $noErrorLogsAtAll != true ]; then
                                                                log "Could not stop command with pid [$pid]." "ERROR"
                                                                fi
                                                        fi
                                                        errorcount=$((errorcount+1))

                                                        if [ $noTimeErrorLog != true ]; then
                                                                SendAlert true
                                                        fi
                                                fi

                                                newPidsArray+=($pid)
                                        fi
                                else
                                        # pid is dead, get its exit code from wait command
                                        wait $pid
                                        retval=$?
                                        # Check for valid exit codes
                                        if [ $(ArrayContains $retval "${validExitCodes[@]}") -eq 0 ]; then
                                                if [ "$noErrorLogsAtAll" != true ]; then
                                                        log "${FUNCNAME[0]} called by [$id] finished monitoring pid [$pid] with exitcode [$retval]." "ERROR"
                                                        if [ "$functionMode" == "ParallelExec" ]; then
                                                                log "Command was [${commandsArrayPid[$pid]}]." "ERROR"
                                                        fi
                                                        if [ -f "${commandsArrayOutput[$pid]}" ]; then
                                                                log "Truncated output:\n$(head -c16384 "${commandsArrayOutput[$pid]}")" "ERROR"
                                                        fi
                                                fi
                                                errorcount=$((errorcount+1))
                                                # Welcome to variable variable bash hell
                                                if [ "$failedPidsList" == "" ]; then
                                                        failedPidsList="$pid:$retval"
                                                else
                                                        failedPidsList="$failedPidsList;$pid:$retval"
                                                fi
                                        elif [ "$_DEBUG" == true ]; then
                                                if [ -f "${commandsArrayOutput[$pid]}" ]; then
                                                        log "${FUNCNAME[0]} called by [$id] finished monitoring pid [$pid] with exitcode [$retval]." "DEBUG"
                                                        log "Truncated output:\n$(head -c16384 "${commandsArrayOutput[$pid]}")" "DEBUG"
                                                fi
                                        else
                                                log "${FUNCNAME[0]} called by [$id] finished monitoring pid [$pid] with exitcode [$retval]." "DEBUG"
                                        fi
                                fi
                        fi
                done

                pidsArray=("${newPidsArray[@]}")

                # Trivial wait time for bash to not eat up all CPU
                sleep $sleepTime
        }

        while [ ${#pidsArray[@]} -gt 0 ] || [ $counter -lt $mainItemCount ]; do
                _ExecTasksTimeCheck
                retval=$?
                if [ $retval -ne 0 ]; then
                        return $retval;
                fi

                # The following execution block is only needed in ParallelExec mode since WaitForTaskCompletion does not execute commands, but only monitors them
                if [ $functionMode == "ParallelExec" ]; then
                        while [ ${#pidsArray[@]} -lt $numberOfProcesses ] && [ $counter -lt $mainItemCount ]; do
                                _ExecTasksTimeCheck
                                retval=$?
                                if [ $retval -ne 0 ]; then
                                        return $retval;
                                fi

                                executeCommand=false
                                currentCommand=""
                                currentCommandCondition=""

                                if [ $readFromFile == true ]; then
                                        # awk identifies first line as 1 instead of 0 so we need to increase counter
                                        currentCommand=$(awk 'NR == num_line {print; exit}' num_line=$((counter+1)) "$mainInput")
                                        if [ $auxItemCount -ne 0 ]; then
                                                currentCommandCondition=$(awk 'NR == num_line {print; exit}' num_line=$((counter+1)) "$auxInput")
                                        fi
                                else
                                        currentCommand="${commandsArray[$counter]}"
                                        if [ $auxItemCount -ne 0 ]; then
                                                currentCommandCondition="${commandsConditionArray[$counter]}"
                                        fi
                                fi

                                log "Running command [$currentCommand]." "INFO"
                                randomOutputName=$(date '+%Y%m%dT%H%M%S').$(PoorMansRandomGenerator 5)
                                eval "$currentCommand" >> "$RUN_DIR/$PROGRAM.${FUNCNAME[0]}.$id.$randomOutputName.$SCRIPT_PID.$TSTAMP" 2>&1 &
                                pid=$!
                                pidsArray+=($pid)
                                commandsArrayPid[$pid]="$currentCommand"
                                commandsArrayOutput[$pid]="$RUN_DIR/$PROGRAM.${FUNCNAME[0]}.$id.$randomOutputName.$SCRIPT_PID.$TSTAMP"
                                # Initialize pid execution time array
                                pidsTimeArray[$pid]=0
                                counter=$((counter+1))
                                _ExecTasksPidsCheck
                        done
                fi

        _ExecTasksPidsCheck
        done

        # Return exit code if only one process was monitored, else return number of errors
        # As we cannot return multiple values, a global variable WAIT_FOR_TASK_COMPLETION contains all pids with their return value

        eval "WAIT_FOR_TASK_COMPLETION_$id=\"$failedPidsList\""

        if [ $mainItemCount -eq 1 ]; then
                return $retval
        else
                return $errorcount
        fi
}

START=$SECONDS
TSTAMP=$(date '+%Y%m%dT%H%M%S').$(PoorMansRandomGenerator 5)
parse_args "$@"
acquire_lock
# Set bandwidth on replication interface if tcset is installed
if [ -n "${MAX_BANDWIDTH}" ]; then
        if [ -x /usr/local/bin/tcset ]; then
                log "Setting bandwidth on interface ${REPLICATION_INTERFACE} to ${MAX_BANDWIDTH}bps"
                /usr/local/bin/tcset "${REPLICATION_INTERFACE}" --rate ${MAX_BANDWIDTH}bps --overwrite
        else
                log "Cannot set bandwidth on interface ${REPLICATION_INTERFACE} since /usr/local/bin/tcset is not found" "ERROR"
        fi
fi
replicate
log "Run took $(($SECONDS-$START)) seconds" "INFO"