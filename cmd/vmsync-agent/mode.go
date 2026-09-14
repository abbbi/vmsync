package main

import (
	"fmt"
	"strings"
)

// agentMode is what this invocation of the agent is FOR. It is declared on the
// command line, not inferred from the configuration file.
//
// It used to be inferred, and there were only two of them: a "schedule_file"
// meant standalone, a "control_plane" meant control-plane. That worked while
// the two modes needed different files, but it made the most consequential
// property of a host -- whether a network service can start and stop things on
// it -- something an operator established by reading a JSON document and
// knowing a rule about it. A flag says it in the unit, where `systemctl cat`
// puts it on the same line as the binary.
//
// It is also the one piece of this agent's configuration that genuinely cannot
// be reloaded: the mode decides which goroutines exist, and goroutines are not
// something a SIGHUP can retract. Making it a flag removes a whole class of
// reload the agent would otherwise have had to detect and refuse.
type agentMode string

const (
	// modeStandalone is a scheduler and nothing else. The schedule comes from
	// a local file, no control plane is involved at any point, and nothing
	// outside this host can reach it.
	modeStandalone agentMode = "standalone"

	// modeMonitor reports, and does not act.
	//
	// It enrols with a control plane and keeps reporting to it, but runs no
	// scheduler, executes no operations and fences nothing. Replication here
	// is somebody else's job -- typically cron -- and this agent exists to
	// make what cron actually did visible in the console, rather than leaving
	// a whole site invisible because it is driven by a crontab.
	//
	// It still POLLS the control plane, which is not the contradiction it
	// looks like. The poll is inbound-only: it fetches configuration and
	// caches it, and it is what carries the expected cadence, without which
	// every report would say "no configured cadence" and the console could
	// never show a cron-driven VM as overdue -- most of the reason to watch
	// one. What makes the mode read-only is not a refusal to receive
	// configuration, it is that the goroutines which could act on it are
	// never started.
	modeMonitor agentMode = "monitor"

	// modeControlled is the full agent: the control plane holds the schedule,
	// this agent runs it, executes the operations it publishes, and fences on
	// its behalf.
	modeControlled agentMode = "controlled"
)

// resolveMode turns the three flags into one mode, or says what is wrong.
//
// Exactly one is required, and there is deliberately no default. The only
// sensible default would be modeControlled, which is the mode in which a
// network service can stop this host's VMs -- and a default is how a host ends
// up in a mode nobody chose for it. Naming it costs one word in the unit file
// and is the difference between a decision and an accident.
func resolveMode(standalone, monitor, controlled bool) (agentMode, error) {
	var chosen []string
	if standalone {
		chosen = append(chosen, "--standalone")
	}
	if monitor {
		chosen = append(chosen, "--monitor")
	}
	if controlled {
		chosen = append(chosen, "--controlled")
	}

	switch len(chosen) {
	case 1:
		switch {
		case standalone:
			return modeStandalone, nil
		case monitor:
			return modeMonitor, nil
		default:
			return modeControlled, nil
		}
	case 0:
		return "", fmt.Errorf("no mode given: pass exactly one of --standalone (schedule from a local file, no control plane), --monitor (report to a control plane, run nothing) or --controlled (the control plane holds the schedule and this agent runs it)")
	default:
		return "", fmt.Errorf("%s were all given: an agent runs in exactly one mode", strings.Join(chosen, " and "))
	}
}

// checkFile refuses a configuration file that does not describe the mode this
// agent was told to run in.
//
// The file and the flag can disagree in ways that are individually harmless
// and jointly useless -- a --controlled agent whose file has no control plane
// has nothing to be controlled by; a --standalone agent with no schedule file
// has nothing to run. Every one of them is a host that starts, logs nothing
// alarming and replicates nothing, which is the failure this whole agent is
// built to refuse.
//
// AgentFile.Validate has already established that the file is internally
// coherent (schedule_file and control_plane are mutually exclusive, and at
// least one is present). This adds only the question Validate cannot ask,
// because the answer is not in the file: is this the mode the operator asked
// for?
func (m agentMode) checkFile(a AgentFile, configPath string) error {
	switch m {
	case modeStandalone:
		if a.ScheduleFile == "" {
			return fmt.Errorf(`--standalone, but %s has no "schedule_file": with no control plane that file is the only thing that says what to sync`, configPath)
		}
	case modeMonitor, modeControlled:
		if a.ControlPlane == nil {
			return fmt.Errorf(`--%s, but %s has no "control_plane": there is nothing to enrol with or report to. Use --standalone to schedule from a local file instead`, m, configPath)
		}
	default:
		return fmt.Errorf("unknown agent mode %q", string(m))
	}

	// Deliberately NOT checked here: "features.schedule" and
	// "features.autofence" against monitor mode.
	//
	// It is the obvious next rule -- a monitor agent runs neither, so a file
	// asking for both looks like a contradiction worth refusing -- and it is
	// wrong, because by this point the file no longer says what the operator
	// wrote. Both fields are *bool so that "not mentioned" can be told apart
	// from "turned off", but applyDefaults has already turned every nil into
	// true before LoadAgentFile returns. Refusing a true here would refuse
	// every monitor configuration ever written, over a value the loader
	// invented rather than one anybody asked for.
	//
	// Monitor mode therefore does not consult features at all: it does not
	// act, so there is nothing for them to describe. run() says so in the log
	// at startup, once per mode, which is where an operator who did write
	// those lines will see that they do not apply.
	return nil
}

// actsOnItsOwn reports whether this mode starts anything that can change the
// state of a VM.
//
// One predicate rather than three comparisons at three call sites: the loops
// it gates (the scheduler, the operations loop, the fence loop) are exactly
// the ones that act, and a fourth acting loop added later should have to name
// this rather than re-derive the rule from the mode list.
func (m agentMode) actsOnItsOwn() bool { return m != modeMonitor }
