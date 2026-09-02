/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>
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
	"bytes"
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

	"vmsync/pkg/blockdigest"
	"vmsync/pkg/disk"
	"vmsync/pkg/failover"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/metrics"
	"vmsync/pkg/nbdbridge"
	"vmsync/pkg/nbdsync"
	"vmsync/pkg/portalloc"
	"vmsync/pkg/remotessh"
	"vmsync/pkg/restorepoint"
	"vmsync/pkg/runresult"
	"vmsync/pkg/streamrelay"
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
//
// Aliased to util.RunLockDir rather than duplicated: vmsync-agent reads these
// same files to find out whether a sync it did not start is still running, so
// the two binaries must not be able to drift apart on the path.
const runLockDir = util.RunLockDir

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

// stampDisk pairs a replica disk's libvirt target dev with the file on the
// target host that holds it.
//
// The pairing is computed in ONE loop, and both the write side (the
// replica_written_at stamp) and the read side (the preflight's per-disk
// comparison) derive their paths from it, so the two provably name the same
// files. Two independent derivations of the same path is how a stamp ends up
// recorded against a file nobody compares.
type stampDisk struct{ dev, path string }

// targetExportName names the NBD export a replica disk is served under.
//
// It carries the DOMAIN, not just the device, and that is the whole point.
// The source side has always named its exports; the target side was
// unnamed, so a connection to a target port got "whatever is listening
// there". Two runs colliding on a port -- misconfigured to the same base,
// or handed overlapping blocks by auto-allocation -- could therefore have
// the loser read from, or WRITE INTO, the winner's disk, with nothing in
// either process able to notice.
//
// With the name, that becomes an NBD handshake failure instead. Port
// hygiene stops being the thing standing between two VMs' data.
//
// The same name is used for the writable copy export and the read-only
// verify one: they identify the same disk and never share a port.
func targetExportName(targetDomain, dev string) string { return targetDomain + "-" + dev }

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

	// The -verify modes. All three compare the target against the SAME
	// frozen source export the copy read from, so all three answer the same
	// question and none can be confused by a running guest. What separates
	// them is cost and independence, not correctness.
	//
	// verifyModeFast stops at the first differing range. Cheapest answer to
	// "is this replica intact", and the right default for a scheduled run:
	// once one byte is wrong the replica needs attention regardless of how
	// many others are.
	verifyModeFast = "fast"
	// verifyModeFull scans the whole image and reports every differing
	// range and the total bytes. Same verdict as fast, more information for
	// the person who has to act on it: one 4 KiB range is a bad cluster,
	// hundreds scattered across the image is a bad copy path or bad storage.
	verifyModeFull = "full"
	// verifyModeQemuImg shells out to `qemu-img compare` and suspends the
	// source for the duration. The arbiter: an INDEPENDENT implementation,
	// so it is the mode to reach for when one of the others reports a
	// mismatch and the question becomes whether vmsync's own comparator is
	// the thing at fault.
	//
	// It is also the only mode that still suspends, and the reason is not
	// the one the code gave for years. -verify originally read the source
	// as a local FILE (disk.CompareImages(d.RootSource, ...)), where a
	// running guest genuinely would have corrupted the comparison; the
	// suspend was added in that same commit. A later change repointed the
	// source at the frozen NBD backup export and left the suspend behind,
	// so every mode paused production guests to protect a file read that no
	// longer happened. The suspend survives HERE on its own merits: a
	// stopped guest issues no writes, so the fleecing scratch behind the
	// source export stays empty across what is otherwise the longest read
	// in the tool.
	verifyModeQemuImg = "qemu-img"
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
	// RunID is an opaque label a supervising agent passes so it can join the
	// run lock this process writes to its own record of having launched it.
	// Never interpreted here.
	RunID string
	// ResultJSON is where to write this run's degradations for a supervising
	// agent to read back -- see pkg/runresult for why neither the exit code
	// nor the log tail can carry them. Empty for a run started by hand, which
	// has nobody to report to.
	ResultJSON string

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
	// RestoredBy is whoever asked for the rollback, recorded on the domain.
	// The counterpart of -promoted-by, and there for the same reason: an
	// audit log in a control plane does not survive losing the control
	// plane, and domain metadata does.
	RestoredBy string
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
	// TimestampToleranceSec is how far a replica disk's mtime may be ahead
	// of last_sync_timestamp before the out-of-band-modification check
	// refuses. Zero, the default, is the behaviour that predates the flag.
	// See targetFileNewerThanSync for why a tolerance is needed at all.
	TimestampToleranceSec int

	Start  bool
	Reinit bool
	// ForceClean is -reinit for a target that is wedged: it additionally
	// removes the target DOMAIN, overrides the promoted/paused replication
	// role interlock, and clears a shut-down source's checkpoint chain
	// bitmaps and all. It never touches a RUNNING target.
	ForceClean             bool
	ReinitAfterFailures    int
	Verify                 string
	IgnoreExternalSnapshot bool
	IODepth                int

	// NoChecksum disables the pre-commit integrity check, which is ON by
	// default -- hence the negative name. Phrased as an opt-OUT because the
	// check is what makes a successful run mean "these bytes are on the
	// target" rather than "the writes were issued and nothing complained",
	// and a default nobody has to know about is the only kind that protects
	// the estate that never reads the flag list.
	//
	// Not the whole decision on its own: the check also needs a matching
	// vmsync-bridge-helper on the target, and is skipped with a warning when
	// there isn't one -- a missing helper must not break a sync that never
	// asked to bridge. See checksumEnabled in run(), the single place that
	// resolves it.
	//
	// Deliberately a bool rather than -checksum=<algo>. The digest algorithm
	// is a wire-compatibility property of a matched vmsync/vmsync-bridge-helper
	// pair (see pkg/blockdigest), not a preference: exposing it would invite
	// tuning a value whose only requirement is that both sides agree, and
	// would multiply the bench matrix by an axis nobody needs.
	NoChecksum bool

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
	flag.IntVar(&cfg.TimestampToleranceSec, "timestamp-tolerance-sec", 0, "How far a replica disk's mtime may be ahead of the recorded sync time before the sync refuses, in seconds. Mostly no longer needed: vmsync now records replica_written_at per disk, stat'd on the TARGET host, so where that exists both sides of the comparison come from the same clock and drift cannot trigger it. It still matters for a replica written by an older vmsync, where the only record is last_sync_timestamp taken on THIS host's clock -- a target running even a second fast then fails every incremental sync with an error blaming out-of-band modification. Set this above the drift the error reports to recover without a full -reinit, and pass it for ONE run rather than persisting it: that run records replica_written_at for every disk it writes even if it then fails, which is enough to make every later comparison exact. Fixing NTP is still the real repair")
	flag.BoolVar(&cfg.Start, "start", false, "In case vm is in non-running state, start in paused mode to allow sync")
	flag.BoolVar(&cfg.Reinit, "reinit", false, "Delete VM on target and restart a full sync process")
	flag.BoolVar(&cfg.ForceClean, "force-clean", false, "A -reinit for a target that is wedged. Implies -reinit, and additionally: removes the target DOMAIN before syncing rather than redefining it at the end, so a broken definition cannot block the run; overrides the replication_role interlock for a promoted or paused target, DISCARDING its current disks; and clears the source's checkpoint chain even when the source is shut down, removing the qcow2 bitmaps that would otherwise make every later sync fail with \"Bitmap already exists\". It never touches a RUNNING target, and never overrides role=source, which means the pair is configured backwards")
	flag.StringVar(&cfg.ReplacedDiskAction, "replaced-disk-action", replacedDiskRename, fmt.Sprintf("What to do with a target disk file that is about to be discarded and rebuilt (currently only -reinit does this): %q renames it to <path>%s<unixtime> so its contents survive, %q removes it. Defaults to %q: the target of a reinit may be a former primary whose disks still hold everything written after the last successful sync, and that is unrecoverable once deleted. Renaming needs room for both copies, and the aside files are never reaped automatically", replacedDiskRename, replacedDiskSuffix, replacedDiskDelete, replacedDiskRename))
	flag.StringVar(&cfg.TargetDiskOwner, "target-disk-owner", util.DiskOwnerAuto, fmt.Sprintf("Who should own the disk files created on the target: %q (default), %q, or an explicit \"user\", \"user:group\" or \":group\". vmsync creates those files by running qemu-img over SSH, so they are owned by that SSH user (root) -- while qemu runs as \"qemu\" on RHEL and \"libvirt-qemu\" on Debian, and cannot open a root-owned disk. libvirt's dynamic_ownership usually hides this, but it is off in plenty of deployments and cannot work at all on NFS with root_squash. %q preserves whatever owned the file before (which is what makes -reinit safe, since it replaces a correctly-owned disk with a fresh root-owned one) and otherwise takes what the target's libvirt qemu.conf sets; it never guesses, and warns instead. %q is the old behaviour", util.DiskOwnerAuto, util.DiskOwnerOff, util.DiskOwnerAuto, util.DiskOwnerOff))
	flag.IntVar(&cfg.ReinitAfterFailures, "reinit-after-failures", 0, "Reinit automatically after N failures (disabled by default). Count is held on target XML")
	flag.StringVar(&cfg.Retention, "retention", "", "Keep point-in-time copies of the replica on the target, as COUNT,INTERVAL -- for example 24,3h for twenty-four copies at least three hours apart, so a sync that faithfully replicated an already-damaged source can be stepped back from. The COUNT is the guarantee; the window it covers is not, because vmsync does not decide when it runs: the interval is a floor (\"take one if at least this long has passed\"), so a pair syncing every 4h gets 4h spacing and a pause leaves a gap. Copies are made with reflink, share storage with the replica, and cost almost nothing until they diverge -- but the target filesystem must support it (XFS with reflink=1, or btrfs), and this is refused at startup where it does not. Disabled by default")
	flag.BoolVar(&cfg.ListRestorePoints, "list-restore-points", false, "List the restore points kept on the target and stop. Needs -target-uri and -target-disk-path; reads the target filesystem only, and touches neither the replica nor libvirt")
	flag.StringVar(&cfg.CloneRestorePoint, "clone-restore-point", "", "Copy one restore point's disks to the directory given by -clone-to, and stop. Takes a tag from -list-restore-points. This is how to answer \"is that copy clean?\": boot a throwaway domain from the clone. It changes nothing about the replica, its metadata, or its role -- restoring in place is a different operation and is deliberately not this one")
	flag.StringVar(&cfg.CloneRestorePointTo, "clone-to", "", "Directory on the target to write -clone-restore-point's copies into. Created if missing")
	flag.StringVar(&cfg.RestoreRestorePoint, "restore-restore-point", "", "Put one restore point back over the replica IN PLACE, discarding its current contents. Takes a tag from -list-restore-points. -target-disk-path is optional here (unlike the read-only verbs): a restore needs the target domain to exist, so where its disks are is read from the domain itself. Without -force-restore this only prints an assessment and changes nothing. A restore is for promoting: it leaves replication PAUSED, because the next sync from the same source would otherwise overwrite exactly what was rolled back to")
	flag.BoolVar(&cfg.ForceRestore, "force-restore", false, "Carry out -restore-restore-point instead of only assessing it. Required, because a restore replaces the replica's disks and cannot be undone once the displaced contents are removed")
	flag.StringVar(&cfg.RestoredBy, "restored-by", "", "Who asked for the rollback, recorded on the domain as restored_by. The counterpart of -promoted-by: a promoted domain's data-loss window says how far back its contents are, but only this says somebody chose to put them there")
	flag.StringVar(&cfg.TestFault, "test", "", fmt.Sprintf("FOR TESTING ONLY: make vmsync deliberately fail at a chosen point, so error-recovery paths that cannot be reached from outside the process can be exercised. Accepts one of: %s. A run with this set WILL fail and its result means nothing as a replication. Listed here rather than hidden, so an operator who finds it in a log can look it up", strings.Join(libvirtsync.TestFaults, ", ")))
	compressArg := optionalValueFlag{bareDefault: "s2"}
	fenceSourceArg := optionalValueFlag{bareDefault: fenceSourceAuto}
	netBufferArg := optionalValueFlag{bareDefault: "128k,1G"}
	flag.Var(&compressArg, "compress", "Compress NBD traffic between hosts. Bare -compress (no value) defaults to \"s2\"); ACCEPTS \"zstd\" or \"s2\". Requires vmsync-bridge-helper binary on target")
	// No flag default, deliberately: one literal cannot be right for both
	// algorithms. "3" is a valid zstd level and an invalid s2 mode, "better"
	// is the reverse -- so whichever were declared, --help would print a
	// value the other algorithm refuses outright. Left empty, Go omits the
	// "(default ...)" clause altogether and the text below states both,
	// which is the only accurate thing to say before -compress is known.
	// streamrelay.ResolveLevel turns empty into the right one.
	flag.StringVar(&cfg.CompressLevel, "compress-level", "", "Compression level/mode to use when -compress is set. For -compress=zstd: a number 1-19, defaulting to 3. For -compress=s2 (which has no numeric levels, and is what bare -compress selects): one of \"default\" (s2's own fastest mode), \"better\" (the default here) or \"best\". Left unset it resolves per algorithm, so there is no single default to print.")
	flag.Var(&netBufferArg, "netbuffer", "Buffer NBD bridge traffic through a bounded in-memory buffer to smooth throughput, formatted as <blocksize>,<buffersize> (e.g. 64k,512M). Defaults to \"128k,1G\". Requires vmsync-bridge-helper binary on target")
	flag.StringVar(&cfg.BridgeHelperPath, "bridge-helper-path", "/usr/local/bin/vmsync-bridge-helper", "Remote path to the vmsync-bridge-helper binary. Defaults to /usr/local/bin")
	flag.BoolVar(&cfg.UseSSH, "use-ssh", false, "When --compress/--netbuffer is set, route the bridged NBD traffic through the existing SSH connection as an encrypted tunnel")
	flag.IntVar(&cfg.IODepth, "io-depth", 8, "Number of NBD read/write pairs to keep in flight simultaneously during the disk copy, defaults to 8")
	flag.BoolVar(&cfg.NoChecksum, "no-checksum", false, "Disable the pre-commit integrity check, which is ON by default. Normally every chunk read from the source is hashed as it passes, vmsync-bridge-helper hashes the same ranges back off the target, and an incremental sync's overlay is removed instead of committed if they disagree -- so a run that succeeds means the bytes are on the target, not merely that the writes were issued. The digests cost no extra I/O on either side and only a few bytes per megabyte on the wire. The only requirement is a matching vmsync-bridge-helper binary on the target (same binary and -bridge-helper-path as -compress/-netbuffer use, but no compression, buffering or bridge port is involved -- it is run as a one-shot command). If it is absent or a different version, the check is skipped with a warning rather than failing the sync; pass this flag to state that intent explicitly and silence the warning")
	flag.StringVar(&cfg.PrometheusTextfile, "prometheus-textfile", "", "Write sync metrics to this path in Prometheus textfile-collector format. Name should be something like /var/lib/node_exporter/textfile_collector/vmsync_[vmname].prom")
	flag.BoolVar(&cfg.IgnoreExternalSnapshot, "ignore-external-snapshot", false, "If the source domain currently has any external disk snapshot, skip this run entirely")
	flag.StringVar(&cfg.Verify, "verify", "", "After syncing, compare every disk on the target against the same frozen source snapshot the copy read from. Accepts fast|full|qemu-img. All three answer the same question and none is confused by a running guest; they differ in cost and independence. \"fast\" stops at the first differing range -- the right choice for a scheduled run. \"full\" scans the whole image and reports how many ranges and bytes differ, which is what tells a bad cluster apart from a bad copy path. \"qemu-img\" runs qemu-img compare instead, an INDEPENDENT implementation, and is the one to reach for when another mode reports a mismatch and you need to know whether to believe it; it is also the only mode that suspends the source, which it does to keep the source snapshot's scratch space empty for the duration, not because the comparison needs it")
	flag.StringVar(&cfg.UpdateRole, "update-role", "", "Set the replication role recorded in a domain's own vmsync metadata, then exit without syncing anything. Accepts "+strings.Join(libvirtsync.ValidRoles, "|")+" (\"none\" clears it). The domain is addressed with -target-uri/-target-domain regardless of which direction it currently replicates in. vmsync refuses to sync INTO a domain whose role is anything other than \"target\" or unset -- this is what stops a scheduled sync from overwriting a domain that was failed over to and then shut down for maintenance")
	flag.StringVar(&cfg.RunID, "run-id", "", "Opaque identifier for this run, written into the run lock so a supervising agent can join it to its own record of having started this process. Ignored except as a label; vmsync-agent sets it, and nothing needs it when vmsync is run by hand")
	flag.StringVar(&cfg.ResultJSON, "result-json", "", "Write this run's degradations to this path as JSON, for a supervising agent to read back. A degradation is something the exit code cannot carry -- a guest left frozen by a failed thaw, or a copy that is crash-consistent because the freeze did not take -- since both can happen to a run that otherwise succeeds. vmsync-agent sets this; nothing needs it when vmsync is run by hand")
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
	// including, for example, a trailing -verify=full. vmsync takes no
	// positional arguments at all, so any leftover ones are unambiguously a
	// mistake -- fail loudly instead of silently ignoring whatever came
	// after them.
	if flag.NArg() > 0 {
		trace.Error("invalid command line", "error", fmt.Errorf("unexpected extra argument(s) %v -- if you meant to pass a value to -compress or -netbuffer, use -compress=value / -netbuffer=value (with an \"=\"), not a space", flag.Args()))
		os.Exit(2)
	}

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
				// Nothing was done, so this must not read as a failure. The
				// caller most likely to care is the agent: a terminal result
				// burns the operation's id permanently, and "another vmsync
				// was busy" is the one outcome that deserves another go.
				if errors.Is(err, util.ErrLockHeld) {
					trace.Warning("standing down: another vmsync is already working on this domain, and nothing was changed -- retry once it finishes",
						"vm", cfg.TargetDomain, "error", err)
					os.Exit(util.ExitBusy)
				}
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
		// Empty means "the operator did not choose", so resolve it to this
		// algorithm's default before validating -- otherwise an unset level
		// would be rejected as not a zstd number.
		cfg.CompressLevel = streamrelay.ResolveLevel(streamrelay.Algo(cfg.Compress), cfg.CompressLevel)
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
	case "", verifyModeFast, verifyModeFull, verifyModeQemuImg:
	default:
		trace.Error("invalid verify configuration", "error", fmt.Errorf("-verify must be %q, %q or %q (or omitted to disable verification), got %q",
			verifyModeFast, verifyModeFull, verifyModeQemuImg, cfg.Verify))
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
			// ExitBusy, not 0. Nothing was done, and a caller must be able to
			// tell that apart from a sync that ran. Exiting 0 here is what let
			// a restarted agent -- whose in-flight bookkeeping is memory-only
			// -- launch a second vmsync per interval and record each phantom
			// as a SUCCESS, so metrics, the UI and the journal all showed
			// healthy replication while nothing was being copied.
			trace.Info("another vmsync is already syncing this domain, standing down without touching anything", "domain", cfg.SourceDomain, "error", err)
			os.Exit(util.ExitBusy)
		}
		trace.Error("failed to acquire run lock for domain -- this is not lock contention, something is actually broken (permissions, a read-only lock directory, or the lock file being repeatedly replaced)", "domain", cfg.SourceDomain, "error", err)
		os.Exit(1)
	}
	defer lockFile.Close()

	// Stamp who holds this lock into the lock file itself.
	//
	// The lock file is otherwise always empty, and it is the only record whose
	// lifetime is tied to the process it describes: only the exclusive holder
	// can write it, the kernel releases the lock with the holder however it
	// dies, and /run is tmpfs so a reboot clears the whole namespace. That is
	// what lets vmsync-agent tell "a sync I started and forgot about across a
	// restart" from "a stale file", without ever probing the lock itself --
	// probing would mean acquiring it, which is the very skip it would be
	// trying to detect.
	//
	// Best-effort on purpose. This is provenance, not correctness: a failure
	// here costs an agent one wasted process spawn later, and refusing to sync
	// because a diagnostic could not be written would be the wrong trade.
	identity := util.NewRunLockIdentity("sync", cfg.SourceDomain,
		util.ReplicaHost(cfg.TargetURI, cfg.LocalHostName)+":"+cfg.TargetDomain,
		cfg.RunID, time.Now().Unix())
	if err := util.WriteRunLockIdentity(lockFile, identity); err != nil {
		trace.Warning("could not record this process's identity in its run lock; an agent restarting during this sync may launch a second one, which will stand down harmlessly",
			"domain", cfg.SourceDomain, "error", err)
	}

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

	// -force-clean is a -reinit that also removes obstacles, so it turns the
	// flag on rather than duplicating everything the reinit path already does
	// -- the disk removal, the restore point sweep, the ownership carry-over.
	//
	// Not marked ReinitAutomatic: that flag means "nobody asked for this", and
	// it is what stops -reinit-after-failures quietly discarding restore
	// points. Somebody typed this one, so the ordinary rules about what a
	// deliberate reinit takes with it apply.
	if cfg.ForceClean && !cfg.Reinit {
		trace.Warning("-force-clean implies -reinit")
		cfg.Reinit = true
	}

	// Consecutive failure count lives in the target domain's own vmsync
	// metadata (alongside last_checkpoint/last_sync_timestamp), not in local
	// state -- so it survives being tracked from a different host and stays
	// in one place with the rest of vmsync's bookkeeping.
	//
	// Exactly ONE thing is exempt from this counter: a verification that ran
	// and found the images different (isVerifyMismatch). Auto-reinit answers
	// a failure by discarding the checkpoint chain and recopying, which for
	// a corruption finding would destroy the evidence before anybody saw it.
	//
	// Everything else on a -verify run counts, and that is the fix. This
	// used to be gated on `cfg.Verify != ""` -- "verification was REQUESTED"
	// -- which is not the same question at all. It exempted the SSH session
	// that died, the copy that failed, the preflight refusal, and the
	// checkpoint-chain inconsistency that contrib/bench/bench.sh's Stage 3
	// uses as the canonical thing auto-reinit exists to heal. An operator
	// running -verify on their scheduled syncs, which is the whole point of
	// having it, therefore had -reinit-after-failures silently doing
	// nothing: nothing was ever counted, so the threshold was never reached.
	//
	// The codebase already draws this line elsewhere -- verificationRan()
	// exists because cfg.Verify alone stays true for a run that never
	// reached the compare at all.
	if cfg.ReinitAfterFailures > 0 {
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
		// The target-side counterpart of the source-lock skip above, and it
		// exits the same way for the same reason: nothing was touched, no
		// failure counted, and (see run()'s metrics defer) no metrics record
		// written -- but ExitBusy rather than 0, so the caller can tell "stood
		// down" from "ran". Left as 0, this is the same phantom-success hole
		// as the source lock, reached whenever two sources replicate into one
		// target host.
		if errors.Is(err, util.ErrLockHeld) {
			trace.Info("another vmsync is already working on this target, standing down without touching anything", "vm", cfg.TargetDomain, "error", err)
			os.Exit(util.ExitBusy)
		}
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
		//
		// A verification that RAN and found a difference is exempt for a
		// related but distinct reason: it is not a broken sync either, it is
		// a finding about the data, and the reinit this counter would force
		// is precisely the wrong response to one. A compare that could not
		// run is NOT exempt -- that is a broken sync like any other. See
		// isVerifyMismatch.
		if cfg.ReinitAfterFailures > 0 && !isVerifyMismatch(err) && !errors.Is(err, libvirtsync.ErrRoleRefusesSync) {
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

// targetFileNewerThanSync reports whether a replica disk looks like it was
// written to behind vmsync's back since the last sync, and by how much.
//
// THE TWO TIMESTAMPS COME FROM DIFFERENT CLOCKS, and that is the whole reason
// this function takes a tolerance. mtime is `stat -c %Y` on the TARGET host,
// so it is the target's clock; lastSync is last_sync_timestamp, written by
// UpdateSyncMetadata from time.Now() on the host RUNNING vmsync, which is the
// source side. Comparing them is only meaningful to the extent those two
// clocks agree.
//
// With no tolerance -- which is what this was before the flag existed -- a
// target whose clock is even one second ahead of the source's fails this check
// on every incremental sync, forever, with an error blaming out-of-band
// modification. The replica is fine; the clocks are not. That is a permanent,
// self-inflicted replication outage caused by NTP drift, and recovering from
// it needed either a full -reinit or a clock fix on a host an operator may not
// control.
//
// The check is still worth having: it catches somebody writing to the replica
// through the filesystem between syncs, which no checkpoint comparison can
// see. A tolerance narrows it rather than removing it -- an out-of-band write
// is normally minutes or hours after a sync, not inside the window two NTP
// clients disagree by.
//
// Parsed as integers rather than compared as strings. The old string
// comparison happened to order 10-digit unix timestamps correctly and will
// keep doing so until the year 2286, but it silently reported "newer" for any
// value that was not a number at all -- a stat that printed an error, say --
// which is the wrong answer in the dangerous direction.
func targetFileNewerThanSync(mtime, lastSync string, tolerance time.Duration) (newer bool, aheadBy time.Duration, err error) {
	m, err := strconv.ParseInt(strings.TrimSpace(mtime), 10, 64)
	if err != nil {
		return false, 0, fmt.Errorf("target file mtime %q is not a unix timestamp: %w", mtime, err)
	}
	s, err := strconv.ParseInt(strings.TrimSpace(lastSync), 10, 64)
	if err != nil {
		return false, 0, fmt.Errorf("last_sync_timestamp %q is not a unix timestamp: %w", lastSync, err)
	}
	ahead := time.Duration(m-s) * time.Second
	if tolerance < 0 {
		tolerance = 0
	}
	return ahead > tolerance, ahead, nil
}

// isVerifyMismatch reports whether err is a verification that RAN and found
// the images genuinely different, as opposed to one that could not be
// performed.
//
// The distinction decides what automation is allowed to do about it, and the
// two answers are opposite. A compare that could not run -- an SSH session
// that died, an export that never came up, a stalled pipeline, a cancelled
// context -- says the sync mechanism is unhealthy, which is exactly what
// -reinit-after-failures exists to notice and heal. A compare that ran and
// found a difference says the DATA is suspect, and answering that by
// automatically discarding the checkpoint chain and recopying would destroy
// the evidence of a corruption finding before anybody saw it.
//
// Both nbdsync and disk raise their own ErrImagesDiffer for exactly this,
// and -verify=full wraps the same sentinel around its collected result, so
// all three modes are classified by one rule rather than by which mode
// happened to run.
func isVerifyMismatch(err error) bool {
	return errors.Is(err, nbdsync.ErrImagesDiffer) || errors.Is(err, disk.ErrImagesDiffer)
}

// syncFloor picks the timestamp a replica disk's mtime is judged against,
// and reports whether it came from replica_written_at.
//
// MAX, never "replica_written_at always wins", and the reason is a property
// rather than a preference: max can only ever make the check MORE
// permissive. A change whose whole purpose is removing a spurious refusal
// must not be able to create one, and max makes that provable in a line.
//
// The two floors differ in which clock they were taken on, which is what
// decides the outcome in each direction of skew:
//
//   - Target clock AHEAD of this host's. The only direction that ever
//     produced a false refusal, because it is the only one where a healthy
//     replica's mtime exceeds last_sync_timestamp. Here replica_written_at
//     is the larger value and wins, and it was stat'd on the same clock as
//     the mtime -- so the comparison becomes exact and the NTP false
//     positive that -timestamp-tolerance-sec exists for is simply gone.
//   - Target clock BEHIND. last_sync_timestamp wins, leaving a window as
//     wide as the skew in which an out-of-band write goes unnoticed. That is
//     the permissive direction, which never refused anything, and the skew
//     is sub-second under working NTP -- vmsync warns past 30s on its own.
//     Anyone running -timestamp-tolerance-sec=60 has already accepted a
//     blind spot sixty times larger, deliberately.
//
// So: replica_written_at is the accurate floor, last_sync_timestamp is a
// compatibility floor that can only relax the check. A replica that predates
// the field, or a disk with no entry, keeps exactly today's behaviour.
func syncFloor(lastSync string, writtenAt int64, haveWrittenAt bool) (floor string, fromWrittenAt bool) {
	if !haveWrittenAt {
		return lastSync, false
	}
	s, err := strconv.ParseInt(strings.TrimSpace(lastSync), 10, 64)
	if err != nil {
		// An unparsable last_sync is targetFileNewerThanSync's error to
		// report, not this function's to hide -- but a usable stamp is
		// strictly better than a value that cannot be compared at all.
		return strconv.FormatInt(writtenAt, 10), true
	}
	if writtenAt > s {
		return strconv.FormatInt(writtenAt, 10), true
	}
	return lastSync, false
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

// formatRemoteStderr renders a remote command's stderr for appending to an
// error message, or "" when there was none.
//
// Its own function so that the empty case is handled in one place: a bare
// ": " with nothing after it, on the many runs where the remote command says
// nothing on stderr, reads as a truncated message and invites a hunt for the
// missing half.
func formatRemoteStderr(stderr string) string {
	if strings.TrimSpace(stderr) == "" {
		return ""
	}
	return " (remote stderr: " + strings.TrimSpace(stderr) + ")"
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
//
// The integrity check deliberately does NOT appear here, in either of its
// two forms. The pre-commit check exports over a UNIX SOCKET, so it needs
// no port at all; the digest-based -verify reuses the verify export already
// counted at +2N. Neither adds to a run's reservation, which is what keeps
// a check that is on by default from silently widening every run's port
// span -- and from shrinking how many runs an auto-allocated range can
// hold.
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
// The runner is an interface rather than *remotessh.Client because a restore
// can run on the target host itself, where reaching the filesystem is exec
// and not ssh. Nothing in here needed the concrete type: the two qemu-account
// helpers it calls already took this same one-method seam.
// detectTargetQemuOwner works out which account libvirt runs qemu as on the
// target, once per run: every disk gets the same answer and this costs
// several SSH round trips.
//
// Split out because the DIRECTORIES a disk lives in need the same answer, and
// they are created long before the disk itself exists -- so the resolution
// can no longer live inside the function that chowns the file.
func detectTargetQemuOwner(ctx context.Context, client util.CommandRunner) util.DiskOwner {
	targetQemuOwnerOnce.Do(func() {
		targetQemuOwnerOnce.owner = util.ReadQemuConfOwner(ctx, client)
		if !targetQemuOwnerOnce.owner.Empty() {
			return
		}
		// qemu.conf said nothing, which is the ORDINARY case: every
		// distribution ships that setting commented out, so this is where a
		// first-ever sync lands rather than an exotic corner. Fall back to
		// which well-known qemu account the host actually has.
		targetQemuOwnerOnce.owner, targetQemuOwnerOnce.candidates =
			util.DetectQemuAccount(ctx, client)
	})
	return targetQemuOwnerOnce.owner
}

// targetDirOwner is who a newly-created target DIRECTORY should belong to.
//
// The same account the disk inside it will get, resolved the same way, minus
// the "what owned the previous file" evidence -- there is no previous file
// when a directory is being created for the first time.
//
// Returns the zero owner when there is nothing to apply (-target-disk-owner
// off, or auto that resolved to nothing), and the caller then creates the
// directory exactly as it always did.
func targetDirOwner(ctx context.Context, client util.CommandRunner, cfg syncConfig) util.DiskOwner {
	owner, err := util.ParseDiskOwner(cfg.TargetDiskOwner)
	if err != nil || owner.IsOff() {
		return util.DiskOwner{}
	}
	if !owner.Empty() {
		return owner
	}
	return detectTargetQemuOwner(ctx, client)
}

func applyTargetDiskOwner(ctx context.Context, client util.CommandRunner, cfg syncConfig, targetPath string, replaced util.DiskOwner) error {
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
			detectTargetQemuOwner(ctx, client)
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
	// every consumer site.
	//
	// Only qemu-img suspends, and NOT because suspending affects what is
	// compared -- see verifyModeQemuImg for why it does not, and for the
	// history that left every mode suspending long after it stopped
	// mattering. It suspends because a stopped guest issues no writes, so
	// the fleecing scratch behind the source export stays empty for the
	// whole compare. That is worth having on the one mode meant to be run
	// deliberately, on a full image, as the tie-breaker.
	verifySuspends := cfg.Verify == verifyModeQemuImg
	verifyFast := cfg.Verify == verifyModeFast
	verifyFull := cfg.Verify == verifyModeFull

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
	// stampDisks pairs each replica disk's libvirt target dev with the file
	// on the target host holding it, so replica_written_at is keyed by the
	// same expression on the write side as the preflight uses on the read
	// side. Populated once the disk list is known; read from the signal
	// handler's goroutine too, hence the mutex.
	var stampMu sync.Mutex
	var stampDisks []stampDisk
	var sourceCleanupOnce sync.Once
	var resumeOnce sync.Once
	var suspendedForVerify bool
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
	// fsFreezeFailed and fsThawFailed both need the same mutex, for the same
	// reason: the metrics closure reads them from the SIGNAL HANDLER's
	// goroutine while this one is still running the sync.
	//
	// fsFreezeFailed used to be safe unguarded only because nothing off this
	// goroutine read it -- the handler passed a literal state and
	// finalRunState ran on the normal return path. Reporting freeze as its
	// own metric is what made it shared, so it is guarded now.
	//
	// Deliberately kept as two flags: the copy can be perfect and the source
	// still be hung, and reporting one as the other loses whichever it is not.
	var freezeFailedMu sync.Mutex
	var fsFreezeFailed bool
	var fsThawFailed bool
	setFreezeFailed := func() {
		freezeFailedMu.Lock()
		fsFreezeFailed = true
		freezeFailedMu.Unlock()
	}
	freezeDidFail := func() bool {
		freezeFailedMu.Lock()
		defer freezeFailedMu.Unlock()
		return fsFreezeFailed
	}
	setThawFailed := func() {
		freezeFailedMu.Lock()
		fsThawFailed = true
		freezeFailedMu.Unlock()
	}
	thawDidFail := func() bool {
		freezeFailedMu.Lock()
		defer freezeFailedMu.Unlock()
		return fsThawFailed
	}
	var started bool = false
	var metricsMu sync.Mutex
	diskMetrics := make([]metrics.DiskMetric, 0)
	var nbdHost, targetNBDHost string
	// Declared up here with the rest of the metricsMu-guarded state, not
	// down where the source bridge is actually started: writeMetricsTextfile
	// closes over it and is defined further up still, so a declaration at
	// the assignment site is simply not in scope for it. Nil until the
	// bridge exists, which is the state the signal handler can genuinely
	// observe -- it can fire before setup gets that far.
	var sourceBridgeCounters *nbdbridge.ByteCounters
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
		// Read here rather than per disk: the source bridge is one shared
		// listener for the whole run, so its totals belong to the run.
		//
		// Under metricsMu for the same reason as nbdHost above, and it is
		// the POINTER that needs the lock, not the counters: the counters
		// are atomic, but sourceBridgeCounters is assigned once by this
		// goroutine partway through setup, and the signal handler can call
		// this before that assignment has happened.
		var srcBridgeRecv, srcBridgeSent uint64
		if sourceBridgeCounters != nil {
			srcBridgeRecv = sourceBridgeCounters.ReceivedSnapshot()
			srcBridgeSent = sourceBridgeCounters.SentSnapshot()
		}
		metricsMu.Unlock()
		now := time.Now().Unix()
		run := metrics.RunMetric{
			SourceHost: sourceHost,
			TargetHost: targetHost,
			VM:         cfg.SourceDomain,
			State:      state,
			Timestamp:  now,

			SourceBridgeReceivedBytes: srcBridgeRecv,
			SourceBridgeSentBytes:     srcBridgeSent,
			// Read through the accessor: this can run from the signal
			// handler's goroutine while thawSource is setting it from
			// another.
			FSFreezeFailed:        freezeDidFail(),
			FSThawFailed:          thawDidFail(),
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

	// writeRunResult tells a supervising agent what the exit code cannot.
	//
	// Its own function rather than a branch inside writeMetricsTextfile,
	// because that one returns early when no -prometheus-textfile is set --
	// and an operator running no node_exporter must not thereby lose the
	// report that their guest is still frozen. Called from the same two
	// places, for the same reason: os.Exit skips defers, and the interrupted
	// run is one that very plausibly left a guest frozen.
	writeRunResult := func() {
		if cfg.ResultJSON == "" {
			return
		}
		res := runresult.Result{
			VM:             cfg.SourceDomain,
			RunID:          cfg.RunID,
			FSFreezeFailed: freezeDidFail(),
			FSThawFailed:   thawDidFail(),
		}
		if err := runresult.Write(cfg.ResultJSON, res); err != nil {
			// Warning, not Error: this file is how the agent LEARNS about a
			// degradation, so failing to write it cannot itself be counted as
			// one. The agent reports the absence in its own words.
			trace.Warning("failed to write run result", "path", cfg.ResultJSON, "error", err)
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
		// A run that stood down because another vmsync holds the target lock
		// writes NOTHING, the same as the source-side lock skip -- which
		// happens before run() is entered and so never reaches this defer at
		// all. Neither state would be true here: StateFailure is what the
		// skip exists to avoid, and StateSuccess would claim a sync that did
		// not happen. Leaving the previous run's record in place is what a
		// skip means, and it keeps vmsync_last_run_timestamp_seconds honest
		// about when this pair last actually synced.
		if errors.Is(runErr, util.ErrLockHeld) {
			return
		}
		interruptedMu.Lock()
		wasInterrupted := interrupted
		interruptedMu.Unlock()
		writeMetricsTextfile(finalRunState(runErr, wasInterrupted, freezeDidFail()))
		// After the metrics, and NOT skipped for the lock-held case above: a
		// run that stood down touched no guest, so it has no degradation to
		// report and the agent already knows what exit 75 means.
		writeRunResult()
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
		// -force-clean overrides two of the four refusals, and only those two.
		//
		// promoted and paused both mean "this replica is in a state a sync
		// must not blunder into", and getting out of them is exactly what a
		// deliberate clean is for. Loud rather than silent: a promoted domain
		// is one somebody failed over TO, so its disks are live data, and the
		// operator is told precisely what is being discarded.
		//
		// source and an unrecognised role are NOT overridden, and that is not
		// timidity. role=source says this domain is the primary of its pair,
		// so syncing into it overwrites the original with its own replica --
		// which is a reversed -source-uri/-target-uri, not a mess to clean,
		// and no amount of force makes destroying the primary the intent. An
		// unrecognised role was written by a newer vmsync and fails closed for
		// the reason it always does.
		if cfg.ForceClean && (targetRole == libvirtsync.RolePromoted || targetRole == libvirtsync.RolePaused) {
			trace.Warning("-force-clean: overriding the replication role interlock and DISCARDING this domain's current disks",
				"vm", cfg.TargetDomain, "role", targetRole, "refusal", err.Error())
		} else {
			return fmt.Errorf("refusing to sync into %s: %w", cfg.TargetDomain, err)
		}
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

	// Unconditional, and now purely an upgrade path: NOTHING creates a
	// verify-window checkpoint any more. This self-heals one left behind by
	// an older build -- either a crashed run of one, or simply the first run
	// after upgrading past the version that made them. Cheap (one lookup,
	// delete only if found), and safe to run whatever -verify says --
	// AcquireRunLock already
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
				// Unlike abortBackup/the checkpoint cleanup, this used to
				// have no reconnect fallback at all --
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
	// measureReplicaWrittenAt stats every replica disk this run is
	// responsible for and renders the replica_written_at value describing
	// them (see libvirtsync.MetadataFieldReplicaWrittenAt).
	//
	// context.Background(), NEVER run()'s ctx. reportWorkerErr cancels ctx
	// BEFORE it pushes the error, and wg.Wait() returns strictly after that,
	// so on every failure path -- which is precisely what this stamp exists
	// for -- ctx is already done by the time this runs. remotessh.Client.Run
	// answers a done ctx before it even opens a session, so with ctx this
	// would fail ~100% of the time in its primary case and appear to work in
	// every success-path test. Same reason cleanupTargetNBD above and
	// cleanupSourceBridge below abandon ctx.
	//
	// Returns "" for "nothing to record", which the writer treats as a
	// no-op. Never fails the run: a missing stamp costs precision on the
	// next run's check, and replacing a real failure with this one would be
	// a strictly worse trade.
	measureReplicaWrittenAt := func(trigger string) string {
		stampMu.Lock()
		disks := append([]stampDisk(nil), stampDisks...)
		stampMu.Unlock()
		if len(disks) == 0 {
			return ""
		}
		sshClientMu.Lock()
		client := targetSSHClient
		sshClientMu.Unlock()
		if client == nil {
			return ""
		}

		paths := make([]string, 0, len(disks))
		devByPath := make(map[string]string, len(disks))
		for _, d := range disks {
			paths = append(paths, d.path)
			devByPath[d.path] = d.dev
		}
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		out, err := client.Run(sctx, util.StatMTimesCommand(paths))
		if err != nil {
			trace.Warning("could not read the replica disks' modification times, so this run will not record when it wrote them; if it fails, the next run's out-of-band-write check may refuse it",
				"trigger", trigger, "error", err, "output", out)
			return ""
		}
		byDev := make(map[string]int64, len(disks))
		for p, mtime := range util.ParseStatMTimes(out) {
			if dev, ok := devByPath[p]; ok {
				byDev[dev] = mtime
			}
		}
		return util.FormatReplicaWrittenAt(byDev)
	}

	// recordReplicaWrittenAt writes the stamp onto the TARGET domain,
	// best-effort.
	//
	// A narrow metadata merge, not a DefineDomain: this can run against a
	// target that is promoted and RUNNING, and rewriting a live domain's
	// whole definition from a typed round-trip is how configuration goes
	// missing. No removals, so failure_count is untouched.
	recordReplicaWrittenAt := func(trigger, value string) {
		if value == "" {
			return
		}
		exists, err := libvirtsync.DomainExists(tgtMgr.Conn, cfg.TargetDomain)
		if err != nil || !exists {
			// A first full sync, or -reinit/-force-clean having undefined
			// the target: there is nothing to write to yet, and the run's
			// own DefineDomain carries the value instead. Not a warning --
			// this is the ordinary shape of a first run.
			trace.Debug("no target domain to record the replica write against yet", "trigger", trigger, "vm", cfg.TargetDomain)
			return
		}
		if err := libvirtsync.SetDomainMetadataFields(tgtMgr, cfg.TargetDomain, map[string]string{
			libvirtsync.MetadataFieldReplicaWrittenAt: value,
		}); err != nil {
			trace.Warning("could not record when this run wrote the replica disks; if this run fails, the next one's out-of-band-write check may refuse it",
				"trigger", trigger, "vm", cfg.TargetDomain, "error", err)
		}
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
	// abortBackup/cleanupTargetNBD/cleanupSourceBridge/
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
				if libvirtsync.ThawFs(srcDom, true) {
					setThawFailed()
				}
				return nil
			}); err != nil {
				// A timeout leaves the guest frozen just as surely as a
				// refusal does -- the call never completed, so nothing
				// unfroze it.
				setThawFailed()
				trace.Error("thaw source filesystem timed out; this guest may still have its filesystems FROZEN and block on every write until somebody runs virsh domfsthaw against it", "trigger", trigger, "error", err)
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
			// Six, not seven: cleanupVerifyWindow is gone along with the
			// second backup job it existed to tear down. abortBackup now
			// covers the only backup job a run ever has.
			cleanupWg.Add(6)
			go func() { defer cleanupWg.Done(); abortBackup(sig.String()) }()
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
			// Record what was written to the replica, for the same reason as
			// the two writes below: os.Exit skips run() entirely, so the
			// stamp taken there never happens on this path. An interrupted
			// run has usually written disks -- vmsync-agent SIGTERMs every
			// scheduled sync on shutdown or reload -- and without this the
			// next run refuses on a fresh mtime nothing accounts for.
			//
			// The cleanup goroutines above have already killed every target
			// export, so nothing there still holds a replica disk open.
			//
			// PARTIAL BY DESIGN: this handler deliberately does not wait for
			// the disk goroutines, so a disk still inside qemu-img commit
			// right now is not recorded. That under-records -- the next run
			// may still refuse, exactly as it does today -- and never
			// over-records, which is the safe direction. See
			// MetadataFieldReplicaWrittenAt.
			recordReplicaWrittenAt(sig.String(), measureReplicaWrittenAt(sig.String()))
			// Mirrors the deferred metrics write further up in run() --
			// duplicated here for the same reason as the checkpoint cleanup
			// above: os.Exit below skips that defer entirely. An interrupted
			// run is always recorded as a failure, regardless of how far the
			// sync had gotten.
			writeMetricsTextfile(metrics.StateFailure)
			// And the result file, for the same reason -- with more at stake.
			// The cleanup above has just tried to thaw the guest, and this is
			// the path where that thaw is most likely to have failed: the
			// process is already being torn down, possibly because something
			// is wedged. If it did fail, this file is the only thing that will
			// ever tell the agent that a production guest was left frozen.
			writeRunResult()
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

	// checksumEnabled is THE decision about the pre-commit integrity check
	// for this run, resolved once, here.
	//
	// Resolved in one place rather than re-derived at each use, and that is
	// not tidiness: the port layout reserves a block for this check, and the
	// copy and the check itself each consult it. Three copies of a predicate
	// that must agree is precisely how a run ends up binding outside the
	// range it reserved -- the hazard targetPortsNeeded's own comment exists
	// to warn about. It sits here because this is the earliest point where
	// both things it depends on exist: the flag, and an SSH connection to
	// the target to ask about the helper.
	//
	// Note what it does NOT depend on: -compress/-netbuffer. The check runs
	// vmsync-bridge-helper as a ONE-SHOT command over SSH, so it needs no
	// relay running, no bridge port and no compression -- only the binary
	// present at -bridge-helper-path. Tying it to bridging would have denied
	// the check to every estate that deploys the helper but syncs over a
	// fast local link, purely because bridging is where that binary was
	// historically needed. Asking the target directly costs two cheap SSH
	// commands per run and answers the actual question.
	checksumEnabled := false
	switch {
	case cfg.NoChecksum:
		trace.Info("checksum: pre-commit integrity check disabled by -no-checksum")
	case bridgeCfg.Enabled():
		// nbdbridge.CheckRemote below hard-fails on a helper that is missing
		// or version-mismatched, so on a bridged run there is nothing to
		// probe for: either the helper is good or the run does not proceed.
		// Skipping the probe here avoids asking the same two questions twice.
		checksumEnabled = true
		trace.Info("checksum: pre-commit integrity check enabled", "algo", blockdigest.DefaultAlgo, "helper", cfg.BridgeHelperPath)
	default:
		st := nbdbridge.ProbeHelper(ctx, targetSSHClient, bridgeCfg, targetSSHConfig.Address)
		if st.Usable {
			checksumEnabled = true
			trace.Info("checksum: pre-commit integrity check enabled", "algo", blockdigest.DefaultAlgo, "helper", cfg.BridgeHelperPath, "helper_version", st.Version)
		} else {
			// Not an error: nothing about this run asked for the helper, so a
			// missing or mismatched one means no check rather than no sync.
			// Said out loud every time, because a silently absent integrity
			// check is worse than none -- an operator who believes it ran
			// would trust a replica it never looked at.
			trace.Warning("checksum: pre-commit integrity check SKIPPED -- vmsync-bridge-helper "+st.Reason,
				"remedy", "deploy a matching vmsync-bridge-helper on the target to enable it, or pass -no-checksum to state that intent and silence this")
		}
	}

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
		// Wrapped, not swallowed: main() reads ErrLockHeld off this and
		// exits 0 without counting a failure, exactly as it already does for
		// the source-side lock -- which is what AcquireRemoteRunLock's own
		// doc comment says callers should do. Contention means another
		// vmsync is working on this target right now, which is a reason to
		// stand down and let it, not evidence that anything is broken.
		//
		// Treating it as a failure had a cost beyond a misleading exit code:
		// it counted toward -reinit-after-failures, so two agents syncing
		// different sources into one target host, or a cron run colliding
		// with a scheduled one, would climb that counter on a perfectly
		// healthy replica -- eventually forcing a full resync nobody asked
		// for, and blocking promotion on a non-zero failure_count in the
		// meantime.
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

		// -force-clean removes the target DEFINITION as well, before anything
		// else touches it.
		//
		// A plain -reinit deliberately leaves it alone and lets DefineDomain
		// replace it at the end, so a failed sync still has the old definition
		// to fall back on. That is the right default and the wrong one for a
		// domain whose definition is itself the obstacle: a half-applied
		// redefine, a UUID collision, checkpoint metadata libvirt will not
		// undefine around, an NVRAM file confusing the plain Undefine path.
		// This is the flag for when the target is wedged, so the definition
		// goes too -- and if the sync then fails there is no target domain
		// left, which is the trade being asked for by name.
		if cfg.ForceClean {
			if err := forceCleanTargetDomain(tgtMgr, cfg); err != nil {
				return fmt.Errorf("force-clean: %w", err)
			}
		}
		if err := libvirtsync.AbortActiveBlockJobs(srcDom, qcowDisks); err != nil {
			return fmt.Errorf("reinit: abort active block jobs: %w", err)
		}
		// Via dropCheckpointChain rather than DeleteAllManagedCheckpoints
		// directly, so this works on a SHUT-DOWN source too. Deleting a
		// checkpoint merges its bitmap into the next one, which only a running
		// qemu can do -- so a reinit of a stopped source used to fail here with
		// libvirt's "cannot delete checkpoint for inactive domain", which is a
		// legitimate thing to want (stop the source, reinit, promote). The
		// offline path removes the bitmaps with qemu-img and then the metadata,
		// always both.
		if err := dropCheckpointChain(srcDom, cfg.SourceDomain, cfg.SourceURI, cleanVerb(cfg)); err != nil {
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
			// Directories this creates get the same owner the disk inside them
			// will get. Without it they stay owned by the SSH user -- root --
			// and under a restrictive umask the chain is 0700, so qemu cannot
			// traverse to a disk it demonstrably owns. Directories that already
			// exist are never touched: the target commonly lives under
			// something the operator set up, and re-owning that because a
			// replica landed inside it would be the worse bug.
			if _, err := targetSSHClient.Run(ctx, util.MkdirOwnedCommand(targetDirOwner(ctx, targetSSHClient, cfg), targetDir)); err != nil {
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
				// Per-disk record of when vmsync itself last wrote each
				// replica file, on the TARGET's own clock. Absent on a
				// replica written by an older vmsync, and absent per disk
				// for anything that build never stamped -- both fall back to
				// last_sync_timestamp, i.e. to exactly today's behaviour.
				writtenAtRaw, wErr := libvirtsync.ParseMetadata(tgtXML, libvirtsync.MetadataFieldReplicaWrittenAt)
				if wErr != nil {
					trace.Warning("could not read when this replica was last written; falling back to the last sync timestamp, which is a different clock and may refuse a healthy replica", "error", wErr)
				}
				writtenAt := util.ParseReplicaWrittenAt(writtenAtRaw)
				for _, d := range qcowDisks {
					targetPath = util.SetTargetPath(cfg.TargetDiskPath, d.RootSource)
					// Deliberately still one stat per disk, and still a hard
					// error: on an incremental sync a MISSING target file is
					// a real problem, and the tolerant batch form used for
					// writing the stamp would swallow it.
					out, err := targetSSHClient.Run(ctx, "stat -c '%Y' "+util.ShQuote(targetPath))
					if err != nil {
						return fmt.Errorf("%w: %s", err, out)
					}
					stamp, haveStamp := writtenAt[d.TargetDev]
					floor, fromWrittenAt := syncFloor(metadataEntryTimestamp, stamp, haveStamp)
					newer, aheadBy, cmpErr := targetFileNewerThanSync(out, floor, time.Duration(cfg.TimestampToleranceSec)*time.Second)
					if cmpErr != nil {
						return fmt.Errorf("comparing %s against the last sync timestamp: %w", targetPath, cmpErr)
					}
					if newer && fromWrittenAt {
						// Both sides came from the target's own clock, so
						// drift is ruled out and the tolerance would only
						// hide a real finding. Say so, rather than repeating
						// the cross-clock advice that no longer applies.
						return fmt.Errorf("target file %s has an mtime %s newer than the last sync timestamp recorded for this replica (replica_written_at %s=%d) -- BOTH of those come from the target host's own clock, so this is not clock drift: something wrote to this replica since vmsync last did. Find that writer; raising -timestamp-tolerance-sec would only hide it",
							targetPath, aheadBy, d.TargetDev, stamp)
					}
					if newer {
						// The skew is named, because the number is what tells
						// the two causes apart: a few seconds is two hosts'
						// clocks disagreeing, and hours is somebody having
						// written to the replica.
						return fmt.Errorf("target file %s has an mtime %s newer than the last sync timestamp, beyond the %ds tolerance -- either something wrote to the replica between syncs, or this host's clock and the target's disagree by that much (they are different clocks: the mtime is the target's, last_sync_timestamp is this host's). If it is clock drift, fix NTP or raise -timestamp-tolerance-sec above %d. This replica carries no per-disk replica_written_at for %s yet, so the comparison is still cross-clock; one successful sync records one and makes it exact",
							targetPath, aheadBy, cfg.TimestampToleranceSec, int64(aheadBy.Seconds()), d.TargetDev)
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
			// Warning rather than Error, and deliberately not retried: unlike
			// a failed thaw, this degrades the COPY, not the running guest.
			// The run goes on and produces a usable crash-consistent
			// checkpoint. Retrying would delay the checkpoint on every run of
			// every guest with no agent -- a permanent condition, not a
			// transient one -- to buy nothing.
			//
			// Say what it costs, though. "Filesystem freeze failed" alone
			// reads as a step that did not happen; what it means is that
			// everything this checkpoint captures is at the mercy of whatever
			// the guest had not flushed.
			trace.Warning("Filesystem freeze failed: this checkpoint is CRASH-CONSISTENT only, not application-consistent -- a database restored from it recovers as if the host had lost power", "error", err)
			setFreezeFailed()
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
	if bridgeCfg.Enabled() && sourceNeedsSSH {
		// The source has a single shared NBD export (no per-disk ports), so
		// its bridge port simply sits right next to it.
		sourceBridgePort := cfg.SourceNBDPort + 1
		stopCmd, err := nbdbridge.StartRemote(ctx, sourceSSHClient, "src-"+cfg.SourceDomain, sourceBridgePort, cfg.SourceNBDPort, bridgeCfg)
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
		// Under metricsMu because writeMetricsTextfile reads this pointer,
		// and it can run from the signal handler's goroutine at any moment
		// -- including right now, mid-setup. Guarding only the read would
		// leave the race intact.
		metricsMu.Lock()
		sourceBridgeCounters = counters
		metricsMu.Unlock()
		trace.Info("source nbd port in use", "side", "source", "kind", "bridge_local", "host", "127.0.0.1", "port", localPort)
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

	// askTargetDigests has vmsync-bridge-helper hash the given ranges off an
	// export on the target host, and returns the digests it reports.
	//
	// Shared by the two callers that need it -- the pre-commit integrity
	// check and the digest-based -verify -- because the interesting part is
	// identical for both and getting it subtly different in two places is
	// how one of them ends up misreporting version skew as corruption. What
	// differs between them is only which export and which ranges, so those
	// are the parameters.
	//
	// nbdAddr is where the export can be reached FROM THE TARGET HOST, never
	// a local bridge address: either a Unix socket path (the pre-commit
	// check, which needs no port at all) or 127.0.0.1:<target port> (the
	// verify export, which is on TCP because vmsync reads it too). The
	// helper reaching it locally is the whole reason this is cheap.
	askTargetDigests := func(dev string, nbdAddr string, exportName string, plan []blockdigest.Block) ([]blockdigest.Block, error) {
		header := blockdigest.DefaultHeader(blockdigest.MaxRangeLength(plan))
		var request bytes.Buffer
		if err := blockdigest.WriteRequest(&request, header, blockdigest.RangesFromBlocks(plan)); err != nil {
			return nil, fmt.Errorf("checksum: build request for %s: %w", dev, err)
		}

		// No bridge is involved: the exchange is a few bytes per megabyte
		// over this SSH command channel, so there is nothing for one to
		// compress.
		helperCmd := util.ShQuote(cfg.BridgeHelperPath) +
			" -checksum -nbd " + util.ShQuote(nbdAddr) +
			" -export " + util.ShQuote(exportName)

		stdout, stderr, err := targetSSHClient.RunWithInput(ctx, helperCmd, request.Bytes())
		if err != nil {
			return nil, fmt.Errorf("checksum: run %s on the target for %s: %w%s -- pass -no-checksum to run without digest checks if the helper is not deployed there yet",
				cfg.BridgeHelperPath, dev, err, formatRemoteStderr(stderr))
		}

		respHeader, blocks, err := blockdigest.ReadResponse(strings.NewReader(stdout))
		if err != nil {
			return nil, fmt.Errorf("checksum: read the target's digests for %s: %w%s", dev, err, formatRemoteStderr(stderr))
		}
		if err := respHeader.Check(header); err != nil {
			return nil, fmt.Errorf("checksum: %s: %w", dev, err)
		}
		return blocks, nil
	}

	// verifyWrittenDigests is the pre-commit integrity check: it compares
	// the digests the copy collected as it read the source against digests
	// the TARGET computes for itself, and returns an error if they differ.
	//
	// The asymmetry is what makes this affordable. The source digests were
	// free -- CopyExtentsTCP hashed bytes already sitting in its buffers, no
	// extra I/O. The target digests are computed on the target host by
	// vmsync-bridge-helper, so what crosses the network is one short line
	// per chunk rather than the chunk: about 41 KB for a 10 GiB disk, where
	// pulling the data back to hash it here would have roughly doubled a
	// full sync.
	//
	// Three outcomes, deliberately distinct, because they call for opposite
	// responses (the same distinction F3 drew for -verify):
	//
	//   - ErrFormatMismatch: vmsync and the helper are different versions,
	//     or the helper is not deployed. A deployment problem. It must never
	//     read as a corrupt replica, which is exactly what it would if the
	//     exchange were bare digests -- hence the header.
	//   - ErrPlanMismatch: the helper answered about different blocks than
	//     it was asked about. A bug or a truncated transfer, not evidence.
	//   - a non-empty mismatch list: the bytes on the target differ from the
	//     bytes sent. The only one that condemns the replica.
	verifyWrittenDigests := func(i int, d disk.QcowDisk, imagePath string, incremental bool, sourceDigests []blockdigest.Block) error {
		if len(sourceDigests) == 0 {
			// Nothing was written, so there is nothing to check and no
			// reason to start an export. An incremental run that found no
			// dirty extents lands here routinely.
			trace.Info("checksum: nothing written, skipping the pre-commit check", "disk", d.TargetDev)
			return nil
		}

		// A UNIX SOCKET, not a TCP port, and that is the whole reason this
		// check costs no ports at all.
		//
		// This export exists solely to be read by vmsync-bridge-helper
		// running on this same host. It is never reached across the network,
		// so binding it to TCP would spend a port out of the run's
		// reservation -- growing every run's span by N whether or not the
		// check is even enabled -- and would additionally publish an export
		// full of guest data on the network for nobody's benefit. A socket
		// also sidesteps the release race that forced the verify export onto
		// its own block at +2N: sockets are named per disk and per domain, so
		// nothing can collide with a port that was just killed.
		//
		// Keyed by domain and device, like the pidfile, so two runs against
		// different VMs on one target host cannot collide.
		sockPath := path.Join("/tmp", fmt.Sprintf("vmsync-checksum-%s-%s.sock", cfg.TargetDomain, d.TargetDev))
		pidFile := path.Join("/tmp", fmt.Sprintf("vmsync-checksum-qemu-nbd-%s-%s.pid", cfg.TargetDomain, d.TargetDev))
		exportName := targetExportName(cfg.TargetDomain, d.TargetDev)

		// --cache=none is the point of re-exporting at all rather than
		// reading back through the still-open write export. Without it the
		// read is served from qemu's block-layer cache and the host page
		// cache, so it would confirm what qemu believes it wrote and prove
		// nothing about what reached storage -- and "corruption after the
		// write" is precisely the gap nothing else in vmsync covers.
		// O_DIRECT on a cold process is what makes the read come off the
		// device. Deliberately NOT set on the write export: paying O_DIRECT
		// on every byte copied, to benefit a check that reads back only the
		// delta, is the wrong trade.
		//
		// rm -f before starting: qemu-nbd refuses to bind a socket path that
		// already exists, and a previous run killed with -9 leaves one
		// behind. Removing a stale socket is safe in a way removing a stale
		// pidfile is not -- there is no PID to be reused by anything else.
		startCmd := "rm -f " + util.ShQuote(sockPath) + "; " +
			"qemu-nbd --fork --persistent --read-only --cache=none --format=qcow2 --socket " +
			util.ShQuote(sockPath) +
			" --export-name " +
			util.ShQuote(exportName) +
			" --pid-file " +
			util.ShQuote(pidFile) +
			" " +
			util.ShQuote(imagePath)
		if err := runTargetCommand(startCmd, fmt.Sprintf("start read-only checksum export for %s", imagePath)); err != nil {
			return err
		}
		// Same rm -f-after-kill reasoning as the other stop strings: this is
		// replayable from the interrupt-cleanup path after the inline stop
		// below has already run it, and without removing the pidfile that
		// replay could SIGKILL whatever unrelated process the OS has since
		// reused the PID for. The socket goes too -- qemu-nbd unlinks it on
		// a clean exit but not on the kill -9 above.
		stopCmd := "kill -9 $(cat " + util.ShQuote(pidFile) + ") || true; rm -f " + util.ShQuote(pidFile) + " " + util.ShQuote(sockPath)
		stopMu.Lock()
		targetStopCommands = append(targetStopCommands, stopCmd)
		stopMu.Unlock()
		defer func() {
			// context.Background(), never ctx: reportWorkerErr cancels ctx
			// before pushing an error, so on a failing run ctx is already
			// dead by the time this runs and the export would be left
			// holding the image open -- blocking the commit and, on a
			// -reinit, the rm of the disk file.
			if _, err := targetSSHClient.Run(context.Background(), stopCmd); err != nil {
				trace.Warning("checksum: could not stop the read-only checksum export", "disk", d.TargetDev, "error", err)
			}
		}()

		trace.Info("checksum: asking the target to hash what this run wrote",
			"disk", d.TargetDev, "image", imagePath, "blocks", len(sourceDigests),
			"bytes", blockdigest.TotalBytes(sourceDigests), "algo", blockdigest.DefaultAlgo)
		targetBlocks, err := askTargetDigests(d.TargetDev, sockPath, exportName, sourceDigests)
		if err != nil {
			return err
		}

		mismatches, err := blockdigest.Compare(sourceDigests, targetBlocks)
		if err != nil {
			return fmt.Errorf("checksum: %s: %w", d.TargetDev, err)
		}
		if len(mismatches) > 0 {
			// What the operator can conclude differs by mode, so say which.
			// An incremental run's base is still intact and the overlay is
			// simply never committed. A full sync wrote the base directly,
			// so there is nothing to roll back -- the run fails, the replica
			// is not marked synced, and it needs another -reinit.
			remedy := "the replica's base image is untouched -- this run's overlay is removed instead of committed, so the replica still holds its last good contents"
			if !incremental {
				remedy = "this was a full sync writing the base directly, so there is no overlay to discard: the replica is NOT trustworthy and needs another -reinit"
			}
			trace.Error("checksum: the target's contents differ from what was sent",
				"disk", d.TargetDev, "image", imagePath,
				"detail", blockdigest.SummarizeMismatches(mismatches))
			return fmt.Errorf("checksum: %s: the bytes on the target do not match the bytes sent: %s (%s)",
				d.TargetDev, blockdigest.SummarizeMismatches(mismatches), remedy)
		}

		trace.Info("checksum: target contents match what was sent",
			"disk", d.TargetDev, "blocks", len(sourceDigests), "bytes", blockdigest.TotalBytes(sourceDigests))
		return nil
	}

	// recordDiskMetric appends one metrics.DiskMetric, exactly the
	// computation syncDisk's own deferred metrics block always did. Its own
	// closure rather than inline because it once had two callers: the
	// single-phase path and -verify's since-removed two-phase one. Kept
	// factored out -- it is the whole per-disk metric in one place, which is
	// where the source/target bridge attribution above wants to be read.
	recordDiskMetric := func(d disk.QcowDisk, diskSize, writtenBytes uint64, targetBridgeCounters *nbdbridge.ByteCounters, duration time.Duration) {
		if cfg.PrometheusTextfile == "" {
			return
		}
		// The TARGET leg only, and that is the fix rather than an omission.
		//
		// This used to add sourceBridgeCounters too. Two things were wrong
		// with that. The source bridge is created ONCE per run (one shared
		// libvirt backup export, one listener), so its counter is a run-wide
		// monotonic total -- adding it to every disk meant summing the
		// per-disk series multi-counted it, once per disk. And a per-disk
		// delta cannot repair that either: both disk loops fan out one
		// goroutine per disk, so the windows overlap and each disk's delta
		// would absorb whatever the others pushed through the shared bridge
		// meanwhile.
		//
		// The target bridge has neither problem: it is created per disk
		// inside copyAndCommit, so its counter measures exactly this disk.
		// The source leg is a run-level fact and is reported as one, on
		// RunMetric.
		compressedBytes := writtenBytes
		if targetBridgeCounters != nil {
			compressedBytes = targetBridgeCounters.SentSnapshot()
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
	//
	// A struct rather than closure capture because copyAndCommit and
	// runVerify are separate closures: syncDisk calls one then the other and
	// hands the result across. It dates from when the "online" mode ran them in
	// two separate goroutine invocations either side of a whole-run barrier;
	// that barrier is gone, but passing the values explicitly is still
	// clearer than widening what either closure captures.
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

		// The incremental overlay is this run's scratch space, and until it is
		// committed it is worth exactly nothing: every byte in it is also
		// still readable from the source. So remove it on any path out of
		// here that is NOT a successful commit -- a failed copy, a failed
		// export stop, a checksum mismatch, or a cancelled context.
		//
		// One case is deliberately exempt: a FAILED qemu-img commit. Every
		// other failure leaves the base untouched, which is what makes the
		// overlay worthless. A half-finished commit does not -- the base may
		// already be partly written, and the overlay is then the only record
		// of the delta and the only way to retry by hand. So commitAttempted
		// is set before the commit runs, not after it succeeds: the flag
		// means "the base is no longer known-untouched", and from that
		// moment the overlay is evidence rather than scratch.
		//
		// Registered immediately after creation (not before -- there would
		// be nothing to remove) and skipped once commitAttempted is set, so
		// the success path keeps using the existing rm below and pays no
		// extra SSH round trip here.
		//
		// Before this existed, every failing incremental left a delta-sized
		// qcow2 behind on the target. Whether it was ever reclaimed came
		// down to whether the checkpoint chain had advanced: the name is
		// targetPath + "_" + bitmapForRead, so a retry against the same
		// parent checkpoint would recreate and overwrite it, but a retry
		// against a new one writes a different filename and orphans the old
		// file permanently. Persistent failures therefore accumulated them.
		//
		// context.Background(), never ctx: reportWorkerErr cancels ctx
		// before pushing an error, so on precisely the failing runs this
		// exists for, ctx is already dead by the time it runs.
		commitAttempted := false
		if incrementalMode {
			defer func() {
				if commitAttempted {
					return
				}
				trace.Info("Removing uncommitted temporary image", "image", targetPathInc, "disk", d.TargetDev)
				if out, err := targetSSHClient.Run(context.Background(), "rm -f "+util.ShQuote(targetPathInc)); err != nil {
					trace.Warning("could not remove the uncommitted temporary image; it holds this run's delta and nothing else, so it is safe to delete by hand",
						"image", targetPathInc, "disk", d.TargetDev, "error", err, "output", out)
				}
			}()
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
		// --export-name so this export is addressable by identity rather
		// than only by port; see targetExportName.
		exportName := targetExportName(cfg.TargetDomain, d.TargetDev)
		startExportCmd = startExportCmd +
			" --format=qcow2 --bind " +
			util.ShQuote(cfg.TargetNBDBind) +
			" --port " +
			fmt.Sprintf("%d", targetPort) +
			" --export-name " +
			util.ShQuote(exportName) +
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
			bridgeStopCmd, err := nbdbridge.StartRemote(ctx, targetSSHClient, cfg.TargetDomain+"-"+d.TargetDev, targetBridgePort, targetPort, bridgeCfg)
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
			if err := nbdsync.WaitForTCPExport(targetNBDHost, targetPort, exportName, 10*time.Second); err != nil {
				return res, fmt.Errorf("wait for target nbd export %s:%d: %w", targetNBDHost, targetPort, err)
			}
		}

		trace.Info("copy extents to remote target", "extents", len(extents), "path", targetPath, "disk_size", res.diskSize)
		var sourceDigests []blockdigest.Block
		res.writtenBytes, sourceDigests, err = nbdsync.CopyExtentsTCP(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, effectiveTargetHost, effectiveTargetPort, exportName, extents, cfg.IODepth, checksumEnabled)
		if err != nil {
			return res, err
		}

		if res.targetBridgeCounters != nil {
			logicalBytes := nbdbridge.SumLogicalDirtyBytes(extents)
			trace.Info("target nbd bridge compression", "disk", d.TargetDev, "savings", nbdbridge.FormatSavings(logicalBytes, res.targetBridgeCounters.SentSnapshot()))
		}
		if sourceBridgeCounters != nil {
			// ReceivedSnapshot, not Sent: on the source side the payload
			// arrives INBOUND (the disk data being read), while Sent carries
			// the NBD request stream. Comparing dirty bytes against Sent --
			// which this used to do -- divided a disk's worth of data by a
			// few MiB of requests and reported a compression ratio near 100%.
			//
			// Still logged per disk although the counter is run-wide and the
			// disks run concurrently, so on a multi-disk run this line is a
			// running total rather than this disk's share. That is tolerable
			// for a log line and is why the same number is NOT what the
			// per-disk metric reports; see recordDiskMetric.
			logicalBytes := nbdbridge.SumLogicalDirtyBytes(extents)
			trace.Info("source nbd bridge compression (run-wide, all disks)", "disk", d.TargetDev,
				"savings", nbdbridge.FormatSavings(logicalBytes, sourceBridgeCounters.ReceivedSnapshot()))
		}

		trace.Info("Stopping remote daemon", "device", d.TargetDev)
		if err := runTargetCommand(stopCmd, fmt.Sprintf("stop qemu-nbd for %s", targetPath)); err != nil {
			return res, err
		}

		// The pre-commit integrity check. Placed here, after the write
		// export is stopped and before the commit, because that is the one
		// window where both halves of what it needs are true: the bytes are
		// on the target's storage, and in incremental mode the replica's
		// BASE is still untouched -- so a mismatch costs an overlay rather
		// than a replica.
		//
		// The image checked is the overlay on an incremental run and the
		// base itself on a full one. A full sync has no overlay to discard,
		// so a mismatch there cannot be undone; the run still fails loudly,
		// which is the whole value -- the alternative is not knowing.
		if checksumEnabled {
			checkPath := targetPath
			if incrementalMode {
				checkPath = targetPathInc
			}
			if err := verifyWrittenDigests(i, d, checkPath, incrementalMode, sourceDigests); err != nil {
				return res, err
			}
		}

		if incrementalMode {
			trace.Info("Committing changes to base", "image", targetPath)
			commitCmd := "qemu-img commit -b " + util.ShQuote(targetPath) + " " + util.ShQuote(targetPathInc)
			// Set BEFORE the commit runs, not after it succeeds. From here on
			// the base is no longer known-untouched, so the overlay stops
			// being disposable scratch and becomes the only record of this
			// delta -- see the deferred cleanup where it was created. A
			// commit that fails halfway must leave it in place for a manual
			// retry, not have it swept up on the way out.
			commitAttempted = true
			if err := runTargetCommand(commitCmd, fmt.Sprintf("committing changes for %s", targetPathInc)); err != nil {
				return res, fmt.Errorf("%w -- the temporary image %s has been LEFT IN PLACE deliberately: the base may be partly committed, so that overlay is the only remaining record of this run's delta. Inspect both before removing it", err, targetPathInc)
			}
			trace.Info("Removing temporary", "image", targetPathInc)
			if err := runTargetCommand("rm -f "+util.ShQuote(targetPathInc), fmt.Sprintf("removing target image %s", targetPathInc)); err != nil {
				return res, err
			}
		}

		return res, nil
	}

	// runVerify compares one disk, in whichever mode -verify named.
	//
	// It opens a READ-ONLY export on the target's committed base and reads
	// the source through the primary backup job's export -- still open, and
	// frozen at the instant the copy read from. Both sides are therefore the
	// same point in time and must be byte-identical; the mode chooses only
	// how the comparison is performed and how a difference is reported. See
	// the verifyMode constants.
	runVerify := func(i int, d disk.QcowDisk, res diskPhase1Result) (err error) {
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
		verifyExportName := targetExportName(cfg.TargetDomain, d.TargetDev)
		// --cache=none (O_DIRECT) for the same reason the pre-commit checksum
		// export sets it: without it this read is served from the host page
		// cache, which still holds the pages the sync just wrote. That would
		// make -verify confirm what qemu believes it wrote rather than what
		// is actually stored -- so a replica whose on-disk bytes had rotted
		// since the write would pass. A fresh qemu-nbd process has a cold
		// internal cache but the page cache is shared, so bypassing it is
		// the only way the bytes come off the device.
		startVerifyCmd := "qemu-nbd --fork --persistent --read-only --cache=none --format=qcow2 --bind " +
			util.ShQuote(cfg.TargetNBDBind) +
			" --port " +
			fmt.Sprintf("%d", verifyPort) +
			" --export-name " +
			util.ShQuote(verifyExportName) +
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

		if err := nbdsync.WaitForTCPExport(targetNBDHost, verifyPort, verifyExportName, 10*time.Second); err != nil {
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
			stopVerifyBridgeCmd, err = nbdbridge.StartRemote(ctx, targetSSHClient, "verify-"+cfg.TargetDomain+"-"+d.TargetDev, verifyBridgePort, verifyPort, bridgeCfg)
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
		// copy itself already requires. True for all three modes: each reads
		// this same still-open primary export.
		sourceNBDURL := fmt.Sprintf("nbd://%s:%d/%s", effectiveSourceHost, effectiveSourcePort, d.TargetDev)
		// The export NAME belongs in this URL, and leaving it out broke
		// -verify=qemu-img outright.
		//
		// The read-only verify export is started with --export-name (see
		// startVerifyCmd), so a client asking for the default, unnamed export
		// is refused by the NBD handshake. qemu-img compare then exits 2 --
		// "could not open image" -- in a couple of seconds, having compared
		// nothing.
		//
		// Every other comparator here takes verifyExportName as a parameter;
		// this is the only one that formats its own URL, which is exactly why
		// naming the exports (F10) missed it. What kept it hidden is that
		// vmsync_verification_state mirrors the run state rather than saying
		// WHY a verify run failed, so bench's tamper tests read "the run
		// failed" as "a mismatch was found" and scored a comparator that
		// could not connect as a pass. Only the clean-oracle sub-test, which
		// is the one that expects a CLEAN result, could catch it.
		nbdURL := fmt.Sprintf("nbd://%s:%d/%s", verifyTargetHost, verifyTargetPort, verifyExportName)

		// Every mode compares the target against the SAME source export the
		// copy read from -- the primary backup job, still running. That job
		// is a frozen point-in-time view, so both sides are source@T0 and
		// the two must be byte-identical. There is no drift to excuse and no
		// dirty-bitmap reconciliation anywhere in this function.
		//
		// The mode now called "full" used to do something else, under the
		// name "online": it stopped the primary job, started a SECOND one,
		// and so compared source@T2 against target@T0, then tried to excuse
		// the difference using a bitmap.
		// Every guest write during the copy showed up as a mismatch, and on
		// a busy guest that was a near-100% false-positive rate. The
		// exoneration logic existed only to paper over the wrong comparison;
		// removing the second job removed the need for it entirely.
		//
		// What distinguishes online from compare/fast is now ONLY that it
		// does not suspend the source. It never needed to: see verifySuspends
		// above, whose stated reason (a running domain's disk is not static)
		// is about the domain, while every mode reads the frozen export.
		trace.Info("verify: comparing source and target images", "disk", d.TargetDev,
			"source", sourceNBDURL, "target", targetPath, "mode", cfg.Verify)
		var compareErr error
		switch {
		case checksumEnabled && (verifyFull || verifyFast):
			// The digest path. vmsync hashes the source; the helper hashes
			// the same ranges on the target host; only digests cross the
			// wire. What that removes is the dominant cost of a verify: on
			// the common topology (vmsync on the source, so that read is
			// local) a byte compare's whole expense is pulling the target's
			// image over the network, and this replaces it with a few bytes
			// per megabyte.
			//
			// -verify=qemu-img is deliberately excluded. It is the
			// independent oracle -- a separate implementation reading every
			// byte -- and re-expressing it through vmsync's own digest code
			// would make it agree with vmsync by construction, which is
			// precisely the property that makes it worth having.
			//
			// fast and full converge here: the helper computes every digest
			// in one pass, so fast loses its stop-at-first-difference
			// early-out. That costs nothing on a healthy replica (there is
			// no difference to stop at, so it read everything anyway) and
			// only makes an already-failing verify slower. The two still
			// differ in what they report.
			// The plan first, on its own, because it is metadata only and
			// both sides need it before either can start. Settling it up
			// front is what lets the two hashing passes run AT THE SAME
			// TIME: a digest verify then costs max(source, target) instead
			// of their sum, which is what the byte comparator it replaces
			// always did by reading both sides in one pipeline.
			plan, perr := nbdsync.CompareChunkPlanTCP(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, verifyTargetHost, verifyTargetPort, verifyExportName)
			switch {
			case perr != nil:
				compareErr = fmt.Errorf("planning the comparison failed: %w", perr)
			case len(plan) == 0:
				// Every range read as zeros on both sides, so there is
				// nothing left that could differ. Not a skipped check: the
				// allocation maps already answered it.
				trace.Info("verify: nothing to hash -- every range reads as zeros on both sides", "disk", d.TargetDev)
			default:
				trace.Info("verify: hashing both sides in parallel",
					"disk", d.TargetDev, "blocks", len(plan),
					"bytes", blockdigest.TotalBytes(plan), "algo", blockdigest.DefaultAlgo)

				// Both passes touch only their own side -- vmsync reads the
				// source export, the helper reads the target's -- so they
				// cannot contend for the single client slot qemu-nbd allows
				// by default. CompareChunkPlanTCP's own target connection is
				// already closed by the time it returns, which is what makes
				// that true.
				var (
					wg            sync.WaitGroup
					sourceDigests []blockdigest.Block
					targetBlocks  []blockdigest.Block
					sourceErr     error
					targetErr     error
				)
				wg.Add(2)
				go func() {
					defer wg.Done()
					sourceDigests, sourceErr = nbdsync.HashRangesTCP(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, plan, cfg.IODepth)
				}()
				go func() {
					defer wg.Done()
					// The EXISTING verify export at verifyPort -- the block
					// at +2N that -verify already reserves. The digest path
					// adds no port of its own: vmsync needs that export on
					// TCP anyway (it read base:allocation through it above,
					// possibly bridged from another host), and the helper
					// reaches the same export over loopback.
					targetBlocks, targetErr = askTargetDigests(d.TargetDev, fmt.Sprintf("127.0.0.1:%d", verifyPort), verifyExportName, plan)
				}()
				wg.Wait()

				// Source error first, deliberately. If the source read
				// failed there is nothing to compare against, and reporting
				// the target's error instead would point at the replica for
				// a problem on the other side.
				if sourceErr != nil {
					compareErr = fmt.Errorf("hashing the source failed: %w", sourceErr)
					break
				}
				if targetErr != nil {
					// A format or plan problem, or a helper that would not
					// run. NOT wrapped in ErrImagesDiffer: the comparison
					// could not be performed, which is a broken sync rather
					// than a finding about the data, and isVerifyMismatch
					// must not exempt it from failure_count.
					compareErr = targetErr
					break
				}
				mismatches, cerr := blockdigest.Compare(sourceDigests, targetBlocks)
				switch {
				case cerr != nil:
					compareErr = cerr
				case len(mismatches) > 0:
					var diffBytes uint64
					for _, m := range mismatches {
						diffBytes += m.Length
					}
					if verifyFull {
						// full's remit is HOW broken, so name every block.
						for _, m := range mismatches {
							trace.Error("verify: block differs", "disk", d.TargetDev,
								"offset", m.Offset, "length", m.Length,
								"source_digest", fmt.Sprintf("%#x", m.Want),
								"target_digest", fmt.Sprintf("%#x", m.Got))
						}
					}
					// Wrapped with the same sentinel the byte comparators
					// use, so isVerifyMismatch treats all of them alike:
					// this is a finding about the data, not a failure to
					// look at it.
					compareErr = fmt.Errorf("%w: %d block(s) totalling %d bytes differ from the source snapshot this replica was copied from (%s) -- both sides are the same point in time, so this is a real difference and not concurrent guest activity",
						nbdsync.ErrImagesDiffer, len(mismatches), diffBytes, blockdigest.SummarizeMismatches(mismatches))
				}
			}
		case verifyFull:
			// Collect rather than abort on the first difference. Any
			// mismatch is real, so the useful question is no longer whether
			// the replica is broken but HOW broken. Costs a full scan on an
			// already-failing disk, which is exactly the trade this mode
			// exists to make -- fast is there for when it is not wanted.
			mismatches, cerr := nbdsync.CompareTCPCollect(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, verifyTargetHost, verifyTargetPort, verifyExportName, cfg.IODepth)
			switch {
			case cerr != nil:
				compareErr = fmt.Errorf("compare failed: %w", cerr)
			case len(mismatches) > 0:
				var diffBytes uint64
				for _, m := range mismatches {
					diffBytes += m.Length
				}
				// Wrapped with the same sentinel the other two comparators
				// use, so isVerifyMismatch treats all three alike: this is a
				// finding about the data, not a failure to look at it.
				compareErr = fmt.Errorf("%w: %d range(s) totalling %d bytes differ from the source snapshot this replica was copied from -- both sides are the same point in time, so this is a real difference and not concurrent guest activity",
					nbdsync.ErrImagesDiffer, len(mismatches), diffBytes)
			}
		case verifyFast:
			compareErr = nbdsync.CompareTCP(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, verifyTargetHost, verifyTargetPort, verifyExportName, cfg.IODepth)
		default:
			compareErr = disk.CompareImages(sourceNBDURL, nbdURL)
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
	// goroutine per disk with zero cross-disk coordination -- now the only
	// path, for every mode. The mode now called "full" used to take a
	// two-phase one with a barrier and a second backup job; comparing
	// against the export the copy actually read from removed the need for
	// both.
	// Decided before any disk is copied, so a target that cannot deliver what
	// -retention promises fails the run here rather than after paying for a
	// full copy. Returns an inert value when retention is off or the interval
	// has not elapsed, so nothing below needs a conditional of its own.
	//
	// Declared ahead of the closures rather than beside the loop that uses
	// it, because syncDisk below captures it.
	// One loop, two consumers: the restore-point paths and the dev-to-path
	// pairing replica_written_at is keyed by. Deriving the path twice is how
	// a stamp ends up recorded against a file the preflight never stats.
	targetDiskPaths := make([]string, 0, len(qcowDisks))
	stamps := make([]stampDisk, 0, len(qcowDisks))
	for _, d := range qcowDisks {
		p := util.SetTargetPath(cfg.TargetDiskPath, d.RootSource)
		targetDiskPaths = append(targetDiskPaths, p)
		stamps = append(stamps, stampDisk{dev: d.TargetDev, path: p})
	}
	stampMu.Lock()
	stampDisks = stamps
	stampMu.Unlock()
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
			return runVerify(i, d, res)
		}
		trace.Info("disk sync complete", "disk", d.TargetDev, "elapsed", time.Since(diskStart).Round(time.Millisecond).String())
		return nil
	}

	// ONE path for every mode.
	//
	// There used to be a second, two-phase path for the mode then called
	// "online": a barrier across all disks, then a domain-wide "compare
	// window" that stopped the primary backup job and started another. All
	// of it existed to serve a comparison
	// against the wrong point in time. Comparing against the primary export
	// -- which is still open and still frozen at the instant the copy read
	// from -- needs no barrier, no second job and no cross-disk coordination,
	// so each disk copies and verifies in its own goroutine exactly as the
	// suspend-based modes always have.
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

	// Record what this run wrote to the replica -- BEFORE the drain below,
	// which returns on the first worker error.
	//
	// That ordering is the entire fix. last_sync_timestamp is written only
	// by UpdateSyncMetadata, far below and only on full success, so a run
	// that copied the disks and then failed -- a failed -verify, most often
	// -- left every replica disk with a fresh mtime and the recorded
	// timestamp untouched. The next run's preflight then saw a disk newer
	// than the last sync and refused, blaming an out-of-band writer that did
	// not exist, and refused again every run after that because each refusal
	// happens before the copy that would have moved the timestamp on.
	//
	// cleanupTargetNBD first, and not for tidiness: on a FULL sync qemu-nbd
	// exports the base file itself, and on a failed copy copyAndCommit's own
	// inline stop was never reached, so a live daemon could still be
	// completing queued writes into the very file about to be stat'd. It is
	// idempotent (targetCleanupOnce), so the deferred call further up simply
	// becomes a no-op and this costs nothing. Nothing past this point
	// registers another target export or writes a replica disk -- rp.commit
	// renames a staging directory and writes a sidecar, DefineDomain touches
	// XML only -- so this is the last moment a replica disk can change.
	cleanupTargetNBD("stamp")
	replicaWrittenAt := measureReplicaWrittenAt("post-copy")
	recordReplicaWrittenAt("post-copy", replicaWrittenAt)

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
		// -force-clean undefined the target domain itself, so the role it
		// used to carry went with it and this reads "" -- a change this run
		// caused on purpose, not the concurrent failover the check is looking
		// for. Reported as what it is, rather than as a race that did not
		// happen. The redefine below then writes no role at all, which is
		// what a replica rebuilt from nothing should look like: the next sync
		// treats an absent role as permission to proceed.
		if cfg.ForceClean && currentTargetRole == "" {
			trace.Info("-force-clean removed the target domain, so its previous replication role is gone; it is being redefined without one",
				"vm", cfg.TargetDomain, "previous_role", targetRole)
		} else {
			trace.Warning("target replication role changed during this sync but still permits it",
				"vm", cfg.TargetDomain, "from", targetRole, "to", currentTargetRole)
		}
	}

	trace.Info("Adding metadata information")
	var newXML string
	newXML, err = libvirtsync.UpdateSyncMetadata(srcXML, effectiveCheckpoint, util.ReplicaHost(cfg.SourceURI, cfg.LocalHostName), cfg.SourceDomain, currentTargetRole, checkpointAt.Unix(), sourceStoppedAtCheckpoint, replicaWrittenAt)
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
