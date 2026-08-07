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
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path"
	"sync"
	"syscall"
	"time"

	"vmsync/pkg/disk"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/metrics"
	"vmsync/pkg/nbdbridge"
	"vmsync/pkg/nbdsync"
	"vmsync/pkg/remotessh"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"
	"vmsync/pkg/version"

	"libvirt.org/go/libvirt"
)

func main() {
	if os.Getenv("PROFILE") == "development" {
		host := "localhost:6060"
		trace.Info("Enabling pprof for profiling", "address", host)
		go func() {
			log.Println(http.ListenAndServe(host, nil))
		}()
	}

	var cfg struct {
		SourceURI      string
		TargetURI      string
		SourceDomain   string
		TargetDomain   string
		TargetDiskPath string
		SourceNBDHost  string
		SourceNBDPort  int
		SourceNBDBind  string
		TargetNBDHost  string
		TargetNBDPort  int
		TargetNBDBind  string
		SSHUser        string
		SSHKey         string
		SSHPassword    string
		SSHPort        int
		SSHInsecure    bool
		KnownHosts     string
		SSHTimeoutSec  int
		Debug               bool
		Start               bool
		Reinit              bool
		ReinitAfterFailures int
		Compress            bool
		CompressLevel       string
		CompressAlgo        string
		NetBuffer           string
		BridgeHelperPath    string
		UseSSH              bool
		IODepth                int
		PrometheusTextfile     string
		IgnoreExternalSnapshot bool
		Verify                 bool
		VerifyFast             bool
		VerifyOnline           bool
		ShowVersion            bool
	}

	flag.StringVar(&cfg.SourceURI, "source-uri", "", "libvirt source URI (example: qemu+ssh://src/system)")
	flag.StringVar(&cfg.TargetURI, "target-uri", "", "libvirt target URI (example: qemu+ssh://target/system)")
	flag.StringVar(&cfg.SourceDomain, "source-domain", "", "source domain name")
	flag.StringVar(&cfg.TargetDomain, "target-domain", "", "target domain name (defaults to --source-domain)")
	flag.StringVar(&cfg.TargetDiskPath, "target-disk-path", "", "target disk path for changed location")
	flag.StringVar(&cfg.SourceNBDBind, "source-nbd-bind", "0.0.0.0", "source bind address for libvirt backup NBD TCP export")
	flag.IntVar(&cfg.SourceNBDPort, "source-nbd-port", 10809, "source TCP port for libvirt backup NBD export")
	flag.StringVar(&cfg.SourceNBDHost, "source-nbd-host", "", "source host to connect for NBD reads (defaults from --source-uri)")
	flag.StringVar(&cfg.TargetNBDBind, "target-nbd-bind", "0.0.0.0", "target bind address for qemu-nbd TCP export")
	flag.IntVar(&cfg.TargetNBDPort, "target-nbd-port", 20809, "target base TCP port for qemu-nbd exports")
	flag.StringVar(&cfg.TargetNBDHost, "target-nbd-host", "", "target host to connect for NBD writes (defaults from --target-uri)")
	flag.StringVar(&cfg.SSHUser, "ssh-user", "", "ssh user for remote command execution (defaults from URI user, then ~/.ssh/config's User, then root)")
	flag.StringVar(&cfg.SSHKey, "ssh-key", "", "private key path for ssh authentication (defaults from ~/.ssh/config's IdentityFile)")
	flag.StringVar(&cfg.SSHPassword, "ssh-password", "", "password for ssh authentication")
	flag.IntVar(&cfg.SSHPort, "ssh-port", 0, "ssh port for remote command execution (0 = use ~/.ssh/config's Port, falling back to 22)")
	flag.BoolVar(&cfg.SSHInsecure, "ssh-insecure-host-key", false, "disable host key verification (not recommended)")
	flag.StringVar(&cfg.KnownHosts, "ssh-known-hosts", "", "known_hosts file path (defaults to ~/.ssh/known_hosts)")
	flag.IntVar(&cfg.SSHTimeoutSec, "ssh-timeout-sec", 10, "ssh connection timeout in seconds")
	flag.BoolVar(&cfg.Start, "start", false, "In case vm is in non-running state, start in paused mode to allow sync.")
	flag.BoolVar(&cfg.Reinit, "reinit", false, "Discard all vmsync checkpoints on the source and the existing target domain/disks, then perform a fresh full sync. Use to recover from a broken checkpoint chain (e.g. \"Bitmap already exists\" errors).")
	flag.IntVar(&cfg.ReinitAfterFailures, "reinit-after-failures", 0, "After this many consecutive sync failures (tracked in the target domain's vmsync metadata), automatically reinit (as with -reinit) instead of trying again the same way. 0 disables this (default).")
	flag.BoolVar(&cfg.Compress, "compress", false, "Compress NBD traffic between hosts using zstd. Compression runs natively on both ends (no external tool dependency); the remote side requires vmsync-bridge-helper deployed at -bridge-helper-path. By default the bridged traffic goes directly between hosts, not through SSH -- see -use-ssh. Core sync behavior is unchanged when this is not set.")
	flag.StringVar(&cfg.CompressLevel, "compress-level", "3", "Compression level/mode to use when --compress is set. For --compress-algo=zstd (default): a number 1-19 (default 3). For --compress-algo=s2 (which has no numeric levels): one of \"default\" (fastest, s2's own default), \"better\", or \"best\" -- if --compress-level isn't set explicitly, s2 automatically uses \"default\" instead of zstd's \"3\".")
	flag.StringVar(&cfg.CompressAlgo, "compress-algo", "zstd", "Compression format to use with --compress: \"zstd\" (better ratio) or \"s2\" (faster, lower ratio -- better when compression speed, not network bandwidth, is the bottleneck). See --compress-level for the accepted level/mode values for each.")
	flag.StringVar(&cfg.NetBuffer, "netbuffer", "", "Buffer NBD bridge traffic through a bounded in-memory buffer to smooth throughput, formatted as <blocksize>,<buffersize> (e.g. 64k,512M). Runs natively on both ends (no external tool dependency). Independent of --compress -- usable alone or combined with it.")
	flag.StringVar(&cfg.BridgeHelperPath, "bridge-helper-path", "/usr/local/bin/vmsync-bridge-helper", "Remote path to the vmsync-bridge-helper binary, used when --compress/--netbuffer is set. Must already be deployed there by you (e.g. via scp) -- vmsync does not upload it.")
	flag.BoolVar(&cfg.UseSSH, "use-ssh", false, "When --compress/--netbuffer is set, route the bridged NBD traffic through the existing SSH connection as an encrypted tunnel, instead of the default: vmsync-bridge-helper listening on all interfaces and the local relay connecting to it directly over plain TCP. The default (false) has NO encryption or authentication of its own for that traffic -- only appropriate when the network path between the hosts is already secured some other way (e.g. a VPN/WireGuard tunnel). When false, requires the bridge port range to be reachable directly between the two hosts (firewall/routing) -- vmsync does not verify this itself. No effect without --compress/--netbuffer.")
	flag.IntVar(&cfg.IODepth, "io-depth", 8, "Number of NBD read/write pairs to keep in flight simultaneously during the disk copy, instead of waiting for each to fully complete before starting the next. Higher values can hide more per-chunk round-trip latency (real disk I/O plus NBD protocol overhead on both ends), at the cost of io-depth times the negotiated NBD block size in memory. Must be at least 1.")
	flag.StringVar(&cfg.PrometheusTextfile, "prometheus-textfile", "", "Write sync metrics to this path in Prometheus textfile-collector format: per disk, source/target host, vm, disk size, transferred and compressed-transferred bytes, and duration; plus one overall success/failure state for the whole run. Written atomically (temp file + rename). Empty disables it (default).")
	flag.BoolVar(&cfg.IgnoreExternalSnapshot, "ignore-external-snapshot", false, "If the source domain currently has any external disk snapshot, skip this run entirely -- no sync attempt, no -prometheus-textfile write, same clean no-op as losing the per-domain run lock. Default (false): sync anyway, incrementally against the existing checkpoint, since libvirt blocks creating a new one while a snapshot exists (see vmsync_external_snapshot_count for observability of that case instead).")
	flag.BoolVar(&cfg.Verify, "verify", false, "After syncing, suspend the source VM (if running), run a full qemu-img compare against the target for every disk, then resume it. This holds the VM suspended for meaningfully longer than a normal sync -- the whole compare, not just the copy -- so this is meant for periodic verification on its own schedule, not routine syncing. Fails the run if any disk doesn't match. The compare reads both sides over their own NBD exports (same as the disk copy itself), from wherever vmsync itself runs -- no additional reachability beyond what a plain sync without --compress/--netbuffer already requires. If --compress/--netbuffer are set, the target-side read of the compare automatically goes through its own vmsync-bridge-helper instance too (a full-image read benefits from this at least as much as the copy does), tunneled via -use-ssh under the same conditions the regular copy's bridge is. See -verify-fast for actually getting a speedup out of that bridge.")
	flag.BoolVar(&cfg.VerifyFast, "verify-fast", false, "When -verify is set, compare source and target using vmsync's own pipelined NBD reader (same -io-depth concurrency as the disk copy) instead of shelling out to qemu-img compare. qemu-img compare reads one 2MB chunk at a time, synchronously, on both images before advancing -- round-trip-latency-bound, not bandwidth-bound, so --compress/--netbuffer/--use-ssh give it no speedup. -verify-fast fixes that, which is what actually lets --compress/--netbuffer speed up the compare (and --use-ssh encrypt it) the same way they already do for the regular copy. Trade-off: unlike qemu-img compare, this always reads the full image on both sides -- it does not skip regions unallocated on both source and target, so it may transfer more data than qemu-img compare on a very sparse/thin-provisioned image despite completing faster overall on typical (mostly-allocated) disks. No effect without -verify.")
	flag.BoolVar(&cfg.VerifyOnline, "verify-online", false, "Like -verify, but without suspending the source VM: creates a short-lived checkpoint when the compare begins, runs the full compare against the live disk (always via vmsync's own pipelined NBD reader, same as -verify-fast -- qemu-img compare has no equivalent here), then cross-references any mismatch against what the checkpoint's own bitmap shows the guest wrote during the compare -- a mismatch inside a touched region is discarded as inconclusive (the guest changed it after target's last sync, not corruption), only a mismatch outside every touched region fails the run. Trade-off versus -verify: never causes downtime, but a very write-heavy guest during a slow compare can leave large regions unverified this round rather than confirmed either way. Mutually exclusive with -verify.")
	flag.BoolVar(&cfg.Debug, "debug", false, "Enable debug logging")
	flag.BoolVar(&cfg.ShowVersion, "v", false, "Show version and exit")
	flag.BoolVar(&cfg.ShowVersion, "version", false, "Show version and exit")
	flag.Parse()

	// Tracks whether --compress-level was actually passed on the command
	// line, as opposed to just carrying its zstd-oriented flag default
	// ("3") -- needed below to swap in s2's own default ("default") instead
	// when --compress-algo=s2 and the user didn't ask for a specific level.
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
	if cfg.SourceURI == "" || cfg.TargetURI == "" || cfg.SourceDomain == "" {
		flag.Usage()
		os.Exit(2)
	}
	if cfg.TargetDomain == "" {
		cfg.TargetDomain = cfg.SourceDomain
	}
	if cfg.Compress {
		if err := nbdbridge.ValidateCompressAlgo(cfg.CompressAlgo); err != nil {
			trace.Error("invalid compress algo configuration", "error", err)
			os.Exit(2)
		}
		if !compressLevelExplicit && cfg.CompressAlgo == "s2" {
			cfg.CompressLevel = "default"
		}
		if err := nbdbridge.ValidateCompressLevel(cfg.CompressAlgo, cfg.CompressLevel); err != nil {
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
	if cfg.Verify && cfg.VerifyOnline {
		trace.Error("invalid verify configuration", "error", fmt.Errorf("-verify and -verify-online are mutually exclusive -- -verify suspends the source VM, -verify-online explicitly must not"))
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
	// before backing off. Not a sync failure: exits 0, doesn't count toward
	// -reinit-after-failures, and (since run() is never called) never
	// touches -prometheus-textfile either -- it's a clean no-op skip, the
	// same way the wrapper script's own lock already treats "another
	// instance is already running".
	lockFile, err := util.AcquireRunLock("/run/vmsync-locks", cfg.SourceDomain)
	if err != nil {
		trace.Info("another vmsync is already syncing this domain, skipping", "domain", cfg.SourceDomain, "error", err)
		os.Exit(0)
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
	if cfg.ReinitAfterFailures > 0 {
		failures, err := libvirtsync.ReadTargetFailureCount(cfg.TargetURI, cfg.TargetDomain)
		if err != nil {
			trace.Warning("unable to read failure count from target metadata", "error", err)
		} else if failures >= cfg.ReinitAfterFailures {
			trace.Warning("reinit-after-failures threshold reached, forcing reinit", "consecutive_failures", failures, "threshold", cfg.ReinitAfterFailures)
			cfg.Reinit = true
		}
	}

	if err := run(cfg); err != nil {
		trace.Error("sync failed", "error", err)
		if cfg.ReinitAfterFailures > 0 {
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
// -- used by -verify-online to tell a real mismatch (outside anything the
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

func run(cfg struct {
	SourceURI      string
	TargetURI      string
	SourceDomain   string
	TargetDomain   string
	TargetDiskPath string
	SourceNBDHost  string
	SourceNBDPort  int
	SourceNBDBind  string
	TargetNBDHost  string
	TargetNBDPort  int
	TargetNBDBind  string
	SSHUser        string
	SSHKey         string
	SSHPassword    string
	SSHPort        int
	SSHInsecure    bool
	KnownHosts     string
	SSHTimeoutSec       int
	Debug               bool
	Start               bool
	Reinit              bool
	ReinitAfterFailures int
	Compress            bool
	CompressLevel       string
	CompressAlgo        string
	NetBuffer           string
	BridgeHelperPath    string
	UseSSH              bool
	IODepth             int
	PrometheusTextfile  string
	IgnoreExternalSnapshot bool
	Verify              bool
	VerifyFast          bool
	VerifyOnline        bool
	ShowVersion         bool
}) (runErr error) {
	runStart := time.Now()
	defer func() {
		trace.Info("vmsync run finished", "elapsed", time.Since(runStart).Round(time.Millisecond).String(), "success", runErr == nil)
	}()

	var tgtState bool
	var srcState bool
	var targetSSHClient *remotessh.Client
	var sourceSSHClient *remotessh.Client
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
	// meaningful when -verify-online actually reaches that step.
	var verifyWindowMu sync.Mutex
	var verifyWindowActive bool
	var verifyWindowOnce sync.Once
	var stopMu sync.Mutex
	targetStopCommands := make([]string, 0)
	sourceStopCommands := make([]string, 0)
	// checkpointMu guards checkpointName/checkpointAdvanced specifically
	// against the signal handler's own goroutine (started further down),
	// which reads both to decide whether there's a checkpoint to clean up
	// on Ctrl+C/SIGTERM -- every other read/write of these two happens
	// sequentially within run()'s own goroutine (checkpoint creation runs
	// entirely before the per-disk goroutines are spawned, and the
	// end-of-run reads happen after wg.Wait() has already joined them), so
	// only the two writes below and the signal handler's read actually need
	// it. Without this, a signal landing between CreateCheckpoint
	// succeeding and checkpointAdvanced being set could make the handler
	// observe a stale "not advanced", skip deleting a checkpoint that
	// genuinely exists now, and leave it orphaned for the next run to trip
	// over.
	var checkpointMu sync.Mutex
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
	var freezed bool = false
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
			VerificationRan:       (cfg.Verify || cfg.VerifyOnline) && attempted,
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
		state := metrics.StateSuccess
		if runErr != nil {
			state = metrics.StateFailure
		} else if fsFreezeFailed {
			// A failed run (above) always takes priority over this -- a
			// degraded-but-completed freeze is meaningfully less severe than
			// the sync not having completed at all.
			state = metrics.StateFSFreezeFailed
		}
		writeMetricsTextfile(state)
	}()

	netbufferBlock, netbufferSize, err := nbdbridge.ParseNetBufferSpec(cfg.NetBuffer)
	if err != nil {
		return err
	}
	bridgeCfg := nbdbridge.Config{
		Compress:       cfg.Compress,
		CompressLevel:  cfg.CompressLevel,
		CompressAlgo:   cfg.CompressAlgo,
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

	srcDom, err := srcMgr.LookupDomain(cfg.SourceDomain)
	if err != nil {
		return err
	}
	defer srcDom.Free()

	if srcState, err = libvirtsync.DomainActive(srcDom); err != nil {
		return err
	}

	// Unconditional, regardless of whether -verify-online is requested this
	// run: self-heals a verify-window checkpoint left behind by a prior
	// -verify-online invocation that crashed (e.g. SIGKILL) before its own
	// cleanup ran. Cheap (one lookup, delete only if found), and safe to run
	// even when -verify-online was never used -- AcquireRunLock already
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
	if cfg.Verify {
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
			if err := srcDom.Resume(); err != nil {
				trace.Error("failed to resume source VM after -verify", "trigger", trigger, "error", err)
			} else {
				trace.Info("verify: resumed source VM", "trigger", trigger, "vm", cfg.SourceDomain)
			}
		})
	}
	defer resumeSource("cleanup")

	// callWithTimeout runs a blocking libvirt/cgo call in its own goroutine
	// and gives up waiting for it after timeout. libvirt calls have no
	// built-in cancellation, so a genuinely stuck call still runs to
	// completion in the background (and its goroutine/OS thread with it) --
	// but the *caller* (the signal handler) is no longer blocked by it, so
	// the rest of cleanup can still proceed instead of requiring a SIGKILL.
	callWithTimeout := func(name string, timeout time.Duration, fn func() error) error {
		done := make(chan error, 1)
		go func() {
			done <- fn()
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(timeout):
			return fmt.Errorf("%s timed out after %s", name, timeout)
		}
	}

	abortBackup := func(trigger string) {
		abortOnce.Do(func() {
			backupMu.Lock()
			if !backupActive {
				backupMu.Unlock()
				return
			}
			backupActive = false
			backupMu.Unlock()
			trace.Info("stopping libvirt backup job", "trigger", trigger)
			stopErr := callWithTimeout("abort backup job", 5*time.Second, func() error {
				return libvirtsync.StopBackup(srcDom)
			})
			if stopErr != nil {
				trace.Error("stop backup job failed on primary connection", "trigger", trigger, "error", stopErr)
				if retryErr := libvirtsync.StopBackupViaReconnect(cfg.SourceURI, cfg.SourceDomain); retryErr != nil {
					trace.Error("stop backup retry via reconnect also failed", "trigger", trigger, "error", retryErr)
				}
			}
			if started {
				trace.Info("destroying vm as it was started by sync process")
				if destroyErr := callWithTimeout("destroy vm", 5*time.Second, func() error {
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
	// verifyWindowActive was never set (i.e. -verify-online never reached
	// that step this run, including whenever it's not requested at all).
	cleanupVerifyWindow := func(trigger string) {
		verifyWindowOnce.Do(func() {
			verifyWindowMu.Lock()
			active := verifyWindowActive
			verifyWindowMu.Unlock()
			if !active {
				return
			}
			trace.Info("removing verify-online window checkpoint", "trigger", trigger)
			stopErr := callWithTimeout("stop verify-window backup job", 5*time.Second, func() error {
				return libvirtsync.StopBackup(srcDom)
			})
			if stopErr != nil {
				trace.Error("failed to stop verify-window backup job", "trigger", trigger, "error", stopErr)
			}
			delErr := callWithTimeout("delete verify-window checkpoint", 5*time.Second, func() error {
				return libvirtsync.DeleteVerifyWindowCheckpoint(srcDom)
			})
			if delErr != nil {
				trace.Error("failed to delete verify-window checkpoint on primary connection", "trigger", trigger, "error", delErr)
				if retryErr := libvirtsync.DeleteCheckpointViaReconnect(cfg.SourceURI, cfg.SourceDomain, libvirtsync.VerifyWindowCheckpointName); retryErr != nil {
					trace.Error("failed to delete verify-window checkpoint via reconnect", "trigger", trigger, "error", retryErr)
				}
			}
		})
	}
	// Registered right away: cleanupVerifyWindow itself checks
	// verifyWindowActive at the time it actually runs, so this is a no-op
	// for every run that never reaches (or doesn't use) -verify-online's
	// beginVerifyWindow step further down, and the real backstop for one
	// that does but fails or gets interrupted partway through.
	defer cleanupVerifyWindow("cleanup")

	cleanupTargetNBD := func(trigger string) {
		targetCleanupOnce.Do(func() {
			stopMu.Lock()
			stopCommands := append([]string(nil), targetStopCommands...)
			stopMu.Unlock()
			if targetSSHClient == nil || len(stopCommands) == 0 {
				return
			}
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer ccancel()
		for i := len(stopCommands) - 1; i >= 0; i-- {
				if out, err := targetSSHClient.Run(cctx, stopCommands[i]); err != nil {
					trace.Error("failed to stop target qemu-nbd export", "trigger", trigger, "error", err, "output", out)
				}
			}
		})
	}
	cleanupSourceBridge := func(trigger string) {
		sourceCleanupOnce.Do(func() {
			stopMu.Lock()
			stopCommands := append([]string(nil), sourceStopCommands...)
			stopMu.Unlock()
			if sourceSSHClient == nil || len(stopCommands) == 0 {
				return
			}
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer ccancel()
			for i := len(stopCommands) - 1; i >= 0; i-- {
				if out, err := sourceSSHClient.Run(cctx, stopCommands[i]); err != nil {
					trace.Error("failed to stop source nbd bridge", "trigger", trigger, "error", err, "output", out)
				}
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
			// These touch independent connections (source libvirt, target
			// SSH) with no dependency on each other, so run them
			// concurrently -- worst-case wait is the slowest ONE of them,
			// not their sum, before the process can actually exit.
			var cleanupWg sync.WaitGroup
			cleanupWg.Add(6)
			go func() { defer cleanupWg.Done(); abortBackup(sig.String()) }()
			go func() { defer cleanupWg.Done(); cleanupVerifyWindow(sig.String()) }()
			go func() { defer cleanupWg.Done(); cleanupTargetNBD(sig.String()) }()
			go func() { defer cleanupWg.Done(); cleanupSourceBridge(sig.String()) }()
			go func() { defer cleanupWg.Done(); resumeSource(sig.String()) }()
			go func() {
				defer cleanupWg.Done()
				// Snapshot both under checkpointMu, once, rather than
				// reading each separately -- a signal landing between
				// CreateCheckpoint succeeding and checkpointAdvanced being
				// set must see either the fully-pre-checkpoint state or the
				// fully-post-checkpoint state, never a stale mix of the two.
				checkpointMu.Lock()
				name, advanced := checkpointName, checkpointAdvanced
				checkpointMu.Unlock()
				// Mirrors the deferred checkpoint cleanup further down in
				// run() -- duplicated here because that defer never gets a
				// chance to run if we os.Exit below without waiting for it.
				if name == "" || !advanced {
					return
				}
				if err := callWithTimeout("delete checkpoint", 5*time.Second, func() error {
					return libvirtsync.DeleteCheckpointIfExists(srcDom, name)
				}); err != nil {
					trace.Error("failed to delete checkpoint after interrupt", "checkpoint", name, "error", err)
					if retryErr := libvirtsync.DeleteCheckpointViaReconnect(cfg.SourceURI, cfg.SourceDomain, name); retryErr != nil {
						trace.Error("failed to delete checkpoint via reconnect after interrupt", "checkpoint", name, "error", retryErr)
					}
				} else {
					trace.Info("removed checkpoint after interrupt", "checkpoint", name)
				}
			}()
			cleanupWg.Wait()
			cancel()

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
		sourceSSHClient, err = remotessh.Dial(sourceSSHConfig)
		if err != nil {
			return fmt.Errorf("connect ssh for source qemu-img execution: %w", err)
		}
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
		// is no longer that stable file once an external snapshot exists
		// (virsh snapshot-create --disk-only redirects the domain to a new
		// overlay named after the snapshot). Target-side paths are named
		// after this base, not d.Source, so they keep matching the real
		// target file that earlier (pre-snapshot) syncs already created
		// under the original name.
		rootPath := chain[len(chain)-1].Filename
		if rootPath != d.Source {
			trace.Info("resolved disk's backing chain to its base file (external snapshot detected)", "disk", d.TargetDev, "active", d.Source, "base", rootPath)
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
	targetSSHClient, err = remotessh.Dial(targetSSHConfig)
	if err != nil {
		return fmt.Errorf("connect ssh for target file/export execution: %w", err)
	}
	defer targetSSHClient.Close()
	defer cleanupTargetNBD("cleanup")
	defer cleanupSourceBridge("cleanup")
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
		exists, err := libvirtsync.DomainExists(tgtMgr.Conn, cfg.TargetDomain)
		if err != nil {
			return fmt.Errorf("reinit: check target domain existence: %w", err)
		}
		if exists {
			tgtDom, err := tgtMgr.LookupDomain(cfg.TargetDomain)
			if err != nil {
				return fmt.Errorf("reinit: look up target domain %s: %w", cfg.TargetDomain, err)
			}
			running, runErr := libvirtsync.DomainActive(tgtDom)
			if runErr != nil {
				tgtDom.Free()
				return fmt.Errorf("reinit: check target domain state: %w", runErr)
			}
			if running {
				tgtDom.Free()
				return fmt.Errorf("reinit: target domain %s is running, shut it down before reinitializing", cfg.TargetDomain)
			}
			if err := tgtDom.Undefine(); err != nil {
				tgtDom.Free()
				return fmt.Errorf("reinit: undefine target domain %s: %w", cfg.TargetDomain, err)
			}
			tgtDom.Free()
			trace.Info("reinit: undefined existing target domain", "vm", cfg.TargetDomain)
		}

		for _, d := range qcowDisks {
			reinitTargetPath := util.SetTargetPath(cfg.TargetDiskPath, d.RootSource)
			trace.Info("reinit: removing target disk", "path", reinitTargetPath)
			if out, err := targetSSHClient.Run(ctx, "rm -f "+util.ShQuote(reinitTargetPath)); err != nil {
				return fmt.Errorf("reinit: remove target disk %s: %w: %s", reinitTargetPath, err, out)
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
		trace.Info("Target domain exists, parse metadata info")

		tgtXML, err := tgtDom.GetXMLDesc(0)
		if err != nil {
			tgtDom.Free()
			return fmt.Errorf("read source domain xml: %w", err)
		}
		// tgtDom itself isn't touched again past this point -- only tgtXML
		// (the plain string already extracted from it) is used below -- so
		// free it here rather than holding the target-side libvirt handle
		// open for the rest of this (potentially long-running) sync.
		tgtDom.Free()

		metadataEntryCheckpoint, err = libvirtsync.ParseMetadata(tgtXML, libvirtsync.MetadataFieldLastCheckpoint)
		metadataEntryTimestamp, err = libvirtsync.ParseMetadata(tgtXML, libvirtsync.MetadataFieldLastSync)
		if err != nil {
			trace.Warning("unable to parse target domain metadata entry")
		} else {
			if metadataEntryCheckpoint == "" {
				trace.Warning("empty target domain metadata entry, cannot verify checkpoint chain")
			} else {
				trace.Info("Target domain metadata", "checkpoint", metadataEntryCheckpoint)
			}
			if metadataEntryTimestamp == "" {
				trace.Warning("empty target domain metadata entry, cannot verify timestamp")
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
		if metadataEntryCheckpoint != "" {
			if metadataEntryCheckpoint != parent {
				return fmt.Errorf("checkpoint inconsistency detected: target VM definition lists [%s] as parent checkpoint, but parent checkpoint defined is [%s]", metadataEntryCheckpoint, parent)
			} else {
				trace.Info("Successfully verified checkpoint chain")
			}
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
			freezed = true
			trace.Info("Successfully freezed file systems using guest agent")
		}
	} else {
		trace.Info("VM is not in running state, skipping filesystem freeze")
	}

	if err := libvirtsync.CreateCheckpoint(srcDom, checkpointName, parent, qcowDisks); err != nil {
		// A full sync (parent == "") has no earlier checkpoint to fall back
		// on -- without a checkpoint at all there's no bitmap to establish a
		// baseline for future incremental syncs, so that case still fails
		// outright, same as any other CreateCheckpoint error.
		if parent == "" || !libvirtsync.IsCheckpointBlockedBySnapshot(err) {
			libvirtsync.ThawFs(srcDom, freezed)
			return err
		}
		trace.Warning("checkpoint creation blocked by an existing external snapshot on the source domain; syncing incrementally against the existing checkpoint without advancing the checkpoint chain", "attempted_checkpoint", checkpointName, "parent", parent, "error", err)
	} else {
		checkpointMu.Lock()
		checkpointAdvanced = true
		checkpointMu.Unlock()
	}
	libvirtsync.ThawFs(srcDom, freezed)

	defer func() {
		if runErr == nil || !checkpointAdvanced {
			return
		}
		if err := libvirtsync.DeleteCheckpointIfExists(srcDom, checkpointName); err != nil {
			trace.Error("failed to delete checkpoint after sync error on primary connection", "checkpoint", checkpointName, "error", err)
			if retryErr := libvirtsync.DeleteCheckpointViaReconnect(cfg.SourceURI, cfg.SourceDomain, checkpointName); retryErr != nil {
				trace.Error("failed to delete checkpoint via reconnect", "checkpoint", checkpointName, "error", retryErr)
			} else {
				trace.Info("removed checkpoint after sync failure (reconnect path)", "checkpoint", checkpointName)
			}
		} else {
			trace.Info("removed checkpoint after sync failure", "checkpoint", checkpointName)
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

	if err := libvirtsync.StartPullBackupTCP(srcDom, incrementalCheckpoint, exportBitmap, cfg.SourceNBDBind, cfg.SourceNBDPort, qcowDisks); err != nil {
		return err
	}
	backupMu.Lock()
	backupActive = true
	backupMu.Unlock()
	defer abortBackup("cleanup")

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

	// verifyWindow carries what -verify-online's compare phase needs from
	// beginVerifyWindow. checkpointName is empty for the plain -verify/
	// -verify-fast path (runVerify uses that to tell the two modes apart);
	// non-empty means the compare should reconcile mismatches against that
	// checkpoint's own dirty bitmap instead of failing on the first one.
	type verifyWindow struct {
		checkpointName string
		cleanup        func()
	}

	// beginVerifyWindow opens -verify-online's compare window: it stops the
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

		if err := libvirtsync.StartPullBackupTCP(srcDom, libvirtsync.VerifyWindowCheckpointName, libvirtsync.VerifyWindowCheckpointName, cfg.SourceNBDBind, cfg.SourceNBDPort, qcowDisks); err != nil {
			cleanupVerifyWindow("verify window setup failed")
			return verifyWindow{}, fmt.Errorf("start verify-window backup job: %w", err)
		}
		backupMu.Lock()
		backupActive = true
		backupMu.Unlock()

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
	// syncDisk's defer) and -verify-online's two-phase path (duration =
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
	// Under -verify-online, these two run as separate goroutine invocations
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
		stopCmd := "kill -9 $(cat " + util.ShQuote(pidFile) + ") || true"
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

	// runVerify is today's `if cfg.Verify` block, unchanged in behavior for
	// its original caller (syncDisk, verify.checkpointName always "" there
	// since -verify and -verify-online are mutually exclusive). Under
	// -verify-online (verify.checkpointName != ""), the compare step
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
		stopVerifyCmd := "kill -9 $(cat " + util.ShQuote(verifyPidFile) + ") || true"
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
		// copy itself already requires. Still true under -verify-online:
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
			trace.Info("verify: comparing source and target images", "disk", d.TargetDev, "source", sourceNBDURL, "target", targetPath, "fast", cfg.VerifyFast)
			if cfg.VerifyFast {
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

	// syncDisk is the single-phase path: copy+commit, then (if cfg.Verify)
	// verify against the same already-open backup export, all in one
	// goroutine per disk with zero cross-disk coordination -- exactly
	// today's behavior, used whenever -verify-online is NOT set (including
	// plain syncs and plain -verify/-verify-fast, which suspend instead of
	// using a barrier). verifyWindow{} (checkpointName == "") tells
	// runVerify to use the original compare path, not -verify-online's.
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
		if cfg.Verify {
			return runVerify(i, d, res, verifyWindow{})
		}
		trace.Info("disk sync complete", "disk", d.TargetDev, "elapsed", time.Since(diskStart).Round(time.Millisecond).String())
		return nil
	}

	if !cfg.VerifyOnline {
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
		// -verify-online: copy+commit for every disk first, then (once,
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
	trace.Info("Adding metadata information")
	var newXML string
	newXML, err = libvirtsync.UpdateSyncMetadata(srcXML, effectiveCheckpoint)
	if err != nil {
		trace.Warning("Unable to add metadata info", err)
		newXML = srcXML
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
	if err := libvirtsync.DefineDomain(tgtMgr, cfg.TargetDomain, newXML, cfg.TargetDiskPath, rootSourceByLiveSource); err != nil {
		return err
	}

	return nil
}
