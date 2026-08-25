/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"vmsync/pkg/disk"
	"vmsync/pkg/failover"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/metrics"
	"vmsync/pkg/nbdbridge"
	"vmsync/pkg/nbdsync"
	"vmsync/pkg/portalloc"
	"vmsync/pkg/remotessh"
	"vmsync/pkg/restorepoint"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"
	"vmsync/pkg/version"

	"libvirt.org/go/libvirt"
)

// optionalValueFlag implements flag.Value (plus the IsBoolFlag optimization)
// for a string flag that also works bare -- "-name" alone resolves to
// bareDefault, "-name=x" takes x literally, and "-name=false" (or simply
// omitting the flag) disables it. Plain flag.StringVar can't do this: the
// bare "-name" form (no "=value") is only accepted by the flag package for
// a Value whose IsBoolFlag() returns true, which is otherwise reserved for
// real bools -- this hijacks that same mechanism for a still-string-valued
// flag with a sensible bare default.
type optionalValueFlag struct {
	value       string
	bareDefault string
}

func (f *optionalValueFlag) String() string   { return f.value }
func (f *optionalValueFlag) IsBoolFlag() bool { return true }
func (f *optionalValueFlag) Set(s string) error {
	switch s {
	case "true":
		f.value = f.bareDefault
	case "false":
		f.value = ""
	default:
		f.value = s
	}
	return nil
}

// runLockDir holds both run locks: the source-side one taken in main(), and
// the target-side one taken in run() over SSH. The same directory on
// whichever host the lock belongs to.
const runLockDir = "/run/vmsync-locks"

// targetLockTimeout bounds acquiring the target-side lock. Short on purpose:
// it is a single SSH round trip, and a contended lock answers immediately.
// Anything slower is a sick host, and waiting longer to discover that only
// delays a run which is going to fail anyway.
const targetLockTimeout = 30 * time.Second

// clockSkewWarnAt is when a difference between two hosts' clocks stops
// being measurement noise and starts corrupting comparisons.
//
// Generous on purpose: `date +%s` has one-second resolution and a WAN round
// trip adds more, so a few seconds means nothing. Tens of seconds is enough
// to invert the comparison between a target's last_sync and a source's
// last_replicated, which is the point at which somebody could restore the
// wrong copy.
const clockSkewWarnAt = 30 * time.Second

// targetLockKey namespaces the target-side lock so it cannot collide with
// the source-side lock of a same-named domain. Both live in runLockDir, and
// a domain that is replicated onto a host which also replicates it onward
// would otherwise contend with itself.
func targetLockKey(targetDomain string) string { return "target-" + targetDomain }

// Accepted values for -replaced-disk-action.
const (
	replacedDiskDelete = "delete"
	replacedDiskRename = "rename"
	// replacedDiskSuffix precedes the unix timestamp on a renamed-aside disk.
	// Distinctive enough to find with a glob when reclaiming space later,
	// and clearly vmsync's doing rather than something a human left behind.
	replacedDiskSuffix = ".vmsync-replaced-"

	// fenceSourceAuto is what bare -fence-source resolves to: take the
	// source to fence from the target's own replica_source, rather than
	// making an operator retype a reference they can get wrong under
	// pressure. Not a valid host:domain itself -- it has no colon -- so it
	// can never be mistaken for one.
	fenceSourceAuto = "auto"
)

// syncConfig is every value the CLI accepts, parsed once in main() and
// passed to run() by value.
//
// Deliberately a named type rather than the anonymous struct this used to
// be, declared identically in both places. Two anonymous struct types are
// identical in Go only when their fields match name-for-name,
// type-for-type, IN ORDER -- so adding a flag meant editing two lists and
// keeping them in step, and getting it wrong produced a compiler error that
// prints both 30-field type literals in full without naming the field that
// differs. Naming the type makes adding a flag a one-line change and turns
// a mismatch into an "unknown field" error pointing at the actual mistake.
//
// Not every field is read by run(): UpdateRole and ShowVersion are handled
// entirely in main(), which exits before run() is called. They live here
// anyway because this type is the CLI surface, not run()'s parameter list.
type syncConfig struct {
	// LocalHostName is what to call THIS machine when recording it in
	// replica_source / replica_targets / promoted_from.
	//
	// Only used when the relevant URI names no host, which means "this
	// machine" -- and only for identity, never for connectivity. Empty falls
	// back to the system hostname. It exists because the control plane
	// correlates a pair by matching those references against the hostname an
	// agent reports under, and an agent can be told to report under a name
	// that is not os.Hostname().
	LocalHostName string

	// ReplacedDiskAction selects what happens to a target disk file that is
	// about to be discarded and rebuilt: replacedDiskRename (the default) or
	// replacedDiskDelete. See the switch in run()'s reinit block for why
	// this is the operator's decision rather than vmsync's.
	//
	// Named for what it governs -- a disk being replaced -- rather than for
	// -reinit, both because a name a character away from an existing flag
	// invites passing the wrong one, and because the discard-and-rebuild
	// step is not conceptually tied to that one flag.
	ReplacedDiskAction string
	// Retention is the raw -retention value; RetentionPolicy is it parsed.
	// Both are kept because the raw string is what an error message should
	// quote back at the operator.
	Retention       string
	RetentionPolicy restorepoint.Policy
	// ReinitAutomatic is true when -reinit-after-failures forced this reinit
	// rather than an operator asking for one. The two are not interchangeable
	// where restore points are concerned: see sweepRestorePointsForReinit.
	ReinitAutomatic bool
	// The two read-only restore point verbs. Neither touches the replica,
	// libvirt, or any replication state.
	ListRestorePoints   bool
	CloneRestorePoint   string
	CloneRestorePointTo string
	// RestoreRestorePoint names the restore point to put back over the
	// replica, and ForceRestore is what turns the assessment into the act.
	// Two flags rather than one because the assessment is the useful half
	// during an incident: it names every file that would change and the exact
	// replication state that would follow, and it can be run against a
	// production replica while deciding.
	RestoreRestorePoint string
	ForceRestore        bool
	// TestFault names a failure to inject into this run, or "" in every real
	// one. See libvirtsync.TestFault for why this exists and why it is a flag
	// rather than an environment variable.
	TestFault string
	// TargetDiskOwner is who should own the disk files vmsync creates on the
	// target. See util.ParseDiskOwner: qemu-img runs over SSH as root, and
	// qemu does not run as root, so an unowned-for disk is one a promoted
	// domain may be unable to open.
	TargetDiskOwner string

	SourceURI      string
	TargetURI      string
	SourceDomain   string
	TargetDomain   string
	TargetDiskPath string
	SourceNBDHost  string
	SourceNBDBind  string
	TargetNBDHost  string
	TargetNBDBind  string

	// SourceNBDPortSpec/TargetNBDPortSpec hold the raw -source-nbd-port /
	// -target-nbd-port flag values, which accept a fixed port, a range, or
	// "auto" (see portalloc.ParseSpec). SourceNBDPort/TargetNBDPort are the
	// resolved base ports, filled in by run() once the disk count and
	// bridge/verify settings are known -- every other port in the run is
	// derived from them by offset.
	SourceNBDPortSpec string
	TargetNBDPortSpec string
	SourceNBDPort     int
	TargetNBDPort     int

	SSHUser       string
	SSHKey        string
	SSHPassword   string
	SSHPort       int
	SSHInsecure   bool
	KnownHosts    string
	SSHTimeoutSec int

	Start                  bool
	Reinit                 bool
	ReinitAfterFailures    int
	Verify                 string
	IgnoreExternalSnapshot bool
	IODepth                int

	Compress         string
	CompressLevel    string
	NetBuffer        string
	BridgeHelperPath string
	UseSSH           bool

	PrometheusTextfile string
	Debug              bool

	// Handled in main(), never read by run() -- see the type's own comment.
	UpdateRole  string
	ShowVersion bool

	// The failover modes, all handled in main() like -update-role: each
	// changes state and exits, syncing nothing.
	Promote        bool
	Invert         bool
	ShutdownDomain bool
	ReadFence      bool
	PromoteMode    string
	PromotedBy     string
	ForcePromote   bool
	// FenceSource arms a fence against the displaced source: "auto" to take
	// it from the target's own replica_source, an explicit "host:domain", or
	// empty to arm nothing. Empty is the default because a promotion that
	// shuts a production VM down must be asked for, never assumed -- a DR
	// drill is a promotion too.
	FenceSource        string
	ShutdownTimeoutSec int
}

func main() {
	if os.Getenv("PROFILE") == "development" {
		host := "localhost:6060"
		trace.Info("Enabling pprof for profiling", "address", host)
		go func() {
			log.Println(http.ListenAndServe(host, nil))
		}()
	}

	var cfg syncConfig

	flag.StringVar(&cfg.SourceURI, "source-uri", "", "libvirt source URI (example: qemu+ssh://src/system)")
	flag.StringVar(&cfg.TargetURI, "target-uri", "", "libvirt target URI (example: qemu+ssh://target/system)")
	flag.StringVar(&cfg.SourceDomain, "source-domain", "", "source domain name")
	flag.StringVar(&cfg.TargetDomain, "target-domain", "", "target domain name (defaults to --source-domain)")
	flag.StringVar(&cfg.TargetDiskPath, "target-disk-path", "", "target disk path for changed location")
	flag.StringVar(&cfg.SourceNBDBind, "source-nbd-bind", "0.0.0.0", "source bind address for libvirt backup NBD TCP export")
	flag.StringVar(&cfg.SourceNBDPortSpec, "source-nbd-port", "10809", fmt.Sprintf("Source TCP port for the libvirt backup NBD export. A fixed port (10809), a range to pick a free block from (%d-%d), or \"auto\" for the default range %d-%d. A run needs 1 port here, or 2 when -compress/-netbuffer is set", portalloc.DefaultSourceAutoLow, portalloc.DefaultSourceAutoHigh, portalloc.DefaultSourceAutoLow, portalloc.DefaultSourceAutoHigh))
	flag.StringVar(&cfg.SourceNBDHost, "source-nbd-host", "", "source host to connect for NBD reads (defaults from --source-uri)")
	flag.StringVar(&cfg.TargetNBDBind, "target-nbd-bind", "0.0.0.0", "target bind address for qemu-nbd TCP export")
	flag.StringVar(&cfg.TargetNBDPortSpec, "target-nbd-port", "20809", fmt.Sprintf("Target base TCP port for the qemu-nbd exports. A fixed port (20809), a range to pick a free block from (%d-%d), or \"auto\" for the default range %d-%d. A run needs N consecutive ports for N disks, 2N with -compress/-netbuffer, 3N with -verify, 4N with both", portalloc.DefaultTargetAutoLow, portalloc.DefaultTargetAutoHigh, portalloc.DefaultTargetAutoLow, portalloc.DefaultTargetAutoHigh))
	flag.StringVar(&cfg.TargetNBDHost, "target-nbd-host", "", "target host to connect for NBD writes (defaults from --target-uri)")
	flag.StringVar(&cfg.SSHUser, "ssh-user", "", "ssh user for remote command execution (defaults from URI user, then ~/.ssh/config's User, then root)")
	flag.StringVar(&cfg.SSHKey, "ssh-key", "", "private key path for ssh authentication (defaults from ~/.ssh/config's IdentityFile)")
	flag.StringVar(&cfg.SSHPassword, "ssh-password", "", "password for ssh authentication")
	flag.IntVar(&cfg.SSHPort, "ssh-port", 0, "ssh port for remote command execution (0 = use ~/.ssh/config's Port, falling back to 22)")
	flag.BoolVar(&cfg.SSHInsecure, "ssh-insecure-host-key", false, "disable host key verification (not recommended)")
	flag.StringVar(&cfg.KnownHosts, "ssh-known-hosts", "", "known_hosts file path (defaults to ~/.ssh/known_hosts)")
	flag.IntVar(&cfg.SSHTimeoutSec, "ssh-timeout-sec", 10, "ssh connection timeout in seconds")
	flag.BoolVar(&cfg.Start, "start", false, "In case vm is in non-running state, start in paused mode to allow sync")
	flag.BoolVar(&cfg.Reinit, "reinit", false, "Delete VM on target and restart a full sync process")
	flag.StringVar(&cfg.ReplacedDiskAction, "replaced-disk-action", replacedDiskRename, fmt.Sprintf("What to do with a target disk file that is about to be discarded and rebuilt (currently only -reinit does this): %q renames it to <path>%s<unixtime> so its contents survive, %q removes it. Defaults to %q: the target of a reinit may be a former primary whose disks still hold everything written after the last successful sync, and that is unrecoverable once deleted. Renaming needs room for both copies, and the aside files are never reaped automatically", replacedDiskRename, replacedDiskSuffix, replacedDiskDelete, replacedDiskRename))
	flag.StringVar(&cfg.TargetDiskOwner, "target-disk-owner", util.DiskOwnerAuto, fmt.Sprintf("Who should own the disk files created on the target: %q (default), %q, or an explicit \"user\", \"user:group\" or \":group\". vmsync creates those files by running qemu-img over SSH, so they are owned by that SSH user (root) -- while qemu runs as \"qemu\" on RHEL and \"libvirt-qemu\" on Debian, and cannot open a root-owned disk. libvirt's dynamic_ownership usually hides this, but it is off in plenty of deployments and cannot work at all on NFS with root_squash. %q preserves whatever owned the file before (which is what makes -reinit safe, since it replaces a correctly-owned disk with a fresh root-owned one) and otherwise takes what the target's libvirt qemu.conf sets; it never guesses, and warns instead. %q is the old behaviour", util.DiskOwnerAuto, util.DiskOwnerOff, util.DiskOwnerAuto, util.DiskOwnerOff))
	flag.IntVar(&cfg.ReinitAfterFailures, "reinit-after-failures", 0, "Reinit automatically after N failures (disabled by default). Count is held on target XML")
	flag.StringVar(&cfg.Retention, "retention", "", "Keep point-in-time copies of the replica on the target, as COUNT,INTERVAL -- for example 24,3h for twenty-four copies at least three hours apart, so a sync that faithfully replicated an already-damaged source can be stepped back from. The COUNT is the guarantee; the window it covers is not, because vmsync does not decide when it runs: the interval is a floor (\"take one if at least this long has passed\"), so a pair syncing every 4h gets 4h spacing and a pause leaves a gap. Copies are made with reflink, share storage with the replica, and cost almost nothing until they diverge -- but the target filesystem must support it (XFS with reflink=1, or btrfs), and this is refused at startup where it does not. Disabled by default")
	flag.BoolVar(&cfg.ListRestorePoints, "list-restore-points", false, "List the restore points kept on the target and stop. Needs -target-uri and -target-disk-path; reads the target filesystem only, and touches neither the replica nor libvirt")
	flag.StringVar(&cfg.CloneRestorePoint, "clone-restore-point", "", "Copy one restore point's disks to the directory given by -clone-to, and stop. Takes a tag from -list-restore-points. This is how to answer \"is that copy clean?\": boot a throwaway domain from the clone. It changes nothing about the replica, its metadata, or its role -- restoring in place is a different operation and is deliberately not this one")
	flag.StringVar(&cfg.CloneRestorePointTo, "clone-to", "", "Directory on the target to write -clone-restore-point's copies into. Created if missing")
	flag.StringVar(&cfg.RestoreRestorePoint, "restore-restore-point", "", "Put one restore point back over the replica IN PLACE, discarding its current contents. Takes a tag from -list-restore-points. Without -force-restore this only prints an assessment and changes nothing. A restore is for promoting: it leaves replication PAUSED, because the next sync from the same source would otherwise overwrite exactly what was rolled back to")
	flag.BoolVar(&cfg.ForceRestore, "force-restore", false, "Carry out -restore-restore-point instead of only assessing it. Required, because a restore replaces the replica's disks and cannot be undone once the displaced contents are removed")
	flag.StringVar(&cfg.TestFault, "test", "", fmt.Sprintf("FOR TESTING ONLY: make vmsync deliberately fail at a chosen point, so error-recovery paths that cannot be reached from outside the process can be exercised. Accepts one of: %s. A run with this set WILL fail and its result means nothing as a replication. Listed here rather than hidden, so an operator who finds it in a log can look it up", strings.Join(libvirtsync.TestFaults, ", ")))
	compressArg := optionalValueFlag{bareDefault: "s2"}
	fenceSourceArg := optionalValueFlag{bareDefault: fenceSourceAuto}
	netBufferArg := optionalValueFlag{bareDefault: "128k,1G"}
	flag.Var(&compressArg, "compress", "Compress NBD traffic between hosts. Bare -compress (no value) defaults to \"s2\"); ACCEPTS \"zstd\" or \"s2\". Requires vmsync-bridge-helper binary on target")
	flag.StringVar(&cfg.CompressLevel, "compress-level", "3", "Compression level/mode to use when -compress is set. For -compress=zstd: a number 1-19 (default 3 when not set explicitly). For -compress=s2 (which has no numeric levels, including bare -compress, which defaults to s2): one of \"default\" (s2's own fastest mode), \"better\" (default here when not set explicitly), or \"best\".")
	flag.Var(&netBufferArg, "netbuffer", "Buffer NBD bridge traffic through a bounded in-memory buffer to smooth throughput, formatted as <blocksize>,<buffersize> (e.g. 64k,512M). Defaults to \"128k,1G\". Requires vmsync-bridge-helper binary on target")
	flag.StringVar(&cfg.BridgeHelperPath, "bridge-helper-path", "/usr/local/bin/vmsync-bridge-helper", "Remote path to the vmsync-bridge-helper binary. Defaults to /usr/local/bin")
	flag.BoolVar(&cfg.UseSSH, "use-ssh", false, "When --compress/--netbuffer is set, route the bridged NBD traffic through the existing SSH connection as an encrypted tunnel")
	flag.IntVar(&cfg.IODepth, "io-depth", 8, "Number of NBD read/write pairs to keep in flight simultaneously during the disk copy, defaults to 8")
	flag.StringVar(&cfg.PrometheusTextfile, "prometheus-textfile", "", "Write sync metrics to this path in Prometheus textfile-collector format. Name should be something like /var/lib/node_exporter/textfile_collector/vmsync_[vmname].prom")
	flag.BoolVar(&cfg.IgnoreExternalSnapshot, "ignore-external-snapshot", false, "If the source domain currently has any external disk snapshot, skip this run entirely")
	flag.StringVar(&cfg.Verify, "verify", "", "After syncing, verify target matches source for every disk. Accepts compare|fast|online. See documentation for details. (compare|fast suspend the source domain, online does not)")
	flag.StringVar(&cfg.UpdateRole, "update-role", "", "Set the replication role recorded in a domain's own vmsync metadata, then exit without syncing anything. Accepts "+strings.Join(libvirtsync.ValidRoles, "|")+" (\"none\" clears it). The domain is addressed with -target-uri/-target-domain regardless of which direction it currently replicates in. vmsync refuses to sync INTO a domain whose role is anything other than \"target\" or unset -- this is what stops a scheduled sync from overwriting a domain that was failed over to and then shut down for maintenance")
	flag.StringVar(&cfg.LocalHostName, "local-host-name", "", "What to call this machine when recording it in replica_source/replica_targets/promoted_from, for a -source-uri or -target-uri that names no host. Defaults to the system hostname. Set it when something else refers to this host by a different name -- vmsync-agent passes its own --hostname here, because the control plane matches these references against the name an agent reports under")
	flag.BoolVar(&cfg.Promote, "promote", false, "Promote the replica named by -target-uri/-target-domain to serve live: record the promotion and, with -start, boot it. Refuses unless the target actually holds a usable replica. Must be run on the target's own host")
	flag.BoolVar(&cfg.Invert, "invert", false, "Reverse a pair's direction after a failover: -source-uri/-source-domain name the OLD source, -target-uri/-target-domain the promoted replica. Run on the old source's host")
	flag.BoolVar(&cfg.ShutdownDomain, "shutdown-domain", false, "Shut the domain named by -target-uri/-target-domain down cleanly and pause its replication. The source half of a planned failover; must be run on that domain's own host")
	flag.BoolVar(&cfg.ReadFence, "read-fence", false, "Ask the peer named by -target-uri/-target-domain whether its promotion armed a fence against this host, and print the answer as JSON. Reads only; changes nothing anywhere. Unlike the other failover modes this one accepts a REMOTE uri, because asking the other site is the entire operation. An unreachable peer is reported as unreachable rather than as an absence of fencing")
	flag.StringVar(&cfg.PromoteMode, "promote-mode", string(failover.ModeForced), fmt.Sprintf("How this promotion came about, recorded on the domain: %q when the source was cleanly shut down first (no data lost), %q when it was never reached", failover.ModePlanned, failover.ModeForced))
	flag.StringVar(&cfg.PromotedBy, "promoted-by", "", "Who is performing this promotion, recorded on the domain for attribution")
	flag.BoolVar(&cfg.ForcePromote, "force-promote", false, "Promote even when the target does not look like a usable replica (missing disks, no completed sync, an interrupted copy). The data-loss window is then reported as unknown rather than guessed")
	flag.Var(&fenceSourceArg, "fence-source", "With -promote: arm a fence so the displaced source shuts itself down, instead of leaving one VM running in two places. Bare -fence-source takes the source from the target's own replica_source; an explicit host:domain names it directly. Off by default, because a DR drill is a promotion too and must not stop production. The promoted domain records the decision; the source acts on it once, ever, and never destroys a guest that ignores the shutdown request")
	flag.IntVar(&cfg.ShutdownTimeoutSec, "shutdown-timeout-sec", 300, "How long -shutdown-domain waits for a clean guest shutdown. On expiry it fails and leaves the domain running rather than destroying it")
	flag.BoolVar(&cfg.Debug, "debug", false, "Enable debug logging")
	flag.BoolVar(&cfg.ShowVersion, "v", false, "Show version and exit")
	flag.BoolVar(&cfg.ShowVersion, "version", false, "Show version and exit")
	flag.Parse()
	cfg.FenceSource = fenceSourceArg.value
	cfg.Compress = compressArg.value
	cfg.NetBuffer = netBufferArg.value

	// A flag.Var flag whose Value implements IsBoolFlag (both -compress and
	// -netbuffer, here) never consumes a following space-separated argument
	// as its value -- only "-flag=value" does that (see the flag package's
	// own documented behavior). Passing one anyway (e.g. "-compress zstd")
	// leaves "zstd" as an ordinary positional argument, which stops flag
	// parsing right there and silently drops every flag typed after it --
	// including, for example, a trailing -verify=online. vmsync takes no
	// positional arguments at all, so any leftover ones are unambiguously a
	// mistake -- fail loudly instead of silently ignoring whatever came
	// after them.
	if flag.NArg() > 0 {
		trace.Error("invalid command line", "error", fmt.Errorf("unexpected extra argument(s) %v -- if you meant to pass a value to -compress or -netbuffer, use -compress=value / -netbuffer=value (with an \"=\"), not a space", flag.Args()))
		os.Exit(2)
	}

	// Tracks whether --compress-level was actually passed on the command
	// line, as opposed to just carrying its zstd-oriented flag default
	// ("3") -- needed below to swap in s2's own default ("better") instead
	// when -compress=s2 and the user didn't ask for a specific level.
	compressLevelExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "compress-level" {
			compressLevelExplicit = true
		}
	})

	if cfg.ShowVersion {
		trace.Info(fmt.Sprintf("vmsync Version: %s", version.Version))
		os.Exit(0)
	}
	// -update-role is a mode, not a modifier: it changes one metadata field
	// on one domain and exits, syncing nothing. Handled before the
	// source-side argument check below, and before every sync-specific
	// validation, because none of it applies -- there is no compression,
	// no netbuffer, no NBD transport and no checkpoint involved, no source
	// domain is read (so -source-uri/-source-domain aren't required at
	// all), and in particular there is no requirement that the URI be
	// qemu+ssh://: that constraint exists for the data path, which this
	// never touches, so a purely local qemu:///system domain can have its
	// role set too.
	//
	// It deliberately does NOT take the run lock either. The lock is keyed
	// by SOURCE domain and exists to serialize checkpoint-chain
	// mutations, which this doesn't perform -- and blocking behind an
	// in-flight sync would be actively wrong here, since marking a domain
	// promoted or paused is exactly what an operator needs to be able to
	// do WHILE a misdirected sync is running. SetReplicationRole's own
	// re-read-before-write guard is what protects the field itself from a
	// concurrent writer.
	// The failover modes sit alongside -update-role for the same reasons
	// spelled out above it: each changes state on one or two domains and
	// exits, syncing nothing, so none of the sync-path validation applies.
	//
	// Mutually exclusive with each other and with -update-role. Combining
	// them has no meaning, and quietly running one of them would be a poor
	// way to find that out.
	{
		var chosen []string
		for _, m := range []struct {
			on   bool
			name string
		}{
			{cfg.Promote, "-promote"},
			{cfg.Invert, "-invert"},
			{cfg.ShutdownDomain, "-shutdown-domain"},
			{cfg.ReadFence, "-read-fence"},
			{cfg.UpdateRole != "", "-update-role"},
			{cfg.ListRestorePoints, "-list-restore-points"},
			{cfg.CloneRestorePoint != "", "-clone-restore-point"},
			{cfg.RestoreRestorePoint != "", "-restore-restore-point"},
		} {
			if m.on {
				chosen = append(chosen, m.name)
			}
		}
		if len(chosen) > 1 {
			trace.Error("conflicting modes", "error", fmt.Errorf("%s cannot be combined; each is a separate operation", strings.Join(chosen, " and ")))
			os.Exit(2)
		}

		if len(chosen) == 1 && chosen[0] != "-update-role" {
			trace.SetDebug(cfg.Debug)
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			var err error
			switch {
			case cfg.Promote:
				err = runPromote(ctx, cfg)
			case cfg.Invert:
				err = runInvert(ctx, cfg)
			case cfg.ShutdownDomain:
				err = runShutdownDomain(ctx, cfg)
			case cfg.ReadFence:
				err = runReadFence(cfg)
			case cfg.ListRestorePoints:
				err = runListRestorePoints(ctx, cfg)
			case cfg.CloneRestorePoint != "":
				err = runCloneRestorePoint(ctx, cfg, cfg.CloneRestorePoint, cfg.CloneRestorePointTo)
			case cfg.RestoreRestorePoint != "":
				err = runRestoreRestorePoint(ctx, cfg, cfg.RestoreRestorePoint)
			}
			if err != nil {
				trace.Error(strings.TrimPrefix(chosen[0], "-"), "error", err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}

	if cfg.UpdateRole != "" {
		// Validated before connecting, so a typo costs an immediate,
		// readable error rather than a libvirt round trip first.
		if err := libvirtsync.ValidateRole(cfg.UpdateRole); err != nil {
			trace.Error("invalid update-role configuration", "error", err)
			os.Exit(2)
		}
		roleDomain := cfg.TargetDomain
		if roleDomain == "" {
			roleDomain = cfg.SourceDomain
		}
		if cfg.TargetURI == "" || roleDomain == "" {
			trace.Error("invalid update-role configuration", "error", fmt.Errorf("-update-role needs -target-uri and -target-domain (or -source-domain) naming the domain whose role to set"))
			os.Exit(2)
		}
		mgr, err := libvirtsync.Connect(cfg.TargetURI)
		if err != nil {
			trace.Error("update-role: connect to libvirt", "uri", cfg.TargetURI, "error", err)
			os.Exit(1)
		}
		defer mgr.Close()
		previous, err := libvirtsync.SetReplicationRole(mgr, roleDomain, cfg.UpdateRole)
		if err != nil {
			trace.Error("update-role: set replication role", "vm", roleDomain, "role", cfg.UpdateRole, "error", err)
			os.Exit(1)
		}
		displayRole := func(r string) string {
			if r == "" || r == libvirtsync.RoleNone {
				return "(none)"
			}
			return r
		}
		trace.Info("replication role updated", "vm", roleDomain, "uri", cfg.TargetURI, "from", displayRole(previous), "to", displayRole(cfg.UpdateRole))
		os.Exit(0)
	}

	if cfg.SourceURI == "" || cfg.TargetURI == "" || cfg.SourceDomain == "" {
		flag.Usage()
		os.Exit(2)
	}
	if cfg.TargetDomain == "" {
		cfg.TargetDomain = cfg.SourceDomain
	}
	// Parsed here rather than where it is first used, for the same reason
	// -target-disk-owner is: the first place it would otherwise be read is
	// after the whole disk copy, and refusing a typo'd retention value at
	// that point would throw away the run that just paid for it.
	retentionPolicy, retentionErr := restorepoint.ParsePolicy(cfg.Retention)
	if retentionErr != nil {
		trace.Error("invalid -retention", "error", retentionErr)
		os.Exit(2)
	}
	cfg.RetentionPolicy = retentionPolicy
	if cfg.RetentionPolicy.Enabled() {
		// Said out loud on every run that asks for it. A feature whose whole
		// value is "there will be a copy to go back to tomorrow" should never
		// be something an operator has to infer from the absence of errors.
		trace.Info("restore points enabled", "retention", cfg.RetentionPolicy.String(), "keep", cfg.RetentionPolicy.Count, "interval", cfg.RetentionPolicy.Interval.String())
	}

	// Fault injection, off in every real run. Validated before anything else
	// touches a domain so a typo cannot masquerade as a normal sync, and
	// announced at WARNING so a run doing this is never mistaken for one that
	// is not -- the whole point of it is to make vmsync fail, and a reader
	// finding that failure in a log needs to see why in the same place.
	if err := libvirtsync.ValidateTestFault(cfg.TestFault); err != nil {
		trace.Error("invalid -test", "error", err)
		os.Exit(2)
	}
	if cfg.TestFault != "" {
		libvirtsync.TestFault = cfg.TestFault
		trace.Warning("FAULT INJECTION ACTIVE: this run will deliberately fail, and is not a replication anybody should rely on", "test", cfg.TestFault)
	}

	// Validated whether or not -reinit was passed, so a typo is caught at
	// the point it is written rather than lying dormant until the day a
	// -reinit-after-failures threshold trips and silently falls back to
	// deleting disks the operator meant to keep.
	switch cfg.ReplacedDiskAction {
	case replacedDiskDelete, replacedDiskRename:
	default:
		trace.Error("invalid -replaced-disk-action", "error", fmt.Errorf("-replaced-disk-action must be %q or %q, not %q", replacedDiskRename, replacedDiskDelete, cfg.ReplacedDiskAction))
		os.Exit(2)
	}

	// Validated here rather than where it is used: the first place it would
	// otherwise be parsed is after the full disk copy, and refusing a typo'd
	// owner at that point would throw away the whole run.
	if _, err := util.ParseDiskOwner(cfg.TargetDiskOwner); err != nil {
		trace.Error("invalid -target-disk-owner", "error", err)
		os.Exit(2)
	}
	if cfg.Compress != "" {
		if err := nbdbridge.ValidateCompressAlgo(cfg.Compress); err != nil {
			trace.Error("invalid compress configuration", "error", err)
			os.Exit(2)
		}
		if !compressLevelExplicit && cfg.Compress == "s2" {
			cfg.CompressLevel = "better"
		}
		if err := nbdbridge.ValidateCompressLevel(cfg.Compress, cfg.CompressLevel); err != nil {
			trace.Error("invalid compress-level configuration", "error", err)
			os.Exit(2)
		}
	}
	if cfg.NetBuffer != "" {
		if _, _, err := nbdbridge.ParseNetBufferSpec(cfg.NetBuffer); err != nil {
			trace.Error("invalid netbuffer configuration", "error", err)
			os.Exit(2)
		}
	}
	if cfg.IODepth < 1 {
		trace.Error("invalid io-depth configuration", "error", fmt.Errorf("-io-depth must be at least 1, got %d", cfg.IODepth))
		os.Exit(2)
	}
	// Parsed here purely to reject a malformed value before doing any work;
	// the result is discarded and re-parsed in run(), where the disk count
	// needed to actually choose a port is finally known. Re-parsing is a
	// string split -- cheaper than threading a parsed type through the CLI
	// config, which is meant to hold plain flag values.
	if _, err := portalloc.ParseSpec(cfg.SourceNBDPortSpec, portalloc.DefaultSourceAutoLow, portalloc.DefaultSourceAutoHigh); err != nil {
		trace.Error("invalid source-nbd-port configuration", "error", err)
		os.Exit(2)
	}
	if _, err := portalloc.ParseSpec(cfg.TargetNBDPortSpec, portalloc.DefaultTargetAutoLow, portalloc.DefaultTargetAutoHigh); err != nil {
		trace.Error("invalid target-nbd-port configuration", "error", err)
		os.Exit(2)
	}
	switch cfg.Verify {
	case "", "compare", "fast", "online":
	default:
		trace.Error("invalid verify configuration", "error", fmt.Errorf("-verify must be \"compare\", \"fast\", or \"online\" (or omitted to disable verification), got %q", cfg.Verify))
		os.Exit(2)
	}

	trace.SetDebug(cfg.Debug)

	// Scoped to the source domain, not source+target: the shared, non-
	// reentrant resource two concurrent invocations would collide on first
	// is the SOURCE domain's checkpoint chain (proven by a real collision:
	// two processes both got "checkpoint already exists" creating the same
	// checkpoint name at once), so that's what needs protecting regardless
	// of which target either one is writing to. Acquired before any libvirt
	// connection at all, and before -reinit-after-failures's own metadata
	// read just below, so a losing invocation does the least possible work
	// before backing off. Genuine contention (util.ErrLockHeld, i.e.
	// another vmsync really is running for this domain) is not a sync
	// failure: exits 0, doesn't count toward -reinit-after-failures, and
	// (since run() is never called) never touches -prometheus-textfile
	// either -- a clean no-op skip, the same way the wrapper script's own
	// lock already treats "another instance is already running". Any
	// OTHER error (can't create/open the lock file -- e.g. a read-only or
	// permission-denied /run, or the lock file being repeatedly replaced
	// out from under acquisition attempts) means something is actually
	// broken, not that a peer is running: treating that the same way used
	// to silently and permanently stop replication for this domain, since
	// run() is never called, so -prometheus-textfile is never touched and
	// vmsync_sync_state keeps reporting whatever the last successful run
	// left behind -- with the process itself still reporting a clean exit
	// 0 and a log line claiming the benign case.
	lockFile, err := util.AcquireRunLock(runLockDir, cfg.SourceDomain)
	if err != nil {
		if errors.Is(err, util.ErrLockHeld) {
			trace.Info("another vmsync is already syncing this domain, skipping", "domain", cfg.SourceDomain, "error", err)
			os.Exit(0)
		}
		trace.Error("failed to acquire run lock for domain -- this is not lock contention, something is actually broken (permissions, a read-only lock directory, or the lock file being repeatedly replaced)", "domain", cfg.SourceDomain, "error", err)
		os.Exit(1)
	}
	defer lockFile.Close()

	// -ignore-external-snapshot's whole point is to skip cleanly, not just
	// sync anyway -- so this has to happen before run() is ever called: its
	// -prometheus-textfile defer is registered inside run(), and once that's
	// registered, any return path (including a deliberate early one) writes
	// a metrics record. Checked after the run lock (so only one process per
	// domain pays for the extra libvirt round trip) but before everything
	// else, so a skip here does the least possible work, same reasoning as
	// the run-lock skip above.
	if cfg.IgnoreExternalSnapshot {
		snapCount, err := libvirtsync.ExternalSnapshotCountViaReconnect(cfg.SourceURI, cfg.SourceDomain)
		if err != nil {
			trace.Warning("unable to check for existing external snapshots on source domain, proceeding with sync", "domain", cfg.SourceDomain, "error", err)
		} else if snapCount > 0 {
			trace.Info("external snapshot(s) exist on source domain and -ignore-external-snapshot is set, skipping sync", "domain", cfg.SourceDomain, "snapshot count", snapCount)
			os.Exit(0)
		}
	}

	// Consecutive failure count lives in the target domain's own vmsync
	// metadata (alongside last_checkpoint/last_sync_timestamp), not in local
	// state -- so it survives being tracked from a different host and stays
	// in one place with the rest of vmsync's bookkeeping.
	//
	// Both sides of this are gated on !verifying: a -verify=* run's failure
	// (a real mismatch, or just a transient hiccup during the compare step)
	// says nothing about whether the *incremental sync mechanism itself* is
	// broken, which is what -reinit-after-failures exists to auto-heal --
	// folding it into the same counter risks auto-discarding the checkpoint
	// chain in response to a genuine corruption finding instead of
	// surfacing it for a human to look at, and -verify runs are meant to
	// run on their own separate cadence from routine syncs anyway, so
	// mixing their failure signal into this counter conflates two
	// different schedules.
	verifying := cfg.Verify != ""
	if cfg.ReinitAfterFailures > 0 && !verifying {
		failures, err := libvirtsync.ReadTargetFailureCount(cfg.TargetURI, cfg.TargetDomain)
		if err != nil {
			trace.Warning("unable to read failure count from target metadata", "error", err)
		} else if failures >= cfg.ReinitAfterFailures {
			trace.Warning("reinit-after-failures threshold reached, forcing reinit", "consecutive_failures", failures, "threshold", cfg.ReinitAfterFailures)
			cfg.Reinit = true
			// Recorded, because a reinit nobody asked for must not be allowed
			// to discard restore points. See the sweep in run().
			cfg.ReinitAutomatic = true
		}
	}

	// run() logs the failure itself, from a defer positioned to fire just
	// before it writes the Prometheus textfile -- see that defer's own
	// comment. Logging it again here would both duplicate the line and, more
	// to the point, be too late to be counted in vmsync_error_count.
	//
	// The RecordTargetSyncFailure bookkeeping below genuinely cannot move
	// into run(): it must only happen once run() has definitively failed, by
	// which point the textfile is already written. Its own warning on failure
	// is therefore still uncounted -- an accepted, narrow gap (it reports a
	// problem writing the failure counter, not a problem with the sync
	// itself, and the sync's failure is already recorded in
	// vmsync_sync_state).
	if err := run(cfg); err != nil {
		// A role refusal is exempt for the same reason run-lock contention is
		// (see the lock's own comment above): it is not a broken sync. The
		// role gate is an administrative state, it says nothing about whether
		// the incremental mechanism works, and the reinit this counter would
		// eventually force is refused by that same gate -- so the count could
		// only ever climb. It would also do real damage: a non-zero
		// failure_count blocks promotion, so a replica deliberately paused
		// (by -update-role=paused, or by a restore, which pauses precisely so
		// the next sync does NOT overwrite what was just rolled back) would
		// slowly become unpromotable because the scheduler kept being told no.
		// The run still exits 1 and still reports vmsync_sync_state=failure:
		// what is wrong is only counting it against the domain.
		if cfg.ReinitAfterFailures > 0 && !verifying && !errors.Is(err, libvirtsync.ErrRoleRefusesSync) {
			if count, rerr := libvirtsync.RecordTargetSyncFailure(cfg.TargetURI, cfg.TargetDomain); rerr != nil {
				trace.Warning("failed to record sync failure in target metadata", "error", rerr)
			} else {
				trace.Info("recorded sync failure in target metadata", "consecutive_failures", count)
			}
		}
		os.Exit(1)
	}
	// On success, failure_count is already reset to 0 as part of the normal
	// UpdateSyncMetadata call in run() -- nothing further to do here.
}

// overlapsAnyExtent reports whether m overlaps any dirty extent in touched
// -- used by -verify=online to tell a real mismatch (outside anything the
// guest wrote during the compare window) from one that's merely
// inconclusive (inside a region the guest touched, so the target simply
// hasn't caught up with a write that happened during this compare, not
// evidence of corruption).
func overlapsAnyExtent(m nbdsync.MismatchRange, touched []nbdsync.Extent) bool {
	mEnd := m.Offset + m.Length
	for _, e := range touched {
		if !e.Dirty {
			continue
		}
		eEnd := e.Offset + e.Length
		if m.Offset < eEnd && e.Offset < mEnd {
			return true
		}
	}
	return false
}

// ErrCallTimedOut distinguishes callWithTimeout giving up on its own
// deadline from fn itself returning a real error. Only the former leaves
// fn's goroutine still potentially running in the background (see
// callWithTimeout's own doc comment) -- a caller also holding a
// *libvirt.Domain/*libvirt.Connect handle fn operated on must treat
// errors.Is(err, ErrCallTimedOut) as "this handle may still be in use by
// an abandoned goroutine" and avoid ever Free()/Close()-ing it afterward,
// or risk a native use-after-free the moment that goroutine's cgo call
// eventually does return and touches memory Go has already released. A
// real error from fn carries no such risk: fn already returned, so its
// goroutine is already exiting on its own.
var ErrCallTimedOut = errors.New("underlying goroutine abandoned, may still be running")

// callWithTimeout runs a blocking libvirt/cgo call in its own goroutine and
// gives up waiting for it after timeout. libvirt calls have no built-in
// cancellation, so a genuinely stuck call still runs to completion in the
// background (and its goroutine/OS thread with it) -- but the *caller* (the
// signal handler) is no longer blocked by it, so the rest of cleanup can
// still proceed instead of requiring a SIGKILL.
func callWithTimeout(name string, timeout time.Duration, fn func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("%s timed out after %s (%w)", name, timeout, ErrCallTimedOut)
	}
}

// verificationRan reports whether this run's metrics should include the
// vmsync_verification_state/vmsync_verification_timestamp_seconds series --
// true only when verification was both requested (verify != "") and
// actually reached the compare block (attempted), not merely requested.
// Kept as a standalone function (rather than inlined at its one call site
// in writeMetricsTextfile) specifically so this exact guarantee -- a plain
// sync or -reinit run, or a -verify run that failed before comparing
// anything, must never emit verification metrics -- is directly testable.
func verificationRan(verify string, attempted bool) bool {
	return verify != "" && attempted
}

// finalRunState decides what outcome writeMetricsTextfile should report,
// given everything run() (or the signal handler, on its behalf) knows by
// the time it's ready to write. wasInterrupted forcing StateFailure --
// ahead of and regardless of runErr/fsFreezeFailed -- is what makes
// run()'s own deferred metrics write and the signal handler's own direct
// one (metrics.StateFailure, right before os.Exit) compute the IDENTICAL
// result whenever a signal arrives: run()'s goroutine keeps executing
// independently of any signal, so it's not guaranteed to be skipped just
// because a signal handler is heading for os.Exit, and the two calls can
// genuinely race on writing the same textfile. Making them agree removes
// the race's only consequence (which one wins no longer matters, since
// both would write the same thing) instead of trying to prevent the race
// itself. An interrupted run must never be able to report success, no
// matter which write lands last.
func finalRunState(runErr error, wasInterrupted, fsFreezeFailed bool) int {
	if runErr != nil || wasInterrupted {
		return metrics.StateFailure
	}
	if fsFreezeFailed {
		return metrics.StateFSFreezeFailed
	}
	return metrics.StateSuccess
}

// unverifiableCheckpointMetadataError decides whether an unreadable or
// empty last_checkpoint field in the target domain's own metadata must
// abort the run, given the parent checkpoint (per the SOURCE's own
// checkpoint chain) this sync is about to proceed against. Returns nil
// when there's nothing to abort on -- either the metadata was read fine,
// or this is a full sync (parent == "") with no earlier checkpoint to
// verify against in the first place, so an unreadable field is merely
// advisory.
//
// For an incremental sync (parent != ""), this metadata is the ONLY thing
// standing between "the target really is at the checkpoint the source
// thinks it's incrementing from" and "silently apply a partial delta onto
// a target that might be at a completely different point in history" --
// treating it as advisory there (as this function replaces) produces an
// internally-consistent-looking but silently corrupt (mixed-history)
// target file, with the run still reporting success. Kept as a standalone
// function (mirroring verificationRan just above) specifically so this
// exact guarantee is directly testable without a live libvirt domain.
func unverifiableCheckpointMetadataError(targetDomain, parent string, checkpointParseErr error, metadataEntryCheckpoint string) error {
	if checkpointParseErr == nil && metadataEntryCheckpoint != "" {
		return nil
	}
	if parent == "" {
		return nil
	}
	reason := "the last_checkpoint metadata field is empty"
	if checkpointParseErr != nil {
		reason = fmt.Sprintf("its metadata could not be parsed: %v", checkpointParseErr)
	}
	return fmt.Errorf("incremental sync attempted but target domain %s has no verifiable last_checkpoint metadata (%s; parent checkpoint expected: %s) -- if this target was manually redefined, restored from an old XML, or is otherwise missing vmsync's own metadata, its on-disk state cannot be trusted as a base for an incremental copy; run -reinit to establish a fresh baseline", targetDomain, reason, parent)
}

// checkpointChainConsistent is unverifiableCheckpointMetadataError's
// companion guard, for the case that function's own doc comment doesn't
// cover: the target's last_checkpoint metadata was read and parsed fine,
// but names a DIFFERENT checkpoint than parent -- the checkpoint this run
// actually computed as the expected parent from the SOURCE's own
// checkpoint chain (NextCheckpointName, above in run()). That mismatch
// means the two disagree about what the target's on-disk data actually
// represents: the target thinks it's at checkpoint X, but the source's
// chain expects to be incrementing from Y. Proceeding anyway would apply
// this run's delta on top of the wrong base, producing a target that
// looks internally consistent (the sync itself reports success) but is
// silently wrong -- exactly the class of failure this whole metadata
// check exists to catch, and, before this function existed, the one guard
// in this file with no test coverage of its own: unlike
// unverifiableCheckpointMetadataError right above, it was inline in run()
// with nothing to call directly.
//
// metadataEntryCheckpoint == "" (empty or unparsable) is reported as
// consistent here -- not because there's nothing to worry about, but
// because that case is already unverifiableCheckpointMetadataError's own
// responsibility (called separately, earlier, against the same field) --
// this function's only job is judging an actual disagreement between two
// non-empty values.
func checkpointChainConsistent(metadataEntryCheckpoint, parent string) bool {
	if metadataEntryCheckpoint == "" {
		return true
	}
	return metadataEntryCheckpoint == parent
}

// listeningPorts asks a host which TCP ports are currently in LISTEN state,
// running portalloc.ListeningCommand over SSH or locally depending on
// remote. The same "ss" dependency the bridge's own readiness check already
// relies on (see nbdbridge.BuildReadinessCheckCommand), so this adds no new
// requirement to either host.
func listeningPorts(ctx context.Context, remote bool, client *remotessh.Client) (map[int]bool, error) {
	if remote {
		out, err := client.Run(ctx, portalloc.ListeningCommand)
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %s", portalloc.ListeningCommand, err, out)
		}
		return portalloc.ParseListening(out), nil
	}
	out, err := exec.CommandContext(ctx, "sh", "-c", portalloc.ListeningCommand).Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", portalloc.ListeningCommand, err)
	}
	return portalloc.ParseListening(string(out)), nil
}

// targetPortsNeeded returns how many consecutive ports a run occupies on
// the TARGET host, given its disk count and which optional stages are on.
//
// The layout is four contiguous blocks of N, each present or not, but
// always at the same offset -- copyAndCommit puts the qemu-nbd exports at
// [T, T+N) and their bridges at [T+N, T+2N); runVerify puts the read-only
// verify exports at [T+2N, T+3N) and their bridges at [T+3N, T+4N). The
// verify block sits at +2N whether or not bridging is on, so verification
// alone still reserves through 3N with the second block left idle. That is
// deliberate in the existing code (it keeps the verify export's port
// independent of the write export's, which has just been killed and may not
// have released yet), and this function must mirror it exactly or a run
// will bind outside the range it reserved.
func targetPortsNeeded(disks int, bridging, verifying bool) int {
	switch {
	case verifying && bridging:
		return 4 * disks
	case verifying:
		return 3 * disks
	case bridging:
		return 2 * disks
	default:
		return disks
	}
}

// sourcePortsNeeded returns how many consecutive ports a run occupies on
// the SOURCE host: the libvirt backup NBD export, plus its bridge helper at
// +1 when compression or buffering is on. The verify phase reuses the same
// source export rather than opening another.
func sourcePortsNeeded(bridging bool) int {
	if bridging {
		return 2
	}
	return 1
}

// refuseReinitIfTargetRunning decides whether -reinit must abort before
// touching the target's disk files, given whether the target domain
// exists and (if it does) whether it's currently running. A target that
// doesn't exist at all has nothing running to protect, and one that
// exists but is shut off is exactly the state -reinit expects -- only
// "exists AND running" must refuse: -reinit's very next step is an
// unconditional `rm -f` of the target's disk files, and doing that while
// qemu still has one open leaves the running domain serving off an
// unlinked inode, with the replica silently reverting to nothing the next
// time that domain happens to shut down -- discovered only at a real
// disaster recovery attempt, with the run itself having reported success.
// Kept as a standalone, pure function (taking plain bools, not live
// libvirt handles, which have no interface seam to fake them behind) so
// this exact guard is directly testable: inverting or short-circuiting it
// in some future refactor is the one thing standing between a normal
// -reinit and silently corrupting a live target replica.
func refuseReinitIfTargetRunning(targetDomain string, exists, running bool) error {
	if exists && running {
		return fmt.Errorf("reinit: target domain %s is running, shut it down before reinitializing", targetDomain)
	}
	return nil
}

// targetQemuOwnerOnce caches who should own the target's disks, resolved
// once per run: every disk gets the same answer, and the lookup is several
// SSH round trips for a value that cannot change mid-run.
var targetQemuOwnerOnce struct {
	sync.Once
	owner util.DiskOwner
	// candidates records what DetectQemuAccount found, so an ambiguous
	// host can be reported as ambiguous rather than as undetermined.
	candidates []string
}

// applyTargetDiskOwner gives a freshly created target disk an owner qemu can
// open, and says why it chose the one it did.
//
// The problem it exists for: vmsync creates these files by running qemu-img
// over SSH, so they belong to that SSH user -- root, realistically. qemu does
// not run as root (RHEL: `qemu`, Debian: `libvirt-qemu`), so a root-owned
// disk is one the promoted domain may be unable to open, and that is
// discovered during a failover, on the copy that was supposed to take over.
//
// libvirt's dynamic_ownership normally chowns disks as it starts a domain,
// which is why this can go unnoticed for years. It is not something to lean
// on: it is disabled in plenty of deployments, and on NFS with root_squash
// it cannot work at all -- which is exactly where a DR replica often lives.
//
// A failure to chown fails the RUN rather than being logged and shrugged off.
// The alternative is a sync that reports success and leaves behind a replica
// that cannot boot, which is the failure mode this whole function exists to
// remove.
func applyTargetDiskOwner(ctx context.Context, client *remotessh.Client, cfg syncConfig, targetPath string, replaced util.DiskOwner) error {
	owner, err := util.ParseDiskOwner(cfg.TargetDiskOwner)
	if err != nil {
		return err
	}
	if owner.IsOff() {
		return nil
	}

	if owner.Empty() {
		// auto. What owned the file before wins: it is evidence rather than
		// inference -- this pair has synced before, and that ownership is
		// what libvirt was working with.
		if !replaced.Empty() {
			owner = replaced
		} else {
			// Resolved once per run: every disk gets the same answer, and
			// this is several SSH round trips.
			targetQemuOwnerOnce.Do(func() {
				targetQemuOwnerOnce.owner = util.ReadQemuConfOwner(ctx, client)
				if !targetQemuOwnerOnce.owner.Empty() {
					return
				}
				// qemu.conf said nothing, which is the ORDINARY case: every
				// distribution ships that setting commented out, so this is
				// where a first-ever sync lands rather than an exotic
				// corner. Fall back to which well-known qemu account the
				// host actually has.
				targetQemuOwnerOnce.owner, targetQemuOwnerOnce.candidates =
					util.DetectQemuAccount(ctx, client)
			})
			owner = targetQemuOwnerOnce.owner
		}
	}

	if owner.Empty() {
		switch n := len(targetQemuOwnerOnce.candidates); {
		case n > 1:
			// Both a qemu and a libvirt-qemu account. Unusual enough that
			// picking one silently would be a worse answer than saying so.
			trace.Warning("the target host has more than one account libvirt might run qemu as, so the disk is left owned by the SSH user this ran as -- if that is root, the promoted domain may be unable to open it. Say which with -target-disk-owner",
				"disk", targetPath, "candidates", strings.Join(targetQemuOwnerOnce.candidates, ", "))
		default:
			trace.Warning("could not determine who should own the target disk, so it is left owned by the SSH user this ran as -- if that is root, the promoted domain may be unable to open it. Set -target-disk-owner (qemu:qemu on RHEL, libvirt-qemu:kvm on Debian), or set user/group in the target's /etc/libvirt/qemu.conf",
				"disk", targetPath)
		}
		return nil
	}

	if out, err := client.Run(ctx, util.ChownCommand(owner, targetPath)); err != nil {
		return fmt.Errorf("set ownership %s on target disk %s: %w: %s", owner.Spec(), targetPath, err, strings.TrimSpace(out))
	}
	trace.Info("set target disk ownership", "disk", targetPath, "owner", owner.Spec(), "from", owner.Source)
	return nil
}

func run(cfg syncConfig) (runErr error) {
	runStart := time.Now()
	defer func() {
		trace.Info("vmsync run finished", "elapsed", time.Since(runStart).Round(time.Millisecond).String(), "success", runErr == nil)
	}()

	// Resolved once, here, instead of re-deriving cfg.Verify's meaning at
	// every consumer site: verifySuspends covers both suspend-based modes
	// ("compare" and "fast"), verifyFast/verifyOnline each pick out their
	// own single mode.
	verifySuspends := cfg.Verify == "compare" || cfg.Verify == "fast"
	verifyFast := cfg.Verify == "fast"
	verifyOnline := cfg.Verify == "online"

	var tgtState bool
	var srcState bool
	// sshClientMu guards sourceSSHClient/targetSSHClient against the same
	// kind of cross-goroutine race checkpointMu/backupMu/etc. below guard
	// their own state against: the signal handler's watcher goroutine is
	// spawned (see sigCh below) well before either client is actually
	// dialed further down in run()'s own body, so cleanupSourceBridge/
	// cleanupTargetNBD (defined below, run from that goroutine) could
	// otherwise read these two pointers while run() is concurrently
	// writing them -- a genuine, race-detector-visible data race, even
	// though a signal landing in that exact narrow window is rare and,
	// when it does happen, the closures' own nil-check already does the
	// right thing (there's nothing to clean up yet). Real regardless of
	// how benign the likely outcome is, so it gets the same treatment as
	// every other shared piece of state here rather than being left as
	// the one unsynchronized pair.
	var sshClientMu sync.Mutex
	var targetSSHClient *remotessh.Client
	var sourceSSHClient *remotessh.Client
	// interruptedMu/interrupted close a race between the signal handler's
	// own metrics write (always metrics.StateFailure, right before
	// os.Exit) and run()'s own deferred one (whatever runErr/fsFreezeFailed
	// say once run() actually returns): run()'s goroutine keeps executing
	// independently of any signal, so it's not guaranteed to be skipped
	// just because a signal handler is heading for os.Exit -- the two can
	// genuinely race, each computing a DIFFERENT state and writing the
	// prometheus textfile at roughly the same time, with whichever finishes
	// last winning nondeterministically. Rather than trying to make only
	// one of them actually write (a real ordering constraint, harder to get
	// right here since the two computations are otherwise independent),
	// the signal handler sets this the moment it receives a signal, and
	// run()'s own deferred write checks it: once true, run()'s own write
	// ALSO reports failure regardless of runErr, so if they do race, both
	// end up writing the identical outcome -- an interrupted run must never
	// be able to report success, no matter which write happens to land
	// last.
	var interruptedMu sync.Mutex
	var interrupted bool
	// srcDomMu/srcDomMaybeInUse track whether any callWithTimeout call
	// touching srcDom directly has ever timed out (see ErrCallTimedOut's
	// own doc comment) -- meaning its goroutine might still be inside a
	// cgo call using srcDom when this function is otherwise ready to
	// Free() it. Checked at srcDom's own deferred Free() below: skipping
	// that call and leaking the handle for the remainder of this (already
	// exiting) process's short lifetime is the safe tradeoff, matching the
	// same abandon-rather-than-risk-a-native-use-after-free precedent
	// pkg/nbdsync's own AIO drain-timeout path already established for
	// stuck in-flight buffers.
	var srcDomMu sync.Mutex
	var srcDomMaybeInUse bool
	// defineDomainMu/defineDomainInFlight fence libvirtsync.DefineDomain's
	// own undefine-then-redefine window (near the very end of run(), once
	// the copy has already succeeded) against the signal handler's
	// unconditional os.Exit: this session's own -reinit fix already
	// narrowed that window down to two sequential libvirt calls with
	// nothing in between (down from spanning the entire, much longer copy
	// phase before), but a signal landing in the gap between them is still
	// possible in principle, and DefineDomain's own rollback machinery
	// (restoring the target's prior definition) only ever triggers on a
	// SYNCHRONOUS error one of its own steps returns -- an external
	// os.Exit gives it no chance to run at all, leaving the target
	// genuinely undefined with nothing having ever attempted to restore
	// it. Set true immediately before the call, false immediately after
	// (success or failure) -- the signal handler checks this, right before
	// its own os.Exit, and waits briefly for it to clear first.
	var defineDomainMu sync.Mutex
	var defineDomainInFlight bool
	var abortOnce sync.Once
	var backupMu sync.Mutex
	var backupActive bool = false
	var targetCleanupOnce sync.Once
	var sourceCleanupOnce sync.Once
	var resumeOnce sync.Once
	var suspendedForVerify bool
	// verifyWindowActive/verifyWindowOnce guard the ephemeral verify-window
	// checkpoint + its own short-lived backup job (see beginVerifyWindow,
	// defined further down once qcowDisks/backupMu are in scope) the same
	// way backupActive/abortOnce guard the regular backup job -- only ever
	// meaningful when -verify=online actually reaches that step.
	var verifyWindowMu sync.Mutex
	var verifyWindowActive bool
	var verifyWindowOnce sync.Once
	var stopMu sync.Mutex
	targetStopCommands := make([]string, 0)
	sourceStopCommands := make([]string, 0)
	// checkpointMu guards checkpointName/checkpointAdvanced against
	// concurrent access from outside run()'s own goroutine -- specifically
	// cleanupOrphanedCheckpoint (defined further down), which decides
	// whether there's a checkpoint to clean up and is called from both
	// run()'s own deferred cleanup AND the signal handler's goroutine.
	// Every other read/write of these two happens sequentially within
	// run()'s own goroutine (checkpoint creation runs entirely before the
	// per-disk goroutines are spawned, and the end-of-run reads happen
	// after wg.Wait() has already joined them), so only the two writes
	// below and cleanupOrphanedCheckpoint's own read actually need it.
	// checkpointCleanupOnce guards cleanupOrphanedCheckpoint's actual
	// execution to exactly once regardless of which of its two callers
	// gets there first -- see that closure's own doc comment for why this
	// used to be two separate, duplicated pieces of logic instead of one,
	// and why that duplication was itself the bug: run()'s own goroutine
	// keeps executing independently of any signal, so its own deferred
	// cleanup is not guaranteed to be skipped just because the signal
	// handler is heading for os.Exit -- the two can genuinely run
	// concurrently, and a signal landing between CreateCheckpoint
	// succeeding and checkpointAdvanced being set could make an unguarded
	// reader observe a stale "not advanced", skip deleting a checkpoint
	// that genuinely exists now, and leave it orphaned for the next run to
	// trip over.
	var checkpointMu sync.Mutex
	var checkpointCleanupOnce sync.Once
	var checkpointName string
	var parent string
	// checkpointAdvanced is true once CreateCheckpoint below has actually
	// created checkpointName in libvirt. It stays false for the rest of run()
	// when checkpoint creation was skipped because an external snapshot
	// blocked it (see IsCheckpointBlockedBySnapshot) -- in that case the sync
	// still proceeds incrementally against the existing parent checkpoint,
	// but nothing downstream (parent cleanup, checkpoint-on-failure cleanup,
	// target metadata) should treat checkpointName as if it exists.
	var checkpointAdvanced bool
	// dataCopySucceeded is set (under checkpointMu, alongside checkpointName/
	// checkpointAdvanced above) once every disk has actually finished
	// copying (and verifying, if requested) without error -- specifically
	// BEFORE the purely post-copy bookkeeping steps that follow (deleting
	// the now-superseded parent checkpoint, redefining the target domain).
	// Both the deferred checkpoint-delete-on-failure cleanup further down
	// and the signal handler's own mirrored copy of it must check this: a
	// failure in one of those bookkeeping steps still makes runErr non-nil,
	// but the checkpoint just created correctly reflects data that DID
	// finish copying successfully, and deleting it anyway would discard a
	// perfectly good incremental baseline, forcing a needless full resync
	// next run over what was, in the ways that actually matter, a
	// successful sync.
	var dataCopySucceeded bool
	// copyCommitted is closed at the exact same moment dataCopySucceeded is
	// set to true, below. A single snapshot read of dataCopySucceeded (as
	// the signal handler's checkpoint-delete goroutine used to rely on
	// exclusively) is not enough on its own to decide whether to delete the
	// checkpoint: run()'s goroutine keeps executing independently of the
	// signal handler regardless of what signal arrived, so a signal landing
	// in the handful of Go statements between the disk-copy loop exiting
	// and dataCopySucceeded actually being flipped to true would be read as
	// "copy not done", and the handler would delete a checkpoint that, a
	// moment later, run() commits to (deletes the parent checkpoint,
	// writes this one's name into target metadata) -- permanently
	// destroying the incremental chain with no recovery short of a full
	// resync. Waiting on this channel (with a short bounded timeout as a
	// fallback for the case where the copy is genuinely still running) lets
	// the handler react to the actual transition instead of a stale
	// snapshot of it.
	copyCommitted := make(chan struct{})
	// freezeMu guards freezed against the signal handler's own goroutine
	// (thawSource, defined further down), exactly like checkpointMu guards
	// checkpointName/checkpointAdvanced above and for the same reason: a
	// signal landing between FSFreeze succeeding (freezed set true) and
	// FSThaw actually running a few statements later must be able to see
	// that the guest is frozen right now, not a stale pre-freeze snapshot
	// -- otherwise the interrupt-cleanup path skips thawing entirely and
	// the production guest's filesystem stays frozen indefinitely, with no
	// recovery short of an operator noticing and running virsh
	// domfsthaw/fsfreeze-thaw by hand.
	var freezeMu sync.Mutex
	var freezed bool = false
	var thawOnce sync.Once
	var fsFreezeFailed bool = false
	var started bool = false
	var metricsMu sync.Mutex
	diskMetrics := make([]metrics.DiskMetric, 0)
	var nbdHost, targetNBDHost string
	// Resolved immediately (rather than deferred until the backup job
	// actually starts, as this used to be) purely from cfg -- no network
	// calls, nothing that depends on anything fallible happening first --
	// so vmsync_sync_state's source_host/target_host labels are populated
	// even when run() fails before ever reaching the backup job, instead of
	// silently writing empty label values for the earliest-failing runs.
	metricsMu.Lock()
	nbdHost = cfg.SourceNBDHost
	if nbdHost == "" {
		nbdHost = util.ConnectHostFromBindOrURI(cfg.SourceNBDBind, cfg.SourceURI)
	}
	targetNBDHost = cfg.TargetNBDHost
	if targetNBDHost == "" {
		targetNBDHost = util.ConnectHostFromBindOrURI(cfg.TargetNBDBind, cfg.TargetURI)
	}
	metricsMu.Unlock()
	// externalSnapshotCount backs the vmsync_external_snapshot_count metric
	// -- guarded by metricsMu like nbdHost/targetNBDHost above, since
	// writeMetricsTextfile can run concurrently from the signal handler.
	var externalSnapshotCount int
	// verificationAttempted is set once the -verify compare block is
	// actually entered for at least one disk (see syncDisk below) --
	// distinct from cfg.Verify, which is just "was -verify requested" and
	// stays true even when the run fails before ever reaching that block
	// (an early SSH/libvirt error, say). Without this, vmsync_verification_
	// timestamp_seconds would get bumped to "now" on every such early
	// failure, masking real staleness: a persistently failing run could
	// look like it's verifying successfully-recently when no comparison
	// has actually been attempted in a long time. Deliberately does NOT
	// change how a run's overall failure is reported otherwise -- a
	// mismatch, or any later unrelated failure after a passing compare,
	// still fails the whole run via the existing state/runErr handling.
	var verificationAttempted bool

	// writeMetricsTextfile is called from two places: the deferred call
	// below (the normal return path, any outcome) and the signal handler
	// further down (which calls os.Exit directly on a forced shutdown --
	// os.Exit skips every deferred function unconditionally, by Go's own
	// design, so that defer would otherwise never run at all on Ctrl+C/
	// SIGTERM). The normal path only ever runs after wg.Wait() has already
	// joined every per-disk goroutine, so reading diskMetrics/nbdHost/
	// targetNBDHost there needs no lock -- but the signal handler
	// deliberately does NOT wait for those goroutines (a wedged libnbd call
	// can't be interrupted), so it can genuinely run concurrently with a
	// syncDisk goroutine still appending to diskMetrics under metricsMu, or
	// even before nbdHost/targetNBDHost are assigned at all. metricsMu (already
	// used for the diskMetrics append) guards all three here so both call
	// sites are race-free regardless of which one gets there first.
	writeMetricsTextfile := func(state int) {
		if cfg.PrometheusTextfile == "" {
			return
		}
		metricsMu.Lock()
		disksSnapshot := append([]metrics.DiskMetric(nil), diskMetrics...)
		sourceHost, targetHost := nbdHost, targetNBDHost
		snapshotCount := externalSnapshotCount
		attempted := verificationAttempted
		metricsMu.Unlock()
		now := time.Now().Unix()
		run := metrics.RunMetric{
			SourceHost:            sourceHost,
			TargetHost:            targetHost,
			VM:                    cfg.SourceDomain,
			State:                 state,
			Timestamp:             now,
			ExternalSnapshotCount: snapshotCount,
			// trace's own counters, not metricsMu-guarded state -- they're
			// already safe for concurrent reads on their own (atomics), and
			// reflect the whole process's lifetime by the time any call to
			// writeMetricsTextfile (run()'s own deferred one, or the signal
			// handler's) reads them here.
			WarningCount: trace.WarningCount(),
			ErrorCount:   trace.ErrorCount(),
			// VerificationRan requires the compare block to have actually
			// been entered this run (see verificationAttempted's own
			// comment) -- cfg.Verify alone would stay true even for a run
			// that failed before ever reaching it, which would otherwise
			// bump VerificationTimestamp to "now" on every such failure and
			// mask real staleness. A -verify mismatch, or any later
			// unrelated failure after a passing compare, still fails the
			// whole run via state/runErr as before -- this only changes
			// whether the verification metrics are emitted at all, not
			// what the overall run's own failure means.
			VerificationRan:       verificationRan(cfg.Verify, attempted),
			VerificationState:     state,
			VerificationTimestamp: now,
		}
		if err := metrics.WriteTextfile(cfg.PrometheusTextfile, disksSnapshot, run); err != nil {
			trace.Warning("failed to write prometheus textfile", "path", cfg.PrometheusTextfile, "error", err)
		}
	}
	// Registered here, before anything that can fail (SSH setup, checkpoint/
	// backup calls, per-disk syncs, ...), so a run that never gets anywhere
	// near the disk loop still reports failure -- otherwise a sync that dies
	// during early setup would silently never touch the textfile at all,
	// leaving monitoring blind to it. Fires last, once runErr holds its
	// final value -- vmsync_sync_state reflects the whole run's outcome, not
	// any single disk's, since a later step (checkpoint cleanup, metadata
	// update, DefineDomain) can still fail after every disk has already
	// synced successfully.
	defer func() {
		interruptedMu.Lock()
		wasInterrupted := interrupted
		interruptedMu.Unlock()
		writeMetricsTextfile(finalRunState(runErr, wasInterrupted, fsFreezeFailed))
	}()
	// Registered immediately after the metrics defer above specifically so it
	// runs immediately BEFORE it: defers are LIFO, so the later something is
	// registered here, the earlier it runs. This has to be logged while
	// writeMetricsTextfile can still observe it, because vmsync_error_count
	// is read from trace.ErrorCount() at that moment.
	//
	// This used to live in main()'s own post-run() error branch, which is too
	// late: run() has already written the textfile by the time it returns, so
	// a failure during early setup (SSH dial, URI parsing, flag validation)
	// -- none of which log an error of their own, they just return one --
	// produced vmsync_error_count 0 sitting next to a failure
	// vmsync_sync_state, and any alert built on that counter missed the run
	// entirely.
	//
	// Every other defer in run() is registered after this one and therefore
	// runs before it, so errors logged during cleanup (a failed qemu-nbd
	// teardown, a failed bridge stop) are still counted here too. The signal
	// handler's own path doesn't reach this at all -- it calls
	// writeMetricsTextfile(StateFailure) and os.Exit(1) directly, bypassing
	// every defer, which is deliberate and unchanged.
	defer func() {
		if runErr != nil {
			trace.Error("sync failed", "error", runErr)
		}
	}()

	netbufferBlock, netbufferSize, err := nbdbridge.ParseNetBufferSpec(cfg.NetBuffer)
	if err != nil {
		return err
	}
	bridgeCfg := nbdbridge.Config{
		Compress:       cfg.Compress != "",
		CompressLevel:  cfg.CompressLevel,
		CompressAlgo:   cfg.Compress,
		NetBufferBlock: netbufferBlock,
		NetBufferSize:  netbufferSize,
		HelperPath:     cfg.BridgeHelperPath,
		UseSSH:         cfg.UseSSH,
	}
	if err := nbdbridge.CheckLocal(bridgeCfg); err != nil {
		return err
	}
	if bridgeCfg.Enabled() {
		if util.UriUsesSSH(cfg.SourceURI) && cfg.SourceNBDHost != "" {
			if uriHost := util.HostFromURIOrLocal(cfg.SourceURI); cfg.SourceNBDHost != uriHost {
				return fmt.Errorf("--compress/--netbuffer require --source-nbd-host to match the host in --source-uri (got %s, expected %s): the remote bridge only forwards to 127.0.0.1 on that same host", cfg.SourceNBDHost, uriHost)
			}
		}
		if cfg.TargetNBDHost != "" {
			if uriHost := util.HostFromURIOrLocal(cfg.TargetURI); cfg.TargetNBDHost != uriHost {
				return fmt.Errorf("--compress/--netbuffer require --target-nbd-host to match the host in --target-uri (got %s, expected %s): the remote bridge only forwards to 127.0.0.1 on that same host", cfg.TargetNBDHost, uriHost)
			}
		}
	}

	trace.Info(fmt.Sprintf("%s, Version: %s", os.Args, version.Version))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srcMgr, err := libvirtsync.Connect(cfg.SourceURI)
	if err != nil {
		return err
	}
	defer srcMgr.Close()
	srcLibvirtVersion, _ := srcMgr.Conn.GetVersion()
	trace.Info("Connected to source libvirt", "version", srcLibvirtVersion)

	tgtMgr, err := libvirtsync.Connect(cfg.TargetURI)
	if err != nil {
		return err
	}
	defer tgtMgr.Close()
	tgtLibvirtVersion, _ := tgtMgr.Conn.GetVersion()
	trace.Info("Connected to target libvirt", "version", tgtLibvirtVersion)

	// Checked here, as early as the target connection allows and well
	// before -reinit's own disk removal (or anything else that writes to
	// the target), because this is the guard that has to hold when the
	// runtime-state ones cannot: a domain promoted by a failover and then
	// shut down for maintenance looks exactly like an ordinary idle target
	// to refuseReinitIfTargetRunning and to DefineDomain's active re-check.
	// See MetadataFieldReplicationRole and TargetRoleAllowsSync for the
	// full reasoning, including why an absent role is permitted.
	targetRole, err := libvirtsync.ReadReplicationRole(tgtMgr, cfg.TargetDomain)
	if err != nil {
		return fmt.Errorf("read target domain replication role: %w", err)
	}
	if err := libvirtsync.TargetRoleAllowsSync(targetRole); err != nil {
		return fmt.Errorf("refusing to sync into %s: %w", cfg.TargetDomain, err)
	}
	if targetRole != "" {
		trace.Debug("target replication role permits this sync", "vm", cfg.TargetDomain, "role", targetRole)
	}

	srcDom, err := srcMgr.LookupDomain(cfg.SourceDomain)
	if err != nil {
		return err
	}
	// Conditional, not a bare defer srcDom.Free(): see srcDomMu/
	// srcDomMaybeInUse's own doc comment above. Every signal-handler
	// cleanup closure below that calls callWithTimeout directly on srcDom
	// goes through callOnSrcDom instead, specifically so a timeout there
	// is reflected here before this ever runs.
	defer func() {
		srcDomMu.Lock()
		maybeInUse := srcDomMaybeInUse
		srcDomMu.Unlock()
		if maybeInUse {
			trace.Warning("a prior call against the source domain handle timed out and its goroutine was abandoned -- leaking the handle instead of calling Free() on it, to avoid a native use-after-free if that goroutine is still running")
			return
		}
		srcDom.Free()
	}()
	// callOnSrcDom wraps callWithTimeout for the cleanup calls below that
	// touch srcDom directly (as opposed to the "via reconnect" fallbacks,
	// which open their own separate, self-contained connection and so
	// don't put srcDom itself at risk): a timeout here means srcDom may
	// still be in use by an abandoned goroutine, recorded via
	// srcDomMaybeInUse so the deferred Free() above knows to leak rather
	// than risk a native use-after-free.
	callOnSrcDom := func(name string, fn func() error) error {
		err := callWithTimeout(name, 5*time.Second, fn)
		if errors.Is(err, ErrCallTimedOut) {
			srcDomMu.Lock()
			srcDomMaybeInUse = true
			srcDomMu.Unlock()
		}
		return err
	}

	if srcState, err = libvirtsync.DomainActive(srcDom); err != nil {
		return err
	}

	// Unconditional, regardless of whether -verify=online is requested this
	// run: self-heals a verify-window checkpoint left behind by a prior
	// -verify=online invocation that crashed (e.g. SIGKILL) before its own
	// cleanup ran. Cheap (one lookup, delete only if found), and safe to run
	// even when -verify=online was never used -- AcquireRunLock already
	// rules out any concurrent-run hazard for this domain. See
	// VerifyWindowCheckpointName's own doc comment for why this can never
	// collide with or confuse the regular checkpoint chain.
	if err := libvirtsync.DeleteVerifyWindowCheckpoint(srcDom); err != nil {
		trace.Warning("failed to clean up leftover verify-online window checkpoint from a prior run", "error", err)
	}

	onExit := func() {
		abortOnce.Do(func() {
			if started {
				trace.Info("destroying vm as it was started by sync process")
				srcDom.Destroy()
			}
		})
	}

	if srcState == false {
		if cfg.Start {
			trace.Info("Starting VM in paused mode")
			if err := srcDom.CreateWithFlags(libvirt.DOMAIN_START_PAUSED); err != nil {
				return fmt.Errorf("unable to start domain %s in paused mode: %s", cfg.SourceDomain, err)
			}
			started = true
			defer onExit()
		} else {
			return fmt.Errorf("source domain %s is inactive require running state before sync (or use -start option)", cfg.SourceDomain)
		}
	}

	// -verify's whole point is a byte-for-byte compare against a source that
	// cannot change out from under it -- a domain that's merely active
	// (DomainActive, used above) isn't enough, since a paused domain's disk
	// is already static and a running one's isn't. Suspend only when
	// genuinely running; a domain already paused (including one -start just
	// started, which starts it paused) needs no action of our own.
	if verifySuspends {
		state, _, err := srcDom.GetState()
		if err != nil {
			return fmt.Errorf("check source domain state for -verify: %w", err)
		}
		if state == libvirt.DOMAIN_RUNNING {
			trace.Info("verify: suspending source VM for the duration of this sync", "vm", cfg.SourceDomain)
			if err := srcDom.Suspend(); err != nil {
				return fmt.Errorf("verify: suspend source domain %s: %w", cfg.SourceDomain, err)
			}
			suspendedForVerify = true
		}
	}
	resumeSource := func(trigger string) {
		resumeOnce.Do(func() {
			if !suspendedForVerify {
				return
			}
			resumeErr := callOnSrcDom("resume source vm", func() error {
				return srcDom.Resume()
			})
			if resumeErr != nil {
				// Unlike abortBackup/cleanupVerifyWindow/the checkpoint
				// cleanup, this used to have no reconnect fallback at all --
				// despite being the most availability-critical of the four:
				// a leftover backup job or checkpoint is an annoyance the
				// next run can clean up or route around, but a source stuck
				// paused (because the primary connection happened to be
				// wedged/stale at exactly the wrong moment) stays paused
				// indefinitely, with production traffic to it stopped, until
				// an operator notices and resumes it by hand.
				trace.Error("resume source vm failed on primary connection", "trigger", trigger, "error", resumeErr)
				retryErr := callWithTimeout("resume source vm via reconnect", 5*time.Second, func() error {
					return libvirtsync.ResumeDomainViaReconnect(cfg.SourceURI, cfg.SourceDomain)
				})
				if retryErr != nil {
					trace.Error("resume source vm retry via reconnect also failed", "trigger", trigger, "error", retryErr)
				} else {
					trace.Info("verify: resumed source VM via reconnect", "trigger", trigger, "vm", cfg.SourceDomain)
				}
			} else {
				trace.Info("verify: resumed source VM", "trigger", trigger, "vm", cfg.SourceDomain)
			}
		})
	}
	defer resumeSource("cleanup")

	abortBackup := func(trigger string) {
		abortOnce.Do(func() {
			// wasActive gates only the backup-stop logic immediately below,
			// not the whole closure -- this used to be an early return
			// ("if !backupActive { unlock; return }"), which, being inside
			// abortOnce.Do, didn't just skip the backup-stop step for this
			// one call: it consumed abortOnce for good, permanently
			// skipping the unrelated "destroy the VM this run itself
			// started" step further down too, for the rest of the process's
			// lifetime. A signal landing before the backup job ever starts
			// (backupActive still false) but after a -start'd VM is already
			// running would hit exactly that: abortBackup "runs" once,
			// finds no backup to stop, returns immediately, and the started
			// VM is never destroyed -- leaking a running VM the operator
			// never asked to keep. These two cleanup duties are unrelated
			// and must not be able to suppress each other just because they
			// happen to share one Once-guarded closure.
			backupMu.Lock()
			wasActive := backupActive
			backupActive = false
			backupMu.Unlock()
			if wasActive {
				trace.Info("stopping libvirt backup job", "trigger", trigger)
				stopErr := callOnSrcDom("abort backup job", func() error {
					return libvirtsync.StopBackup(srcDom)
				})
				if stopErr != nil {
					trace.Error("stop backup job failed on primary connection", "trigger", trigger, "error", stopErr)
					retryErr := callWithTimeout("stop backup job via reconnect", 5*time.Second, func() error {
						return libvirtsync.StopBackupViaReconnect(cfg.SourceURI, cfg.SourceDomain)
					})
					if retryErr != nil {
						trace.Error("stop backup retry via reconnect also failed", "trigger", trigger, "error", retryErr)
					}
				}
			}
			if started {
				trace.Info("destroying vm as it was started by sync process")
				if destroyErr := callOnSrcDom("destroy vm", func() error {
					return srcDom.Destroy()
				}); destroyErr != nil {
					trace.Error("destroy vm timed out or failed", "trigger", trigger, "error", destroyErr)
				}
			}
		})
	}
	// cleanupVerifyWindow tears down the ephemeral verify-window checkpoint
	// and its own short-lived backup job (see beginVerifyWindow) -- mirrors
	// abortBackup's shape exactly (sync.Once, callWithTimeout, reconnect-
	// retry fallback), since it's the same kind of "must not leak this
	// libvirt-side state past this run" concern. A no-op whenever
	// verifyWindowActive was never set (i.e. -verify=online never reached
	// that step this run, including whenever it's not requested at all).
	//
	// abortBackup and cleanupVerifyWindow both stop the SAME underlying
	// backup job (beginVerifyWindow reuses backupActive/backupMu across the
	// handoff from the regular job to the verify-window one -- see its own
	// comment), and both run concurrently from the signal handler's
	// parallel cleanup goroutines. This claims responsibility for that stop
	// through backupActive/backupMu exactly like abortBackup already does,
	// rather than calling StopBackup unconditionally: whichever of the two
	// closures flips backupActive false first is the one that actually
	// calls it and logs about it, and the other -- seeing it already false
	// -- skips both. StopBackup's own job-stats-based check would have
	// made a redundant second call a harmless no-op regardless (when no
	// job or the same backup job is still running), but this avoids the
	// redundant call (and the confusing "stopping libvirt backup job" log
	// line that would come with it) entirely, instead of relying on that
	// as the only safety net. The checkpoint deletion below
	// is unconditional either way -- it's this closure's own, unique
	// responsibility, regardless of which closure happened to stop the job.
	cleanupVerifyWindow := func(trigger string) {
		verifyWindowOnce.Do(func() {
			verifyWindowMu.Lock()
			active := verifyWindowActive
			verifyWindowMu.Unlock()
			if !active {
				return
			}
			trace.Info("removing verify-online window checkpoint", "trigger", trigger)

			backupMu.Lock()
			shouldStop := backupActive
			backupActive = false
			backupMu.Unlock()
			if shouldStop {
				stopErr := callOnSrcDom("stop verify-window backup job", func() error {
					return libvirtsync.StopBackup(srcDom)
				})
				if stopErr != nil {
					trace.Error("failed to stop verify-window backup job", "trigger", trigger, "error", stopErr)
				}
			}

			delErr := callOnSrcDom("delete verify-window checkpoint", func() error {
				return libvirtsync.DeleteVerifyWindowCheckpoint(srcDom)
			})
			if delErr != nil {
				trace.Error("failed to delete verify-window checkpoint on primary connection", "trigger", trigger, "error", delErr)
				retryErr := callWithTimeout("delete verify-window checkpoint via reconnect", 5*time.Second, func() error {
					return libvirtsync.DeleteCheckpointViaReconnect(cfg.SourceURI, cfg.SourceDomain, libvirtsync.VerifyWindowCheckpointName)
				})
				if retryErr != nil {
					trace.Error("failed to delete verify-window checkpoint via reconnect", "trigger", trigger, "error", retryErr)
				}
			}
		})
	}
	// Registered right away: cleanupVerifyWindow itself checks
	// verifyWindowActive at the time it actually runs, so this is a no-op
	// for every run that never reaches (or doesn't use) -verify=online's
	// beginVerifyWindow step further down, and the real backstop for one
	// that does but fails or gets interrupted partway through.
	defer cleanupVerifyWindow("cleanup")

	// pollStopCommands repeatedly drains newly-appended entries from a
	// stop-command list instead of taking one instant snapshot -- per-disk
	// workers run in parallel and each only registers its own remote
	// process's stop command once that process has actually started, which
	// can happen at any point during the run. A single snapshot taken the
	// moment an interrupt arrives would silently miss any command
	// registered a moment later by a worker that was still mid-startup (or
	// hadn't reached that step yet for a slower disk), leaving that
	// specific remote qemu-nbd/bridge process running forever with nothing
	// left to ever stop it. Polling for entries new since the last check,
	// for up to maxWait but giving up early once two consecutive checks
	// find nothing new, catches stragglers without either missing them or
	// waiting the full duration on the common case of nothing left to
	// clean up.
	//
	// Runs everything found in REGISTRATION order within each batch, not
	// reverse: for each disk, the qemu-nbd export holding that disk's own
	// write lock is registered first, its bridge helper (if any) second --
	// registration order attempts the lock-holder first, reverse order
	// would attempt it last. Each command already gets its own independent
	// timeout below (a slow or hung one no longer eats into a shared
	// budget the way one pre-existing bytes-shared context used to), but
	// they still run one at a time, in sequence, within a batch -- if
	// something cuts this short regardless (an operator's second Ctrl+C
	// forcing an immediate exit, say), attempting the disk-lock holder
	// first means it's the one most likely to have actually been reached,
	// not the bridge helper sitting in front of it. A stray bridge helper
	// left running is a harmless orphaned network listener; a stray
	// qemu-nbd export left running holds the disk file open, blocking a
	// future -reinit's rm -f or the next sync's own attempt to reopen it.
	pollStopCommands := func(list *[]string, maxWait time.Duration, run func(cmd string)) {
		processed := 0
		deadline := time.Now().Add(maxWait)
		quietRounds := 0
		for {
			stopMu.Lock()
			pending := append([]string(nil), (*list)[processed:]...)
			processed = len(*list)
			stopMu.Unlock()

			if len(pending) == 0 {
				quietRounds++
			} else {
				quietRounds = 0
			}
			for _, cmd := range pending {
				run(cmd)
			}
			if quietRounds >= 2 || time.Now().After(deadline) {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	cleanupTargetNBD := func(trigger string) {
		targetCleanupOnce.Do(func() {
			sshClientMu.Lock()
			client := targetSSHClient
			sshClientMu.Unlock()
			if client == nil {
				return
			}
			pollStopCommands(&targetStopCommands, 5*time.Second, func(cmd string) {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer ccancel()
				if out, err := client.Run(cctx, cmd); err != nil {
					trace.Error("failed to stop target qemu-nbd export", "trigger", trigger, "error", err, "output", out)
				}
			})
		})
	}
	cleanupSourceBridge := func(trigger string) {
		sourceCleanupOnce.Do(func() {
			sshClientMu.Lock()
			client := sourceSSHClient
			sshClientMu.Unlock()
			if client == nil {
				return
			}
			pollStopCommands(&sourceStopCommands, 5*time.Second, func(cmd string) {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer ccancel()
				if out, err := client.Run(cctx, cmd); err != nil {
					trace.Error("failed to stop source nbd bridge", "trigger", trigger, "error", err, "output", out)
				}
			})
		})
	}
	// thawSource is the interrupt-cleanup counterpart to the filesystem
	// freeze taken further down, immediately before checkpoint creation.
	// Without this, a signal landing in that window -- freeze succeeded,
	// checkpoint creation still in flight -- had no cleanup step that even
	// knew the guest was frozen, let alone one that thawed it: none of
	// abortBackup/cleanupVerifyWindow/cleanupTargetNBD/cleanupSourceBridge/
	// resumeSource touch the filesystem-freeze state at all. The result was
	// a production guest left frozen indefinitely (until an operator
	// happens to notice and runs virsh fsfreeze-thaw by hand), for
	// something the guest agent itself has no timeout to recover from on
	// its own. thawOnce guards actual execution; freezeMu guards the
	// cross-goroutine read of freezed itself (see its own declaration).
	// Clearing freezed under the same lock right away means whichever of
	// the two triggers -- run()'s own normal-path calls further down, or
	// this closure firing from the signal handler -- gets there first is
	// also the only one that logs/acts, avoiding a confusing redundant
	// thaw attempt if both happen to race.
	thawSource := func(trigger string) {
		thawOnce.Do(func() {
			freezeMu.Lock()
			wasFrozen := freezed
			freezed = false
			freezeMu.Unlock()
			if !wasFrozen {
				return
			}
			trace.Info("thawing source filesystem", "trigger", trigger)
			if err := callOnSrcDom("thaw source filesystem", func() error {
				libvirtsync.ThawFs(srcDom, true)
				return nil
			}); err != nil {
				trace.Error("thaw source filesystem timed out", "trigger", trigger, "error", err)
			}
		})
	}

	// cleanupOrphanedCheckpoint deletes checkpointName if, and only if, it
	// was actually created this run (checkpointAdvanced) but the data copy
	// it's meant to be the baseline for never finished (!dataCopySucceeded)
	// -- otherwise a failed or interrupted run leaves behind a checkpoint
	// the next run would wrongly trust as a valid incremental baseline.
	//
	// This used to be two separate, hand-duplicated pieces of logic: one
	// here as a defer in run()'s own flow, another inline in the signal
	// handler's goroutine below, on the reasoning that "os.Exit skips
	// defers, so only one of them ever actually runs." That reasoning does
	// not hold: os.Exit only skips run()'s defer if it fires *before*
	// run() returns, but nothing stops run()'s own goroutine from
	// returning on its own (noticing ctx cancellation, or a resource the
	// signal handler already tore down failing an in-flight call) while
	// the signal handler's goroutine is still mid-cleanup, heading for its
	// own os.Exit. In that overlap, both used to read
	// checkpointName/checkpointAdvanced/dataCopySucceeded independently --
	// one of them without checkpointMu at all -- and could reach different
	// conclusions about the exact same checkpoint from a torn or stale
	// read, including the read that decides *not* to delete when deletion
	// was actually warranted: precisely the "orphaned checkpoint" outcome
	// the old comment on the signal-handler copy dismissed as prevented.
	//
	// Unifying into one checkpointCleanupOnce-guarded closure, callable
	// from both places, closes this off completely instead of narrowing
	// the window: whichever caller gets here first is the only one that
	// actually evaluates anything.
	cleanupOrphanedCheckpoint := func(trigger string) {
		checkpointCleanupOnce.Do(func() {
			// Snapshot all three under checkpointMu, once, rather than
			// reading each separately -- a signal (or run()'s own defer)
			// landing between CreateCheckpoint succeeding and
			// checkpointAdvanced being set must see either the
			// fully-pre-checkpoint state or the fully-post-checkpoint
			// state, never a stale mix of the two.
			checkpointMu.Lock()
			name, advanced, copySucceeded := checkpointName, checkpointAdvanced, dataCopySucceeded
			checkpointMu.Unlock()
			// A snapshot showing copySucceeded=false is not trustworthy on
			// its own when called from the signal handler: run()'s
			// goroutine keeps executing regardless of the signal, and
			// might be a handful of Go statements away from flipping it to
			// true (see copyCommitted's own doc comment for exactly why).
			// Wait briefly for that transition before concluding the copy
			// is still genuinely in progress. Called from run()'s own
			// defer, run() has already fully returned by this point, so
			// dataCopySucceeded is already final and copyCommitted (if the
			// copy did succeed) is already closed -- this is then an
			// instant, harmless no-op rather than a real wait.
			if advanced && !copySucceeded {
				select {
				case <-copyCommitted:
					copySucceeded = true
				case <-time.After(200 * time.Millisecond):
				}
			}
			if name == "" || !advanced || copySucceeded {
				return
			}
			if err := callOnSrcDom("delete checkpoint", func() error {
				return libvirtsync.DeleteCheckpointIfExists(srcDom, name)
			}); err != nil {
				trace.Error("failed to delete checkpoint", "trigger", trigger, "checkpoint", name, "error", err)
				retryErr := callWithTimeout("delete checkpoint via reconnect", 5*time.Second, func() error {
					return libvirtsync.DeleteCheckpointViaReconnect(cfg.SourceURI, cfg.SourceDomain, name)
				})
				if retryErr != nil {
					trace.Error("failed to delete checkpoint via reconnect", "trigger", trigger, "checkpoint", name, "error", retryErr)
				} else {
					trace.Info("removed checkpoint via reconnect path", "trigger", trigger, "checkpoint", name)
				}
			} else {
				trace.Info("removed checkpoint", "trigger", trigger, "checkpoint", name)
			}
		})
	}

	sigCh := make(chan os.Signal, 1)
	doneCh := make(chan struct{})
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	defer close(doneCh)
	go func() {
		select {
		case sig := <-sigCh:
			trace.Info("received signal", "signal", sig.String())
			interruptedMu.Lock()
			interrupted = true
			interruptedMu.Unlock()
			// A second signal arriving while the cleanup below is still
			// running (each of its reconnect fallbacks is now bounded, but
			// "bounded" still means up to ~10s each, run in parallel below
			// -- not instant) used to be silently swallowed: this goroutine
			// had already left its own select on sigCh and gone on to run
			// cleanup inline, never coming back to read sigCh again, so an
			// operator's second Ctrl+C/SIGTERM -- a completely reasonable
			// reaction to a process that looks stuck -- did nothing at all,
			// leaving kill -9 from another terminal as the only way to
			// actually force an exit (skipping this cleanup entirely,
			// instead of just skipping the wait for it). This watcher gives
			// a second signal real effect: an immediate, unconditional
			// exit, same as a determined operator would reach for anyway.
			go func() {
				if sig2, ok := <-sigCh; ok {
					trace.Warning("received second signal during cleanup, forcing immediate exit without waiting for it to finish", "signal", sig2.String())
					os.Exit(1)
				}
			}()
			// These touch independent connections (source libvirt, target
			// SSH) with no dependency on each other, so run them
			// concurrently -- worst-case wait is the slowest ONE of them,
			// not their sum, before the process can actually exit.
			var cleanupWg sync.WaitGroup
			cleanupWg.Add(7)
			go func() { defer cleanupWg.Done(); abortBackup(sig.String()) }()
			go func() { defer cleanupWg.Done(); cleanupVerifyWindow(sig.String()) }()
			go func() { defer cleanupWg.Done(); cleanupTargetNBD(sig.String()) }()
			go func() { defer cleanupWg.Done(); cleanupSourceBridge(sig.String()) }()
			go func() { defer cleanupWg.Done(); resumeSource(sig.String()) }()
			go func() { defer cleanupWg.Done(); thawSource(sig.String()) }()
			go func() { defer cleanupWg.Done(); cleanupOrphanedCheckpoint(sig.String()) }()
			cleanupWg.Wait()
			cancel()

			// Give DefineDomain's own undefine-then-redefine window (see
			// defineDomainInFlight's own doc comment for why it's narrow
			// but not zero) a brief, bounded chance to finish on its own
			// before force-exiting through it -- its own rollback
			// machinery already handles a synchronous failure correctly,
			// but only os.Exit landing *outside* this window lets it ever
			// run at all. Two sequential libvirt RPCs finish in a small
			// fraction of this budget in the overwhelmingly common case,
			// so this adds negligible delay to a normal interrupt; it
			// exists entirely for the rare case where a signal happens to
			// land inside the window.
			defineDomainWaitDeadline := time.Now().Add(2 * time.Second)
			for {
				defineDomainMu.Lock()
				inFlight := defineDomainInFlight
				defineDomainMu.Unlock()
				if !inFlight || time.Now().After(defineDomainWaitDeadline) {
					if inFlight {
						trace.Warning("timed out waiting for the target domain redefine to finish before exiting -- its own definition may be left in an inconsistent state; verify it by hand (virsh dumpxml) before trusting this target", "signal", sig.String())
					}
					break
				}
				time.Sleep(50 * time.Millisecond)
			}

			// Everything externally visible (backup job, target bridge/
			// qemu-nbd, checkpoint) is now torn down above. Don't wait for
			// wg.Wait() in run() below to unblock: if a sync goroutine is
			// stuck inside a synchronous libnbd call (Pread/Pwrite/
			// BlockStatus) against a wedged connection, context cancellation
			// alone can never force it to return -- Go has no way to
			// interrupt a blocked cgo call from another goroutine, so the
			// process could otherwise sit there forever despite a clean
			// signal handler. Exit directly instead.
			trace.Warning("cleanup complete, forcing process exit", "signal", sig.String())
			// Mirrors the deferred metrics write further up in run() --
			// duplicated here for the same reason as the checkpoint cleanup
			// above: os.Exit below skips that defer entirely. An interrupted
			// run is always recorded as a failure, regardless of how far the
			// sync had gotten.
			writeMetricsTextfile(metrics.StateFailure)
			os.Exit(1)
		case <-doneCh:
			return
		}
	}()

	srcXML, err := srcDom.GetXMLDesc(0)
	if err != nil {
		return fmt.Errorf("read source domain xml: %w", err)
	}
	trace.Info("discovered source domain", "domain", cfg.SourceDomain)

	qcowDisks, err := disk.ParseQcowDisks(srcXML)
	if err != nil {
		return err
	}
	if len(qcowDisks) == 0 {
		return fmt.Errorf("no qcow2 disks found for domain %s", cfg.SourceDomain)
	}
	trace.Info("discovered qcow2 disks", "count", len(qcowDisks))

	// Skipped under -reinit: a stuck block job is exactly the kind of state
	// -reinit is meant to clear (via AbortActiveBlockJobs below), so failing
	// here first would make -reinit unable to reach it.
	if !cfg.Reinit {
		if err := libvirtsync.FailIfBlockJobActive(srcDom, qcowDisks); err != nil {
			return err
		}
	}

	sourceNeedsSSH := util.UriUsesSSH(cfg.SourceURI)
	if sourceNeedsSSH {
		sourceSSHConfig, err := remotessh.ConfigFromLibvirtURI(
			cfg.SourceURI,
			cfg.SSHUser,
			cfg.SSHKey,
			cfg.SSHPassword,
			cfg.KnownHosts,
			cfg.SSHPort,
			cfg.SSHInsecure,
			time.Duration(cfg.SSHTimeoutSec)*time.Second,
		)
		if err != nil {
			return err
		}
		trace.Info("source URI uses SSH; qemu-img info will run remotely", "user", sourceSSHConfig.User, "host", sourceSSHConfig.Address, "port", sourceSSHConfig.Port)
		sourceClient, dialErr := remotessh.Dial(sourceSSHConfig)
		if dialErr != nil {
			return fmt.Errorf("connect ssh for source qemu-img execution: %w", dialErr)
		}
		sshClientMu.Lock()
		sourceSSHClient = sourceClient
		sshClientMu.Unlock()
		defer sourceSSHClient.Close()
		if err := nbdbridge.CheckRemote(ctx, sourceSSHClient, bridgeCfg, sourceSSHConfig.Address); err != nil {
			return err
		}
	} else {
		trace.Info("source URI does not use SSH; qemu-img info will run locally")
	}

	for i, d := range qcowDisks {
		var chain []disk.QemuImgInfo
		if sourceNeedsSSH {
			trace.Info("running remote qemu-img info", "disk", d.TargetDev, "path", d.Source)
			chain, err = disk.QemuImgInfoChainJSONRemote(ctx, sourceSSHClient, d.Source)
		} else {
			trace.Info("running local qemu-img info", "disk", d.TargetDev, "path", d.Source)
			chain, err = disk.QemuImgInfoChainJSON(d.Source)
		}
		if err != nil {
			return err
		}
		info := chain[0]
		qcowDisks[i].VirtualSize = info.VirtualSize
		qcowDisks[i].ClusterSize = info.ClusterSize

		// --backing-chain returns the chain ordered top (d.Source itself)
		// to base; the last element is the disk's real, stable base file.
		// d.Source is whatever the domain's disk currently points at, which
		// differs from that stable base both when an external snapshot
		// exists (virsh snapshot-create --disk-only redirects the domain to
		// a new overlay named after the snapshot) and, unrelated to
		// snapshots, when the disk is a permanent qcow2 linked clone of a
		// shared base image -- this resolution can't tell the two apart
		// (see QcowDisk.RootSource), so the log below doesn't assert which
		// one it is. Target-side paths are named after this base, not
		// d.Source, so they keep matching the real target file that
		// earlier syncs already created under the same resolved name.
		rootPath := disk.ResolveRootSource(chain, d.Source)
		if rootPath != d.Source {
			trace.Info("disk has a backing file chain, resolved target-side naming to its base (external snapshot or linked clone)", "disk", d.TargetDev, "active", d.Source, "base", rootPath)
		}
		qcowDisks[i].RootSource = rootPath

		trace.Info("disk info", "disk", d.TargetDev, "format", info.Format, "virtual_size", d.VirtualSize, "path", d.Source, "discard", d.DiscardMode)
	}

	targetNeedsSSH := util.UriUsesSSH(cfg.TargetURI)
	if !targetNeedsSSH {
		return fmt.Errorf("target URI must be ssh-based for remote target file creation")
	}
	targetSSHConfig, err := remotessh.ConfigFromLibvirtURI(
		cfg.TargetURI,
		cfg.SSHUser,
		cfg.SSHKey,
		cfg.SSHPassword,
		cfg.KnownHosts,
		cfg.SSHPort,
		cfg.SSHInsecure,
		time.Duration(cfg.SSHTimeoutSec)*time.Second,
	)
	if err != nil {
		return err
	}
	trace.Debug("resolved target ssh connection", "user", targetSSHConfig.User, "host", targetSSHConfig.Address, "port", targetSSHConfig.Port, "key", targetSSHConfig.PrivateKeyPath)
	targetClient, dialErr := remotessh.Dial(targetSSHConfig)
	if dialErr != nil {
		return fmt.Errorf("connect ssh for target file/export execution: %w", dialErr)
	}
	sshClientMu.Lock()
	targetSSHClient = targetClient
	sshClientMu.Unlock()
	defer targetSSHClient.Close()

	// Take the TARGET-side run lock before anything touches the target.
	//
	// The lock in main() is on the SOURCE host, keyed by the source domain,
	// and protects the source's checkpoint chain. It says nothing whatsoever
	// about the target, and two different sources replicating into one
	// target -- or a sync and a promotion -- are exactly the collisions it
	// cannot see. This one is keyed by the TARGET domain and lives on the
	// target host, so every operation that writes that domain excludes every
	// other one, wherever it was started from.
	//
	// Held by a remote process blocking on its stdin, so it is released by
	// this SSH connection closing -- which covers a clean exit, a SIGKILL,
	// and the network dropping alike. Nothing to expire, and no stale lock
	// to clear by hand afterwards.
	targetLockCtx, cancelTargetLock := context.WithTimeout(ctx, targetLockTimeout)
	targetLock, lockErr := util.AcquireRemoteRunLock(targetLockCtx, targetSSHClient, runLockDir, targetLockKey(cfg.TargetDomain))
	cancelTargetLock()
	if lockErr != nil {
		return fmt.Errorf("target %s on %s: %w", cfg.TargetDomain, util.HostFromURIOrLocal(cfg.TargetURI), lockErr)
	}
	defer targetLock.Close()
	trace.Debug("holding the target-side run lock", "vm", cfg.TargetDomain, "host", util.HostFromURIOrLocal(cfg.TargetURI))

	// Check the two clocks agree before anything depends on them.
	//
	// This run is about to write a timestamp on the target using THIS
	// host's clock, and later reads -- the metadata-vs-file-timestamp
	// consistency check, a promotion's data-loss window, the control
	// plane's freshness judgement -- compare it against times taken
	// elsewhere. Drift does not break any of that visibly; it makes every
	// one of those answers quietly wrong, which is worse. NTP is a
	// documented prerequisite, and this is what notices when it has stopped
	// being true.
	//
	// A warning, never a refusal: a sync with skewed clocks still copies
	// the right bytes, and refusing to replicate over a clock problem would
	// turn a monitoring issue into an outage.
	{
		skewCtx, cancelSkew := context.WithTimeout(ctx, 15*time.Second)
		skew, skewErr := util.RemoteClockSkew(skewCtx, targetSSHClient)
		cancelSkew()
		switch {
		case skewErr != nil:
			trace.Warning("could not compare this host's clock with the target's", "error", skewErr)
		case skew < -clockSkewWarnAt || skew > clockSkewWarnAt:
			trace.Warning("this host's clock disagrees with the target's; replication ages, failover data-loss windows and the target's out-of-band-modification check all compare timestamps written by the two, so they will be wrong until NTP is fixed",
				"target_host", util.HostFromURIOrLocal(cfg.TargetURI),
				"skew_seconds", int64(skew.Seconds()),
				"threshold_seconds", int64(clockSkewWarnAt.Seconds()))
		default:
			trace.Debug("clocks agree", "skew_seconds", int64(skew.Seconds()))
		}
	}

	defer cleanupTargetNBD("cleanup")
	defer cleanupSourceBridge("cleanup")

	// Resolve the two base ports now: both SSH clients are up, the disk
	// count is known, and nothing has bound anything yet. Every other port
	// this run uses is derived from these two by offset, so this is the
	// only place a choice is made.
	//
	// A fixed spec short-circuits without probing at all -- the operator
	// named a port, and second-guessing it here would only produce a worse
	// version of the bind error that follows.
	{
		bridging := bridgeCfg.Enabled()
		srcNeed := sourcePortsNeeded(bridging)
		tgtNeed := targetPortsNeeded(len(qcowDisks), bridging, cfg.Verify != "")
		// Skewed per target domain so two syncs of different vms into the
		// same target host tend to land on different blocks; see
		// portalloc.SelectBase.
		skew := portalloc.Skew(cfg.TargetDomain)

		srcSpec, err := portalloc.ParseSpec(cfg.SourceNBDPortSpec, portalloc.DefaultSourceAutoLow, portalloc.DefaultSourceAutoHigh)
		if err != nil {
			return fmt.Errorf("source-nbd-port: %w", err)
		}
		srcUsed := map[int]bool{}
		if !srcSpec.IsFixed() {
			srcUsed, err = listeningPorts(ctx, sourceNeedsSSH, sourceSSHClient)
			if err != nil {
				return fmt.Errorf("list listening ports on the source host to choose -source-nbd-port: %w", err)
			}
		}
		cfg.SourceNBDPort, err = portalloc.SelectBase(srcUsed, srcSpec, srcNeed, skew)
		if err != nil {
			return fmt.Errorf("source-nbd-port: %w", err)
		}

		tgtSpec, err := portalloc.ParseSpec(cfg.TargetNBDPortSpec, portalloc.DefaultTargetAutoLow, portalloc.DefaultTargetAutoHigh)
		if err != nil {
			return fmt.Errorf("target-nbd-port: %w", err)
		}
		tgtUsed := map[int]bool{}
		if !tgtSpec.IsFixed() {
			tgtUsed, err = listeningPorts(ctx, true, targetSSHClient)
			if err != nil {
				return fmt.Errorf("list listening ports on the target host to choose -target-nbd-port: %w", err)
			}
		}
		cfg.TargetNBDPort, err = portalloc.SelectBase(tgtUsed, tgtSpec, tgtNeed, skew)
		if err != nil {
			return fmt.Errorf("target-nbd-port: %w", err)
		}

		trace.Info("resolved nbd port layout",
			"source_spec", srcSpec.String(), "source_base", cfg.SourceNBDPort, "source_ports", srcNeed,
			"target_spec", tgtSpec.String(), "target_base", cfg.TargetNBDPort, "target_ports", tgtNeed)
	}
	if err := nbdbridge.CheckRemote(ctx, targetSSHClient, bridgeCfg, targetSSHConfig.Address); err != nil {
		return err
	}

	// Moved here (was previously checked before targetSSHClient even
	// existed, so util.RemotePathExists always failed with "ssh client is
	// not connected" and this warning could never actually fire) -- this is
	// the earliest point in run() where a check against the target host can
	// succeed at all.
	nvram, err := libvirtsync.DetectNvram(srcXML)
	if err != nil {
		return err
	}
	if nvram != "" {
		x, err := util.RemotePathExists(ctx, targetSSHClient, nvram)
		if err != nil {
			// Advisory check only (see loader below too) -- inconclusive
			// isn't the same as "doesn't exist" and must not be reported as
			// such, but it also doesn't warrant failing the whole sync.
			trace.Warning("could not check whether nvram file exists on target host", "path", nvram, "error", err)
		} else if !x {
			trace.Warning("nvram setting detected in vm config", "path", nvram, "but files do not exist on target host")
		}
	}

	loader, lerr := libvirtsync.DetectLoader(srcXML)
	if lerr != nil {
		return lerr
	}
	if loader != "" {
		x, err := util.RemotePathExists(ctx, targetSSHClient, loader)
		if err != nil {
			trace.Warning("could not check whether loader file exists on target host", "path", loader, "error", err)
		} else if !x {
			trace.Warning("loader setting detected in vm config", "path", loader, "but files do not exist on target host")
		}
	}

	// replacedDiskOwners remembers what owned each target disk before a
	// -reinit displaced it, keyed by target path, so the freshly created
	// replacement can be given the same ownership back.
	//
	// Written in the reinit block below, which runs to completion before any
	// per-disk goroutine starts, and only read afterwards -- so no lock.
	replacedDiskOwners := map[string]util.DiskOwner{}

	if cfg.Reinit {
		trace.Warning("reinit requested: discarding checkpoint chain and existing target state", "domain", cfg.SourceDomain)
		if err := libvirtsync.AbortActiveBlockJobs(srcDom, qcowDisks); err != nil {
			return fmt.Errorf("reinit: abort active block jobs: %w", err)
		}
		if err := libvirtsync.DeleteAllManagedCheckpoints(srcDom); err != nil {
			return fmt.Errorf("reinit: delete existing checkpoints: %w", err)
		}

		// DomainExists (unlike a bare LookupDomain) distinguishes a genuine
		// "no such domain" from any other lookup failure (auth, a transient
		// connection hiccup, ...) -- treating those the same way used to
		// silently skip the "refuse if running" guard just below and fall
		// straight through to deleting the target's disk files, even when
		// the domain in fact exists and is running.
		//
		// Deliberately NOT undefining the target domain here (this used to
		// call tgtDom.Undefine() right after this check): that left the
		// target undefined for the entire disk-copy duration below -- often
		// the longest part of the whole run -- so any interruption during
		// that window (SIGINT/SIGTERM, a killed process, a network drop)
		// left the target permanently undefined until some later run
		// happened to complete a full sync all the way through. The target
		// domain's definition is now left completely untouched by -reinit;
		// DefineDomain (at the very end, only after the copy has fully
		// succeeded) is the only place that ever undefines/redefines it,
		// and it already does so with its own rollback-to-original-XML
		// safety net (see its own doc comment) -- undefining early here
		// just threw that rollback target away before it could ever be
		// used. As a side effect, this also drops the one remaining
		// plain Undefine() call in this file: it never passed
		// DOMAIN_UNDEFINE_KEEP_NVRAM the way DefineDomain's own undefine
		// does, so -reinit against any UEFI/OVMF target domain was likely
		// already failing outright at this step.
		exists, err := libvirtsync.DomainExists(tgtMgr.Conn, cfg.TargetDomain)
		if err != nil {
			return fmt.Errorf("reinit: check target domain existence: %w", err)
		}
		running := false
		if exists {
			tgtDom, err := tgtMgr.LookupDomain(cfg.TargetDomain)
			if err != nil {
				return fmt.Errorf("reinit: look up target domain %s: %w", cfg.TargetDomain, err)
			}
			running, err = libvirtsync.DomainActive(tgtDom)
			tgtDom.Free()
			if err != nil {
				return fmt.Errorf("reinit: check target domain state: %w", err)
			}
		}
		if err := refuseReinitIfTargetRunning(cfg.TargetDomain, exists, running); err != nil {
			return err
		}

		// What happens to the existing target disks is the operator's call,
		// not vmsync's, because the two answers differ in what they risk.
		//
		// Deleting is right for an ordinary reinit, where the target holds a
		// stale replica nobody wants. It is emphatically NOT right after a
		// failover: the domain being reinitialised is then the old primary,
		// and its disks hold the only copy of everything written between the
		// last successful sync and the moment it went down -- precisely the
		// data the failover accepted losing.
		//
		// The default is therefore "rename", which changes what a bare
		// -reinit has historically done. Chosen deliberately: the two
		// mistakes are not symmetric. Defaulting to delete costs an
		// unrecoverable loss the one time it is wrong; defaulting to rename
		// costs disk space and a stale file to clean up, and says so loudly
		// in the log every time. Nothing reaps the aside files -- they are
		// deliberately somebody's decision, not a background job's.
		if len(qcowDisks) > 0 {
			if err := sweepRestorePointsForReinit(ctx, cfg, targetSSHClient,
				util.SetTargetPath(cfg.TargetDiskPath, qcowDisks[0].RootSource)); err != nil {
				return err
			}
		}

		for _, d := range qcowDisks {
			reinitTargetPath := util.SetTargetPath(cfg.TargetDiskPath, d.RootSource)

			// Nothing to replace is an ordinary state, not a failure, and it
			// has to be checked rather than shrugged off by the command:
			// `rm -f` tolerates a missing file but `mv -n` does not, and
			// rename is the DEFAULT. So a -reinit against a target that does
			// not exist yet -- a first-ever run, one after a previous
			// -reinit with delete, or a multi-disk domain where only some
			// files are present -- failed outright on the move.
			exists, err := util.RemotePathExists(ctx, targetSSHClient, reinitTargetPath)
			if err != nil {
				return fmt.Errorf("reinit: check whether target disk %s exists: %w", reinitTargetPath, err)
			}
			if !exists {
				trace.Info("reinit: nothing to replace, the target disk does not exist yet", "path", reinitTargetPath)
				continue
			}

			// Remember what owns this file BEFORE it is moved out of the
			// way, because the replacement is created by qemu-img over SSH
			// and will belong to that SSH user -- root. Whatever owns it now
			// is, by construction, ownership that worked: this domain has
			// been synced before, and if it was ever started, libvirt opened
			// these files. Restoring it after the copy is what stops a
			// -reinit quietly converting a bootable replica into one qemu
			// cannot open.
			//
			// Recorded even when the mode is "off" or an explicit owner was
			// given, so the log can still say what changed.
			if out, err := targetSSHClient.Run(ctx, util.StatOwnerCommand(reinitTargetPath)); err == nil {
				if prev := util.ParseStatOwner(out); !prev.Empty() {
					replacedDiskOwners[reinitTargetPath] = prev
					trace.Debug("reinit: remembering the replaced disk's ownership",
						"path", reinitTargetPath, "owner", prev.Spec())
				}
			}

			var cmd string
			switch cfg.ReplacedDiskAction {
			case replacedDiskRename:
				// Suffix, not a different directory: a sibling path is on the
				// same filesystem, so the rename is atomic and cannot fail
				// part-way or silently copy gigabytes.
				aside := reinitTargetPath + replacedDiskSuffix + strconv.FormatInt(time.Now().Unix(), 10)
				trace.Warning("reinit: renaming target disk aside instead of deleting it",
					"path", reinitTargetPath, "renamed_to", aside)
				// -n so an existing file at the destination is never
				// clobbered; the run fails instead of destroying a previous
				// set that was itself kept deliberately.
				cmd = "mv -n " + util.ShQuote(reinitTargetPath) + " " + util.ShQuote(aside)
			default:
				trace.Info("reinit: removing target disk", "path", reinitTargetPath)
				cmd = "rm -f " + util.ShQuote(reinitTargetPath)
			}
			if out, err := targetSSHClient.Run(ctx, cmd); err != nil {
				return fmt.Errorf("reinit: %s target disk %s: %w: %s", cfg.ReplacedDiskAction, reinitTargetPath, err, out)
			}
		}
		trace.Info("reinit complete, proceeding with full sync")
	}

	existing, err := libvirtsync.ListManagedCheckpoints(srcDom)
	if err != nil {
		return err
	}
	checkpointMu.Lock()
	checkpointName, parent, err = libvirtsync.NextCheckpointName(existing)
	checkpointMu.Unlock()
	if err != nil {
		return err
	}
	var targetPath string
	if parent == "" {
		// Preflight for full sync: fail before sync operations if target disk path exists.
		for _, d := range qcowDisks {
			targetPath = util.SetTargetPath(cfg.TargetDiskPath, d.RootSource)
			trace.Info("Using target", "path", targetPath, "disk", d.TargetDev)
			targetDir := path.Dir(targetPath)
			if _, err := targetSSHClient.Run(ctx, "mkdir -p "+util.ShQuote(targetDir)); err != nil {
				return fmt.Errorf("create remote target dir %s: %w", targetDir, err)
			}
			exists, err := util.RemotePathExists(ctx, targetSSHClient, targetPath)
			if err != nil {
				// Unlike the advisory nvram/loader checks above, this one
				// gates a destructive action (full sync overwriting
				// whatever's already there) -- an inconclusive check must
				// fail the run, not silently proceed as if the path were
				// confirmed absent.
				return fmt.Errorf("check target disk existence for %s: %w", targetPath, err)
			}
			if exists {
				return fmt.Errorf("full sync requested but target disk already exists on target host: %s", targetPath)
			}
		}
	}

	tgtDom, err := tgtMgr.LookupDomain(cfg.TargetDomain)
	var metadataEntryCheckpoint string
	var metadataEntryTimestamp string
	if err != nil {
		if parent == "" {
			trace.Info("Domain does not exist on target system")
		} else {
			return fmt.Errorf("Incremental sync attempted but target domain does not exist: domain=%s", cfg.TargetDomain)
		}
	} else {
		if tgtState, err = libvirtsync.DomainActive(tgtDom); err != nil {
			tgtDom.Free()
			return err
		}
		if tgtState == true {
			tgtDom.Free()
			return fmt.Errorf("target domain %s is active require shutoff before sync", cfg.TargetDomain)
		}

		if cfg.Reinit {
			// -reinit already discarded the target's previous state
			// above (its trace.Warning says as much) and removed its
			// disk file -- the metadata/timestamp continuity check
			// below stats that same file and would now fail every
			// single -reinit run for a domain that predates it (the
			// file it's looking for is gone by design), not just flag a
			// real out-of-band change. Nothing here is skipped that
			// -reinit wasn't already meant to bypass; the domain
			// definition itself (unlike before) is simply left alone
			// until DefineDomain redefines it once the copy succeeds.
			tgtDom.Free()
		} else {
			trace.Info("Target domain exists, parse metadata info")

			// DOMAIN_XML_INACTIVE, because this document is read for
			// nothing but vmsync's metadata, and vmsync's metadata is
			// written to the PERSISTENT definition (AFFECT_CONFIG, see
			// SetDomainMetadataFields). Flags 0 returns the LIVE
			// definition of a running domain, which a config-only write
			// never reaches -- so on a target that happens to be running,
			// last_checkpoint would be read from a document no write ever
			// lands in, and every sync would see whatever it said when the
			// domain was started.
			tgtXML, err := tgtDom.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
			if err != nil {
				tgtDom.Free()
				return fmt.Errorf("read target domain xml: %w", err)
			}
			// tgtDom itself isn't touched again past this point -- only
			// tgtXML (the plain string already extracted from it) is
			// used below -- so free it here rather than holding the
			// target-side libvirt handle open for the rest of this
			// (potentially long-running) sync.
			tgtDom.Free()

			// Two separate error variables -- not the shared err used
			// elsewhere in this function -- specifically because the second
			// ParseMetadata call used to overwrite the first call's err
			// before it was ever checked, silently discarding a genuine
			// parse failure on the checkpoint field whenever the timestamp
			// field happened to parse fine (or vice versa).
			var checkpointParseErr, timestampParseErr error
			metadataEntryCheckpoint, checkpointParseErr = libvirtsync.ParseMetadata(tgtXML, libvirtsync.MetadataFieldLastCheckpoint)
			metadataEntryTimestamp, timestampParseErr = libvirtsync.ParseMetadata(tgtXML, libvirtsync.MetadataFieldLastSync)
			if checkpointParseErr != nil || metadataEntryCheckpoint == "" {
				if err := unverifiableCheckpointMetadataError(cfg.TargetDomain, parent, checkpointParseErr, metadataEntryCheckpoint); err != nil {
					return err
				}
				trace.Warning("empty or unparsable target domain metadata entry, cannot verify checkpoint chain", "error", checkpointParseErr)
			} else {
				trace.Info("Target domain metadata", "checkpoint", metadataEntryCheckpoint)
			}
			if timestampParseErr != nil || metadataEntryTimestamp == "" {
				trace.Warning("empty or unparsable target domain metadata entry, cannot verify timestamp", "error", timestampParseErr)
			} else {
				trace.Info("Target domain metadata", "timestamp", metadataEntryTimestamp)
				for _, d := range qcowDisks {
					targetPath = util.SetTargetPath(cfg.TargetDiskPath, d.RootSource)
					out, err := targetSSHClient.Run(ctx, "stat -c '%Y' "+util.ShQuote(targetPath))
					if err != nil {
						return fmt.Errorf("%w: %s", err, out)
					}
					if out > metadataEntryTimestamp {
						return fmt.Errorf("Target file on system is newer (%s)  than last sync timestamp: %s: file on target has been changed between syncs", out, targetPath)

					}
				}
				trace.Info("Successfully verified target file timestamps")
			}
		}
	}

	if parent == "" {
		trace.Info("created initial", "checkpoint", checkpointName)
	} else {
		if !checkpointChainConsistent(metadataEntryCheckpoint, parent) {
			return fmt.Errorf("checkpoint inconsistency detected: target VM definition lists [%s] as parent checkpoint, but parent checkpoint defined is [%s]", metadataEntryCheckpoint, parent)
		}
		if metadataEntryCheckpoint != "" {
			trace.Info("Successfully verified checkpoint chain")
		}
		trace.Info("creating incremental", "checkpoint", checkpointName, "parent", parent)
	}

	if snapCount, err := libvirtsync.ExternalSnapshotCount(srcDom); err != nil {
		trace.Warning("unable to check for existing external snapshots on source domain", "error", err)
	} else {
		metricsMu.Lock()
		externalSnapshotCount = snapCount
		metricsMu.Unlock()
		if snapCount > 0 {
			trace.Info("external snapshot(s) detected on source domain", "vm", cfg.SourceDomain, "count", snapCount)
		}
	}

	// A fresh state check, not the srcState captured before this point --
	// srcState comes from DomainActive (true for RUNNING and PAUSED alike),
	// so it goes stale the moment -verify suspends a running domain above.
	// FSFreeze needs the guest agent to actually service the request, which
	// requires the vCPUs to be scheduled -- attempting it against a paused
	// domain (whether paused by -verify just now, or already paused before
	// this run started) always fails, and isn't a real degradation anyway:
	// a paused domain's disk is already static, at least as consistent as
	// a successful freeze would have made it.
	freezeState, _, err := srcDom.GetState()
	if err != nil {
		return fmt.Errorf("check source domain state before filesystem freeze: %w", err)
	}
	if freezeState == libvirt.DOMAIN_RUNNING {
		if err := srcDom.FSFreeze(nil, 0); err != nil {
			trace.Warning("Filesystem freeze failed", "error", err)
			fsFreezeFailed = true
		} else {
			freezeMu.Lock()
			freezed = true
			freezeMu.Unlock()
			// Registered right here, not up where thawSource itself is
			// declared -- same reasoning as abortBackup's own defer being
			// registered at the point the backup job actually starts: the
			// hazard (a frozen guest filesystem) exists starting exactly
			// now, and thawSource's own thawOnce/freezeMu-guarded check
			// already makes this safe to also call explicitly below and
			// from the signal handler without double-thawing.
			defer thawSource("cleanup")
			trace.Info("Successfully freezed file systems using guest agent")
		}
	} else {
		trace.Info("VM is not in running state, skipping filesystem freeze")
	}

	// Captured before the call, not after: this is the instant the replica's
	// contents will correspond to, because everything the guest writes from
	// the moment the checkpoint exists belongs to the NEXT one. Recorded on
	// the target further down so a later failover can state its data-loss
	// window honestly instead of measuring from the end of the copy.
	checkpointAt := time.Now()
	// Whether the SOURCE was already stopped at this instant, recorded on the
	// target further down. A stopped source cannot write after the checkpoint,
	// which makes the replica provably complete -- the only honest basis for a
	// promotion to report zero data loss, as opposed to trusting that whoever
	// typed -promote-mode=planned really did run a final sync.
	//
	// SHUTOFF specifically, not DomainActive's "not shut off": a PAUSED source
	// is not writing right now but can be resumed the moment this run ends, so
	// treating it as stopped would license a zero that a resume immediately
	// falsifies.
	sourceStoppedAtCheckpoint := false
	if state, _, stateErr := srcDom.GetState(); stateErr == nil {
		sourceStoppedAtCheckpoint = state == libvirt.DOMAIN_SHUTOFF
	} else {
		trace.Warning("could not read the source domain's state at checkpoint time; this replica will not claim a verified zero data-loss window", "vm", cfg.SourceDomain, "error", stateErr)
	}
	if err := libvirtsync.CreateCheckpoint(srcDom, checkpointName, parent, qcowDisks); err != nil {
		// A full sync (parent == "") has no earlier checkpoint to fall back
		// on -- without a checkpoint at all there's no bitmap to establish a
		// baseline for future incremental syncs, so that case still fails
		// outright, same as any other CreateCheckpoint error.
		if parent == "" || !libvirtsync.IsCheckpointBlockedBySnapshot(err) {
			thawSource("checkpoint creation failed")
			return err
		}
		trace.Warning("checkpoint creation blocked by an existing external snapshot on the source domain; syncing incrementally against the existing checkpoint without advancing the checkpoint chain", "attempted_checkpoint", checkpointName, "parent", parent, "error", err)
	} else {
		checkpointMu.Lock()
		checkpointAdvanced = true
		checkpointMu.Unlock()
	}
	thawSource("checkpoint creation complete")

	// runErr is checked here, at the call site, rather than inside
	// cleanupOrphanedCheckpoint itself: runErr is only meaningful once
	// run() has actually returned (it's this function's own named return
	// value), which is exactly when this defer fires -- unlike the signal
	// handler's call to the same closure, which happens mid-flight, before
	// run() has returned at all, and unconditionally wants this checked
	// (being interrupted is inherently not a success). See
	// cleanupOrphanedCheckpoint's own doc comment for why this is now a
	// single shared closure instead of two independently-racing copies of
	// the same logic.
	defer func() {
		if runErr != nil {
			cleanupOrphanedCheckpoint("sync failed")
		}
	}()

	incrementalMode := parent != ""
	incrementalCheckpoint := ""
	exportBitmap := ""
	bitmapForRead := checkpointName
	if incrementalMode {
		// Read blocks changed since previous checkpoint.
		incrementalCheckpoint = parent
		exportBitmap = parent
		bitmapForRead = parent
		if checkpointAdvanced {
			trace.Info("starting incremental pull backup", "parent_checkpoint", parent, "new_checkpoint", checkpointName)
		} else {
			trace.Info("starting incremental pull backup", "parent_checkpoint", parent, "new_checkpoint", "none (checkpoint chain not advancing this run)")
		}
	} else {
		trace.Info("starting full pull backup (no incremental bitmap)")
	}

	// backupActive is set -- and the cleanup defer armed -- BEFORE calling
	// StartPullBackupTCP, not after it returns: libvirt's RPC can already
	// have created the backup job (and opened its NBD TCP export) on the
	// server side before the client-side call itself returns to us,
	// especially over a remote connection where the response can be
	// delayed or lost in transit after the server already acted on the
	// request. A signal landing while still blocked inside this call would
	// otherwise see backupActive still false and skip cleanup entirely,
	// orphaning a running backup job and its exposed NBD export with
	// nothing left to ever stop it -- indefinitely, and blocking any
	// future backup/checkpoint attempt against this domain until an
	// operator intervenes by hand. StopBackup's own job-stats-based check
	// already makes it a safe, harmless no-op when the job never actually
	// started, so there's no real cost to arming this pessimistically
	// before knowing whether the call will even succeed.
	backupMu.Lock()
	backupActive = true
	backupMu.Unlock()
	defer abortBackup("cleanup")
	if err := libvirtsync.StartPullBackupTCP(srcDom, incrementalCheckpoint, exportBitmap, cfg.SourceNBDBind, cfg.SourceNBDPort, qcowDisks); err != nil {
		return err
	}

	trace.Info("source nbd port in use", "side", "source", "kind", "nbd_export", "host", nbdHost, "port", cfg.SourceNBDPort)

	// Default to the direct, uncompressed path; overridden below when
	// --compress/--netbuffer are set and the source is reachable via SSH.
	effectiveSourceHost := nbdHost
	effectiveSourcePort := cfg.SourceNBDPort
	var sourceBridgeCounters *nbdbridge.ByteCounters
	if bridgeCfg.Enabled() && sourceNeedsSSH {
		// The source has a single shared NBD export (no per-disk ports), so
		// its bridge port simply sits right next to it.
		sourceBridgePort := cfg.SourceNBDPort + 1
		stopCmd, err := nbdbridge.StartRemote(ctx, sourceSSHClient, sourceBridgePort, cfg.SourceNBDPort, bridgeCfg)
		if err != nil {
			return fmt.Errorf("start source nbd bridge: %w", err)
		}
		stopMu.Lock()
		sourceStopCommands = append(sourceStopCommands, stopCmd)
		stopMu.Unlock()
		trace.Info("source nbd port in use", "side", "source", "kind", "bridge_remote", "host", nbdHost, "port", sourceBridgePort)
		sourceBridgeDialAddr := fmt.Sprintf("%s:%d", nbdHost, sourceBridgePort)
		if cfg.UseSSH {
			sourceBridgeDialAddr = fmt.Sprintf("127.0.0.1:%d", sourceBridgePort)
		}
		localPort, counters, stopLocal, err := nbdbridge.StartLocal(ctx, sourceSSHClient, sourceBridgeDialAddr, bridgeCfg)
		if err != nil {
			return fmt.Errorf("start local nbd bridge relay for source: %w", err)
		}
		defer stopLocal()
		effectiveSourceHost = "127.0.0.1"
		effectiveSourcePort = localPort
		sourceBridgeCounters = counters
		trace.Info("source nbd port in use", "side", "source", "kind", "bridge_local", "host", "127.0.0.1", "port", localPort)
	}

	// verifyWindow carries what -verify=online's compare phase needs from
	// beginVerifyWindow. checkpointName is empty for the plain -verify/
	// -verify=fast path (runVerify uses that to tell the two modes apart);
	// non-empty means the compare should reconcile mismatches against that
	// checkpoint's own dirty bitmap instead of failing on the first one.
	type verifyWindow struct {
		checkpointName string
		cleanup        func()
	}

	// beginVerifyWindow opens -verify=online's compare window: it stops the
	// regular sync's backup job, creates the ephemeral verify-window
	// checkpoint, then starts a SECOND, short-lived backup job scoped to
	// that checkpoint's bitmap. This second job is necessary, not
	// incidental -- libvirt's pull-mode backup XML binds exactly one
	// bitmap (exportbitmap) per disk, fixed at BackupBegin time (see
	// https://libvirt.org/formatbackup.html), so the already-running first
	// job (scoped to the regular chain's checkpoint) can never expose the
	// new checkpoint's bitmap over the same NBD connection -- only a backup
	// job actually started with that bitmap as its exportbitmap can.
	// Reuses cfg.SourceNBDBind/cfg.SourceNBDPort (the same host:port the
	// first job used), so effectiveSourceHost/effectiveSourcePort -- and
	// any compress/netbuffer bridge already established around them further
	// up -- stay valid unchanged for the compare phase; nothing about the
	// bridge needs to be restarted.
	beginVerifyWindow := func() (verifyWindow, error) {
		// Only marked inactive once the stop is actually confirmed -- if
		// StopBackup itself fails, backupActive deliberately stays true, so
		// the deferred abortBackup("cleanup") still believes there's a job
		// to retry stopping (with its own reconnect fallback) rather than
		// silently skipping it because this attempt already (wrongly)
		// marked it as handled.
		if err := libvirtsync.StopBackup(srcDom); err != nil {
			return verifyWindow{}, fmt.Errorf("stop primary backup job before verify window: %w", err)
		}
		backupMu.Lock()
		backupActive = false
		backupMu.Unlock()

		if err := libvirtsync.DeleteVerifyWindowCheckpoint(srcDom); err != nil {
			return verifyWindow{}, fmt.Errorf("clean up any leftover verify-window checkpoint: %w", err)
		}
		if err := libvirtsync.CreateVerifyWindowCheckpoint(srcDom, qcowDisks); err != nil {
			return verifyWindow{}, fmt.Errorf("create verify-window checkpoint: %w", err)
		}
		verifyWindowMu.Lock()
		verifyWindowActive = true
		verifyWindowMu.Unlock()

		// backupActive is set BEFORE calling StartPullBackupTCP for the same
		// reason as the main backup job's own start above: the RPC can
		// create the job and open its NBD export server-side before the
		// client-side call returns, and a signal landing while still
		// blocked inside it must still see backupActive=true so
		// cleanupVerifyWindow's own shouldStop check actually attempts to
		// stop the job, instead of only deleting the verify-window
		// checkpoint and leaving a real, running backup job (and its NBD
		// export) orphaned.
		backupMu.Lock()
		backupActive = true
		backupMu.Unlock()

		if err := libvirtsync.StartPullBackupTCP(srcDom, libvirtsync.VerifyWindowCheckpointName, libvirtsync.VerifyWindowCheckpointName, cfg.SourceNBDBind, cfg.SourceNBDPort, qcowDisks); err != nil {
			cleanupVerifyWindow("verify window setup failed")
			return verifyWindow{}, fmt.Errorf("start verify-window backup job: %w", err)
		}

		return verifyWindow{
			checkpointName: libvirtsync.VerifyWindowCheckpointName,
			cleanup:        func() { cleanupVerifyWindow("verify-online compare complete") },
		}, nil
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(qcowDisks))
	reportWorkerErr := func(err error) {
		if err == nil {
			return
		}
		cancel()
		errCh <- err
	}
	// runTargetCommand only ever touches ctx/targetSSHClient, both already
	// fixed for the rest of run() by this point -- shared as-is by
	// copyAndCommit and runVerify instead of being redefined in each.
	runTargetCommand := func(command, action string) error {
		trace.Debug(command)
		out, err := targetSSHClient.Run(ctx, command)
		if err != nil {
			return fmt.Errorf("%s: %w: %s", action, err, out)
		}
		return nil
	}

	// recordDiskMetric appends one metrics.DiskMetric, exactly the
	// computation syncDisk's own deferred metrics block always did -- shared
	// so the single-phase path (duration = copy+verify combined, via
	// syncDisk's defer) and -verify=online's two-phase path (duration =
	// copy only, recorded right after copyAndCommit -- see its own doc
	// comment for why) don't each carry their own copy of it.
	recordDiskMetric := func(d disk.QcowDisk, diskSize, writtenBytes uint64, targetBridgeCounters *nbdbridge.ByteCounters, duration time.Duration) {
		if cfg.PrometheusTextfile == "" {
			return
		}
		compressedBytes := writtenBytes
		if targetBridgeCounters != nil || sourceBridgeCounters != nil {
			compressedBytes = 0
			if targetBridgeCounters != nil {
				compressedBytes += targetBridgeCounters.SentSnapshot()
			}
			if sourceBridgeCounters != nil {
				compressedBytes += sourceBridgeCounters.SentSnapshot()
			}
		}
		metricsMu.Lock()
		diskMetrics = append(diskMetrics, metrics.DiskMetric{
			SourceHost:                 nbdHost,
			TargetHost:                 targetNBDHost,
			VM:                         cfg.SourceDomain,
			Disk:                       d.TargetDev,
			DiskSizeBytes:              diskSize,
			TransferredBytes:           writtenBytes,
			CompressedTransferredBytes: compressedBytes,
			DurationSeconds:            duration.Seconds(),
		})
		metricsMu.Unlock()
	}

	// diskPhase1Result carries what runVerify needs from copyAndCommit.
	// Under -verify=online, these two run as separate goroutine invocations
	// (see the two-phase fan-out below) with a whole-run barrier between
	// them, so runVerify can no longer just read copyAndCommit's local
	// variables via closure capture the way the single-phase syncDisk path
	// still does.
	type diskPhase1Result struct {
		diskStart            time.Time
		targetPath           string
		diskSize             uint64
		writtenBytes         uint64
		targetBridgeCounters *nbdbridge.ByteCounters
	}

	// copyAndCommit is exactly today's copy+commit logic (nothing about its
	// behavior changes), just returning what runVerify/metrics need instead
	// of leaving them as syncDisk-local variables.
	copyAndCommit := func(i int, d disk.QcowDisk) (res diskPhase1Result, err error) {
		res.diskStart = time.Now()

		trace.Info("reading disk via libvirt backup NBD tcp export", "disk", d.TargetDev, "export", d.TargetDev)
		var extents []nbdsync.Extent
		var dirty uint64
		extents, res.diskSize, dirty, err = nbdsync.ChangedExtentsTCP(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, bitmapForRead, incrementalMode)
		if err != nil {
			return res, err
		}

		// Computed unconditionally (not just on the dirty>0 path below): -verify
		// needs to compare the whole image even on a run with nothing to copy,
		// not just skip straight past it -- the point is catching drift
		// unrelated to this run's own delta.
		//
		// Avoid datarace in this goroutine by declaring targetPath as local var instead of a shared one
		targetPath := util.SetTargetPath(cfg.TargetDiskPath, d.RootSource)
		res.targetPath = targetPath

		if dirty == 0 && incrementalMode {
			// Only safe to skip entirely when a base already exists from an
			// earlier full sync -- for a full sync (parent == ""), this is
			// the only place the target file ever gets created at all. A
			// disk with zero allocated extents (a freshly attached, never
			// written-to data disk, or an unbooted template) would
			// otherwise be silently left without a target file entirely,
			// while the run still reports success.
			trace.Info("No changed extents selected, skipping copy", "disk", d.TargetDev, "elapsed", time.Since(res.diskStart).Round(time.Millisecond).String())
			return res, nil
		}

		createCmd := "qemu-img create -f qcow2 " + util.ShQuote(targetPath) + " -o cluster_size=" + fmt.Sprintf("%d", d.ClusterSize) + " " + fmt.Sprintf("%d", d.VirtualSize)
		var targetPathInc string
		if incrementalMode {
			targetPathInc = targetPath + "_" + bitmapForRead
			trace.Info("Create temporary image", "disk", targetPathInc)
			createCmd = "qemu-img create -f qcow2 -F qcow2  -o cluster_size=" + fmt.Sprintf("%d", d.ClusterSize) + " " + util.ShQuote(targetPathInc) + " -b " + util.ShQuote(targetPath) + " " + fmt.Sprintf("%d", d.VirtualSize)
		}
		if err := runTargetCommand(createCmd, fmt.Sprintf("create remote qcow2 %s", targetPathInc)); err != nil {
			return res, err
		}

		// Give the base image an owner qemu can actually open.
		//
		// Only the base, never the incremental overlay: the overlay exists
		// for the duration of this run, is written by qemu-nbd as the same
		// SSH user that created it, and is committed into the base and
		// deleted before the domain is ever started. The base is the file a
		// promoted domain boots from, and the only one whose ownership
		// outlives the run.
		//
		// Only on a FULL sync, too. An incremental leaves the base file in
		// place -- qemu-img commit writes into it without touching its
		// ownership -- so there is nothing to correct, and a chown there
		// would silently overrule ownership the storage layer or an
		// administrator had deliberately set.
		if !incrementalMode {
			if err := applyTargetDiskOwner(ctx, targetSSHClient, cfg, targetPath, replacedDiskOwners[targetPath]); err != nil {
				return res, err
			}
		}

		targetPort := cfg.TargetNBDPort + i
		pidFile := path.Join("/tmp", fmt.Sprintf("vmsync-qemu-nbd-%s-%s.pid", cfg.TargetDomain, d.TargetDev))
		startExportCmd := "qemu-nbd --fork --persistent"
		if d.DiscardMode != "" {
			startExportCmd = startExportCmd + " --discard=" + d.DiscardMode
		}
		startExportCmd = startExportCmd +
			" --format=qcow2 --bind " +
			util.ShQuote(cfg.TargetNBDBind) +
			" --port " +
			fmt.Sprintf("%d", targetPort) +
			" --pid-file " +
			util.ShQuote(pidFile) +
			" "

		if incrementalMode {
			targetPathInc = targetPath + "_" + bitmapForRead
			startExportCmd = startExportCmd + util.ShQuote(targetPathInc)
		} else {
			startExportCmd = startExportCmd + util.ShQuote(targetPath)
		}
		if err := runTargetCommand(startExportCmd, fmt.Sprintf("start target qemu-nbd for %s", targetPath)); err != nil {
			return res, err
		}

		// || true baked in here (not just appended for the deferred/
		// signal-handler cleanup registration below) so the inline stop
		// call further down is equally tolerant -- kill -9 returning
		// non-zero for any reason (process already exited, a pidfile
		// race) must not abort copyAndCommit right after a successful copy:
		// for incremental mode that would abandon the already-copied
		// delta sitting in the temp overlay, unmerged and uncleaned,
		// and report a fully successful transfer as a failure. Mirrors
		// stopVerifyCmd's own pattern in runVerify.
		//
		// The trailing rm -f matters beyond tidiness: this same command
		// string stays registered in targetStopCommands for the
		// interrupt-cleanup path even after the inline call below already
		// runs it normally, so it can be replayed a second time if a
		// signal lands later in this run. Without removing the pidfile,
		// that replay would `cat` the same now-stale file and could
		// SIGKILL whatever unrelated process the OS has since reused that
		// PID for -- kill -9 succeeds against any valid PID, recycled or
		// not, so || true can't catch or prevent that. Once the pidfile is
		// gone, a replay's $(cat ...) expands to nothing, kill -9 with no
		// PID argument targets nothing, and rm -f on an already-missing
		// file is a no-op -- the whole replay becomes harmless instead of
		// dangerous. Matches nbdbridge.BuildStopCommand's own identical
		// kill-then-remove pattern for the bridge helper's pidfile.
		stopCmd := "kill -9 $(cat " + util.ShQuote(pidFile) + ") || true; rm -f " + util.ShQuote(pidFile)
		stopMu.Lock()
		targetStopCommands = append(targetStopCommands, stopCmd)
		stopMu.Unlock()

		trace.Info("target nbd port in use", "side", "target", "kind", "nbd_export", "disk", d.TargetDev, "host", targetNBDHost, "port", targetPort)

		// Default to the direct, uncompressed path; overridden below when
		// --compress/--netbuffer are set (target SSH is always available).
		effectiveTargetHost := targetNBDHost
		effectiveTargetPort := targetPort
		if bridgeCfg.Enabled() {
			// All real qemu-nbd ports occupy [TargetNBDPort, TargetNBDPort+N),
			// so the bridge ports lay out right after them, as one contiguous
			// block [TargetNBDPort+N, TargetNBDPort+2N).
			targetBridgePort := targetPort + len(qcowDisks)
			bridgeStopCmd, err := nbdbridge.StartRemote(ctx, targetSSHClient, targetBridgePort, targetPort, bridgeCfg)
			if err != nil {
				return res, fmt.Errorf("start target nbd bridge for %s: %w", d.TargetDev, err)
			}
			stopMu.Lock()
			targetStopCommands = append(targetStopCommands, bridgeStopCmd)
			stopMu.Unlock()
			trace.Info("target nbd port in use", "side", "target", "kind", "bridge_remote", "disk", d.TargetDev, "host", targetNBDHost, "port", targetBridgePort)
			targetBridgeDialAddr := fmt.Sprintf("%s:%d", targetNBDHost, targetBridgePort)
			if cfg.UseSSH {
				targetBridgeDialAddr = fmt.Sprintf("127.0.0.1:%d", targetBridgePort)
			}
			localPort, counters, stopLocal, err := nbdbridge.StartLocal(ctx, targetSSHClient, targetBridgeDialAddr, bridgeCfg)
			if err != nil {
				return res, fmt.Errorf("start local nbd bridge relay for %s: %w", d.TargetDev, err)
			}
			defer stopLocal()
			effectiveTargetHost = "127.0.0.1"
			effectiveTargetPort = localPort
			res.targetBridgeCounters = counters
			trace.Info("target nbd port in use", "side", "target", "kind", "bridge_local", "disk", d.TargetDev, "host", "127.0.0.1", "port", localPort)
		} else {
			if err := nbdsync.WaitForTCPExport(targetNBDHost, targetPort, 10*time.Second); err != nil {
				return res, fmt.Errorf("wait for target nbd export %s:%d: %w", targetNBDHost, targetPort, err)
			}
		}

		trace.Info("copy extents to remote target", "extents", len(extents), "path", targetPath, "disk_size", res.diskSize)
		res.writtenBytes, err = nbdsync.CopyExtentsTCP(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, effectiveTargetHost, effectiveTargetPort, extents, cfg.IODepth)
		if err != nil {
			return res, err
		}

		if res.targetBridgeCounters != nil {
			logicalBytes := nbdbridge.SumLogicalDirtyBytes(extents)
			trace.Info("target nbd bridge compression", "disk", d.TargetDev, "savings", nbdbridge.FormatSavings(logicalBytes, res.targetBridgeCounters.SentSnapshot()))
		}
		if sourceBridgeCounters != nil {
			logicalBytes := nbdbridge.SumLogicalDirtyBytes(extents)
			trace.Info("source nbd bridge compression", "disk", d.TargetDev, "savings", nbdbridge.FormatSavings(logicalBytes, sourceBridgeCounters.SentSnapshot()))
		}

		trace.Info("Stopping remote daemon", "device", d.TargetDev)
		if err := runTargetCommand(stopCmd, fmt.Sprintf("stop qemu-nbd for %s", targetPath)); err != nil {
			return res, err
		}

		if incrementalMode {
			trace.Info("Committing changes to base", "image", targetPath)
			commitCmd := "qemu-img commit -b " + util.ShQuote(targetPath) + " " + util.ShQuote(targetPathInc)
			if err := runTargetCommand(commitCmd, fmt.Sprintf("committing changes for %s", targetPathInc)); err != nil {
				return res, err
			}
			trace.Info("Removing temporary", "image", targetPathInc)
			if err := runTargetCommand("rm -f "+util.ShQuote(targetPathInc), fmt.Sprintf("removing target image %s", targetPathInc)); err != nil {
				return res, err
			}
		}

		return res, nil
	}

	// runVerify is today's `if cfg.Verify != ""` block, unchanged in behavior
	// for its original caller (syncDisk, verify.checkpointName always "" there
	// since cfg.Verify can only ever hold one mode at a time). Under
	// -verify=online (verify.checkpointName != ""), the compare step
	// collects every mismatch instead of failing on the first one, then
	// reconciles them against what the verify-window checkpoint's own
	// bitmap shows the guest touched during the compare.
	runVerify := func(i int, d disk.QcowDisk, res diskPhase1Result, verify verifyWindow) (err error) {
		metricsMu.Lock()
		verificationAttempted = true
		metricsMu.Unlock()

		targetPath := res.targetPath

		// Dedicated port range, distinct from both the regular
		// [TargetNBDPort, +N) and bridge [+N, +2N) ranges above, so this
		// never collides regardless of whether bridging is on, and
		// doesn't depend on the write export's port (already killed
		// above, if it ever existed this run) having actually been
		// released yet.
		verifyPort := cfg.TargetNBDPort + 2*len(qcowDisks) + i
		verifyPidFile := path.Join("/tmp", fmt.Sprintf("vmsync-verify-qemu-nbd-%s-%s.pid", cfg.TargetDomain, d.TargetDev))
		startVerifyCmd := "qemu-nbd --fork --persistent --read-only --format=qcow2 --bind " +
			util.ShQuote(cfg.TargetNBDBind) +
			" --port " +
			fmt.Sprintf("%d", verifyPort) +
			" --pid-file " +
			util.ShQuote(verifyPidFile) +
			" " +
			util.ShQuote(targetPath)
		if err := runTargetCommand(startVerifyCmd, fmt.Sprintf("start read-only verify export for %s", targetPath)); err != nil {
			return err
		}
		// Same rm -f-after-kill reasoning as stopCmd in copyAndCommit above:
		// this string is also replayable from the interrupt-cleanup path
		// after the inline call further down already runs it normally, and
		// without removing the pidfile that replay could SIGKILL whatever
		// unrelated process the OS has since reused the old PID for.
		stopVerifyCmd := "kill -9 $(cat " + util.ShQuote(verifyPidFile) + ") || true; rm -f " + util.ShQuote(verifyPidFile)
		stopMu.Lock()
		targetStopCommands = append(targetStopCommands, stopVerifyCmd)
		stopMu.Unlock()

		if err := nbdsync.WaitForTCPExport(targetNBDHost, verifyPort, 10*time.Second); err != nil {
			return fmt.Errorf("verify: wait for read-only export %s:%d: %w", targetNBDHost, verifyPort, err)
		}

		// Default to the direct, unbridged path against the read-only
		// export itself; overridden below when --compress/--netbuffer
		// are set, so a full-image verify compare -- which reads the
		// *entire* disk, not just the changed extents the regular copy
		// does -- gets the same throughput help over a slow/high-latency
		// link, instead of always being forced onto a raw, unbuffered
		// connection regardless of what the rest of the sync uses.
		verifyTargetHost := targetNBDHost
		verifyTargetPort := verifyPort
		var stopVerifyBridgeCmd string
		if bridgeCfg.Enabled() {
			// A fourth contiguous block, right after the real verify
			// export range above ([TargetNBDPort+2N, +3N)): this is
			// [TargetNBDPort+3N, +4N) -- never collides with the
			// regular/bridge/verify ranges regardless of which
			// combination of -compress/-netbuffer/-verify is active.
			verifyBridgePort := verifyPort + len(qcowDisks)
			var err error
			stopVerifyBridgeCmd, err = nbdbridge.StartRemote(ctx, targetSSHClient, verifyBridgePort, verifyPort, bridgeCfg)
			if err != nil {
				return fmt.Errorf("start verify nbd bridge for %s: %w", d.TargetDev, err)
			}
			stopMu.Lock()
			targetStopCommands = append(targetStopCommands, stopVerifyBridgeCmd)
			stopMu.Unlock()
			trace.Info("target nbd port in use", "side", "target", "kind", "verify_bridge_remote", "disk", d.TargetDev, "host", targetNBDHost, "port", verifyBridgePort)
			verifyBridgeDialAddr := fmt.Sprintf("%s:%d", targetNBDHost, verifyBridgePort)
			if cfg.UseSSH {
				verifyBridgeDialAddr = fmt.Sprintf("127.0.0.1:%d", verifyBridgePort)
			}
			localPort, _, stopLocal, err := nbdbridge.StartLocal(ctx, targetSSHClient, verifyBridgeDialAddr, bridgeCfg)
			if err != nil {
				return fmt.Errorf("start local verify nbd bridge relay for %s: %w", d.TargetDev, err)
			}
			defer stopLocal()
			verifyTargetHost = "127.0.0.1"
			verifyTargetPort = localPort
			trace.Info("target nbd port in use", "side", "target", "kind", "verify_bridge_local", "disk", d.TargetDev, "host", "127.0.0.1", "port", localPort)
		}

		// The source side is read through its own already-open libvirt
		// backup NBD export (the same effectiveSourceHost/Port used for
		// the real copy above -- already itself routed through a bridge
		// when applicable), not d.RootSource as a local file --
		// d.RootSource is only a real local path when vmsync itself
		// runs on the source host, which isn't guaranteed (-source-uri
		// can be qemu+ssh://, with vmsync running on a separate
		// orchestrator host). Using the export instead means this needs
		// no assumption about which host any file actually lives on,
		// only the same source/target network reachability the disk
		// copy itself already requires. Still true under -verify=online:
		// beginVerifyWindow's second backup job rebinds the exact same
		// host:port, so effectiveSourceHost/Port need no change here.
		sourceNBDURL := fmt.Sprintf("nbd://%s:%d/%s", effectiveSourceHost, effectiveSourcePort, d.TargetDev)
		nbdURL := fmt.Sprintf("nbd://%s:%d/", verifyTargetHost, verifyTargetPort)

		var compareErr error
		if verify.checkpointName != "" {
			trace.Info("verify-online: comparing source and target images", "disk", d.TargetDev, "source", sourceNBDURL, "target", targetPath)
			mismatches, cerr := nbdsync.CompareTCPCollect(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, verifyTargetHost, verifyTargetPort, cfg.IODepth)
			switch {
			case cerr != nil:
				compareErr = fmt.Errorf("compare failed: %w", cerr)
			case len(mismatches) == 0:
				trace.Info("verify-online: images match", "disk", d.TargetDev)
			default:
				touched, _, _, terr := nbdsync.ChangedExtentsTCP(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, verify.checkpointName, true)
				if terr != nil {
					compareErr = fmt.Errorf("dirty-bitmap query failed: %w", terr)
					break
				}
				var real []nbdsync.MismatchRange
				for _, m := range mismatches {
					if !overlapsAnyExtent(m, touched) {
						real = append(real, m)
					}
				}
				if len(real) > 0 {
					compareErr = fmt.Errorf("%d real mismatch(es) outside any region the guest touched during the compare window (of %d total detected)", len(real), len(mismatches))
				} else {
					trace.Info("verify-online: remaining mismatches all attributable to concurrent guest writes, not corruption", "disk", d.TargetDev, "count", len(mismatches))
				}
			}
		} else {
			trace.Info("verify: comparing source and target images", "disk", d.TargetDev, "source", sourceNBDURL, "target", targetPath, "fast", verifyFast)
			if verifyFast {
				compareErr = nbdsync.CompareTCP(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, verifyTargetHost, verifyTargetPort, cfg.IODepth)
			} else {
				compareErr = disk.CompareImages(sourceNBDURL, nbdURL)
			}
		}

		// Best-effort: a read-only export (or its bridge) left behind
		// poses no write-lock or data-safety risk, and a stale one still
		// bound to this exact (deterministic) port would simply make the
		// next run's own start attempt fail loudly and obviously -- not
		// worth failing this disk's result over a cleanup hiccup on top
		// of an already-known compare outcome. Bridge stopped before the
		// export it wraps, matching dependency order.
		if stopVerifyBridgeCmd != "" {
			if out, err := targetSSHClient.Run(ctx, stopVerifyBridgeCmd); err != nil {
				trace.Warning("verify: failed to stop bridge helper", "disk", d.TargetDev, "error", err, "output", out)
			}
		}
		if out, err := targetSSHClient.Run(ctx, stopVerifyCmd); err != nil {
			trace.Warning("verify: failed to stop read-only export", "disk", d.TargetDev, "error", err, "output", out)
		}

		if compareErr != nil {
			return fmt.Errorf("verify: disk %s does not match: %w", d.TargetDev, compareErr)
		}
		trace.Info("verify: images match", "disk", d.TargetDev)
		return nil
	}

	// syncDisk is the single-phase path: copy+commit, then (if cfg.Verify != "")
	// verify against the same already-open backup export, all in one
	// goroutine per disk with zero cross-disk coordination -- exactly
	// today's behavior, used whenever -verify=online is NOT set (including
	// plain syncs and -verify=compare/-verify=fast, which suspend instead of
	// using a barrier). verifyWindow{} (checkpointName == "") tells
	// runVerify to use the original compare path, not -verify=online's.
	// Decided before any disk is copied, so a target that cannot deliver what
	// -retention promises fails the run here rather than after paying for a
	// full copy. Returns an inert value when retention is off or the interval
	// has not elapsed, so nothing below needs a conditional of its own.
	//
	// Declared ahead of the closures rather than beside the loop that uses
	// it, because syncDisk below captures it.
	targetDiskPaths := make([]string, 0, len(qcowDisks))
	for _, d := range qcowDisks {
		targetDiskPaths = append(targetDiskPaths, util.SetTargetPath(cfg.TargetDiskPath, d.RootSource))
	}
	rp, err := newRestorePoints(ctx, cfg.RetentionPolicy, targetSSHClient, targetDiskPaths, checkpointName, checkpointAt)
	if err != nil {
		return err
	}

	syncDisk := func(i int, d disk.QcowDisk) (err error) {
		diskStart := time.Now()
		var res diskPhase1Result
		if cfg.PrometheusTextfile != "" {
			// Runs on every exit path (including early "return err"s
			// further down), so each disk always gets a metric --
			// res's fields simply stay at their zero value if the sync
			// failed before reaching the step that would have set them.
			defer func() {
				recordDiskMetric(d, res.diskSize, res.writtenBytes, res.targetBridgeCounters, time.Since(diskStart))
			}()
		}
		res, err = copyAndCommit(i, d)
		if err != nil {
			return err
		}
		// Before verify, not after: the reflink costs milliseconds whatever
		// the image size, while a compare can run for minutes, and a crash in
		// between would lose the restore point for no benefit. What verify
		// found is recorded on the sidecar instead.
		if err := rp.take(ctx, util.SetTargetPath(cfg.TargetDiskPath, d.RootSource)); err != nil {
			return err
		}
		if cfg.Verify != "" {
			return runVerify(i, d, res, verifyWindow{})
		}
		trace.Info("disk sync complete", "disk", d.TargetDev, "elapsed", time.Since(diskStart).Round(time.Millisecond).String())
		return nil
	}

	if !verifyOnline {
		for i, d := range qcowDisks {
			i, d := i, d
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := syncDisk(i, d); err != nil {
					reportWorkerErr(err)
				}
			}()
		}
		trace.Info("waiting for all processes to finish")
		wg.Wait()
		close(errCh)
	} else {
		// -verify=online: copy+commit for every disk first, then (once,
		// domain-wide, not per-disk) open the compare window, then compare
		// every disk in parallel again. See beginVerifyWindow's own doc
		// comment for why this needs a real barrier instead of just running
		// per-disk like the path above.
		phase1Results := make([]diskPhase1Result, len(qcowDisks))
		for i, d := range qcowDisks {
			i, d := i, d
			wg.Add(1)
			go func() {
				defer wg.Done()
				diskStart := time.Now()
				res, err := copyAndCommit(i, d)
				phase1Results[i] = res // each goroutine only ever writes index i -- no race
				recordDiskMetric(d, res.diskSize, res.writtenBytes, res.targetBridgeCounters, time.Since(diskStart))
				if err != nil {
					reportWorkerErr(err)
					return
				}
				// Same point in the sequence as the single-phase path above:
				// straight after this disk's own copy, before any compare.
				// rp.take is safe to call from several goroutines at once.
				if err := rp.take(ctx, util.SetTargetPath(cfg.TargetDiskPath, d.RootSource)); err != nil {
					reportWorkerErr(err)
				}
			}()
		}
		trace.Info("waiting for copy+commit phase before opening the verify-online compare window")
		wg.Wait()

		// First error wins, same contract as the single-phase path: don't
		// spend a checkpoint (or tear down/rebuild the backup job) on a run
		// that already failed during copy.
		select {
		case err := <-errCh:
			return err
		default:
		}

		verify, err := beginVerifyWindow()
		if err != nil {
			return fmt.Errorf("verify-online: open compare window: %w", err)
		}
		trace.Info("verify-online: compare window open, comparing all disks", "checkpoint", verify.checkpointName)

		for i, d := range qcowDisks {
			i, d := i, d
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := runVerify(i, d, phase1Results[i], verify); err != nil {
					reportWorkerErr(err)
				}
			}()
		}
		trace.Info("waiting for verify-online compare phase to finish")
		wg.Wait()
		verify.cleanup()
		close(errCh)
	}

	for err := range errCh {
		if err != nil {
			return err
		}
	}

	// Every disk finished copying (and verifying, if requested) without
	// error at this point -- from here on, only post-copy bookkeeping
	// remains (parent-checkpoint cleanup, target domain redefinition). See
	// dataCopySucceeded's own doc comment for why the checkpoint-delete-on-
	// failure cleanup below (and its signal-handler-side mirror) must not
	// treat a failure in one of those steps the same as a failed copy.
	checkpointMu.Lock()
	dataCopySucceeded = true
	checkpointMu.Unlock()
	close(copyCommitted)

	if incrementalMode && checkpointAdvanced {
		trace.Info("sync successful cleaning up parent checkpoint", "parent", parent)
		err := libvirtsync.DeleteCheckpointIfExists(srcDom, parent)
		if err != nil {
			return err
		}
	} else if incrementalMode {
		trace.Info("checkpoint chain did not advance this run (external snapshot was blocking checkpoint creation); keeping existing checkpoint for next run", "checkpoint", parent)
	}

	// The checkpoint actually current on the source after this run: the
	// newly-created one, or -- when checkpoint creation was skipped because
	// an external snapshot blocked it -- the existing parent checkpoint that
	// this run synced against instead, which is still the live one since it
	// was deliberately not cleaned up above.
	effectiveCheckpoint := checkpointName
	if !checkpointAdvanced {
		effectiveCheckpoint = parent
	}

	// Published here because every disk has now copied, and verified if
	// -verify was asked for: reaching this line at all means the compare
	// passed, since a mismatch returns long before it. Until this rename the
	// set sits under a ".incomplete-" name and is self-evidently junk.
	verifyState := restorepoint.VerifyNotRun
	if cfg.Verify != "" {
		verifyState = restorepoint.VerifyPassed
	}
	if err := rp.commit(ctx, verifyState,
		util.ReplicaHost(cfg.SourceURI, cfg.LocalHostName)+":"+cfg.SourceDomain,
		checkpointAt, effectiveCheckpoint); err != nil {
		return err
	}
	// Re-read the target's role and re-check it here, immediately before the
	// redefine, rather than trusting the read at the top of run().
	//
	// That earlier check is a preflight: it answers "may this sync start".
	// This one answers "is it still permitted to land", and the two are not
	// the same question, because everything between them takes minutes to
	// hours. A domain promoted during that window still reads `target` at
	// the preflight, and DefineDomain below replaces the target's whole
	// persistent definition -- so without this check a failover performed
	// mid-sync is silently reverted at the end of it, by a run that reports
	// success, leaving a live promoted domain marked as an ordinary replica
	// for the next scheduled sync to overwrite.
	//
	// Deliberately fatal rather than a warning. Data has already been
	// written to the target's disks by this point and that cannot be undone
	// here, but refusing to redefine the domain leaves the promotion intact
	// and the operator's decision standing, which is the part that matters.
	currentTargetRole, roleErr := libvirtsync.ReadReplicationRole(tgtMgr, cfg.TargetDomain)
	if roleErr != nil {
		return fmt.Errorf("re-read target domain replication role before redefining it: %w", roleErr)
	}
	if err := libvirtsync.TargetRoleAllowsSync(currentTargetRole); err != nil {
		return fmt.Errorf("refusing to redefine %s: its replication role changed to %q while this sync was running: %w",
			cfg.TargetDomain, currentTargetRole, err)
	}
	if currentTargetRole != targetRole {
		trace.Warning("target replication role changed during this sync but still permits it",
			"vm", cfg.TargetDomain, "from", targetRole, "to", currentTargetRole)
	}

	trace.Info("Adding metadata information")
	var newXML string
	newXML, err = libvirtsync.UpdateSyncMetadata(srcXML, effectiveCheckpoint, util.ReplicaHost(cfg.SourceURI, cfg.LocalHostName), cfg.SourceDomain, currentTargetRole, checkpointAt.Unix(), sourceStoppedAtCheckpoint)
	if err != nil {
		// UpdateSyncMetadata is a pure in-memory XML transformation -- no
		// network or libvirt call involved -- so a failure here is almost
		// certainly a real bug (e.g. srcXML failing to parse), not a
		// transient environmental hiccup. Silently falling back to the
		// unmodified srcXML (as this used to do) would define the target
		// with NO updated checkpoint/timestamp/failure_count metadata at
		// all, quietly disabling the metadata-vs-file-timestamp consistency
		// check (see the read of these same fields further up) for every
		// future run -- exactly the "target file changed out-of-band
		// between syncs" detection this metadata exists for -- with only an
		// easy-to-miss warning log to ever reveal it happened. Data copying
		// already succeeded by this point (dataCopySucceeded is already
		// true), so failing here does not risk the checkpoint-delete-on-
		// failure cleanup discarding a valid checkpoint -- see its own
		// doc comment.
		return fmt.Errorf("update sync metadata (checkpoint=%s): %w", effectiveCheckpoint, err)
	}

	// Maps each disk's live Source path to its resolved backing-chain root
	// file, so DefineDomain names the disk in the target's domain XML the
	// same way the actual data copy already does (see
	// disk.QcowDisk.RootSource's own doc comment) -- otherwise, whenever an
	// external snapshot exists, the domain definition and the real
	// replicated file would silently disagree on the disk's name.
	rootSourceByLiveSource := make(map[string]string, len(qcowDisks))
	for _, d := range qcowDisks {
		rootSourceByLiveSource[d.Source] = d.RootSource
	}
	defineDomainMu.Lock()
	defineDomainInFlight = true
	defineDomainMu.Unlock()
	defineErr := libvirtsync.DefineDomain(tgtMgr, cfg.TargetDomain, newXML, cfg.TargetDiskPath, rootSourceByLiveSource)
	defineDomainMu.Lock()
	defineDomainInFlight = false
	defineDomainMu.Unlock()
	if defineErr != nil {
		return defineErr
	}

	// Records this source<->target relationship on the SOURCE's own
	// definition (replica_targets, deduplicated) and strips any stale
	// target-role metadata (last_checkpoint/last_sync_timestamp/
	// failure_count) it might still be carrying from an earlier life as
	// somebody else's replication target -- e.g. after inverting
	// replication direction between a pair of domains. Deliberately
	// non-fatal: the actual replication above already fully succeeded by
	// this point, and this is discoverability bookkeeping on the source,
	// not something vmsync's own correctness checks ever read back --
	// unlike replica_source on the target (set via UpdateSyncMetadata
	// above), which shares this same non-fatal treatment for the same
	// reason.
	if err := libvirtsync.RecordReplicaTarget(srcMgr, cfg.SourceDomain, util.ReplicaHost(cfg.TargetURI, cfg.LocalHostName), cfg.TargetDomain, time.Now()); err != nil {
		trace.Warning("failed to record replica_targets metadata on source domain", "domain", cfg.SourceDomain, "error", err)
	}

	return nil
}
