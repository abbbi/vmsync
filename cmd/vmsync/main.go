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
	"vmsync/pkg/nbdbridge"
	"vmsync/pkg/nbdsync"
	"vmsync/pkg/remotessh"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"

	"libvirt.org/go/libvirt"
)

const VERSION = "0.30"

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
		CompressLevel       int
		NetBuffer           string
		BridgeHelperPath    string
		ShowVersion         bool
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
	flag.StringVar(&cfg.SSHUser, "ssh-user", "", "ssh user for remote command execution (defaults from URI user, then root)")
	flag.StringVar(&cfg.SSHKey, "ssh-key", "", "private key path for ssh authentication")
	flag.StringVar(&cfg.SSHPassword, "ssh-password", "", "password for ssh authentication")
	flag.IntVar(&cfg.SSHPort, "ssh-port", 22, "ssh port for remote command execution")
	flag.BoolVar(&cfg.SSHInsecure, "ssh-insecure-host-key", false, "disable host key verification (not recommended)")
	flag.StringVar(&cfg.KnownHosts, "ssh-known-hosts", "", "known_hosts file path (defaults to ~/.ssh/known_hosts)")
	flag.IntVar(&cfg.SSHTimeoutSec, "ssh-timeout-sec", 10, "ssh connection timeout in seconds")
	flag.BoolVar(&cfg.Start, "start", false, "In case vm is in non-running state, start in paused mode to allow sync.")
	flag.BoolVar(&cfg.Reinit, "reinit", false, "Discard all vmsync checkpoints on the source and the existing target domain/disks, then perform a fresh full sync. Use to recover from a broken checkpoint chain (e.g. \"Bitmap already exists\" errors).")
	flag.IntVar(&cfg.ReinitAfterFailures, "reinit-after-failures", 0, "After this many consecutive sync failures (tracked in the target domain's vmsync metadata), automatically reinit (as with -reinit) instead of trying again the same way. 0 disables this (default).")
	flag.BoolVar(&cfg.Compress, "compress", false, "Compress NBD traffic between hosts using zstd, tunneled over the existing SSH connection. Compression runs natively on both ends (no external tool dependency); the remote side requires vmsync-bridge-helper deployed at -bridge-helper-path. Core sync behavior is unchanged when this is not set.")
	flag.IntVar(&cfg.CompressLevel, "compress-level", 3, "zstd compression level to use when --compress is set (1-19)")
	flag.StringVar(&cfg.NetBuffer, "netbuffer", "", "Buffer NBD bridge traffic through a bounded in-memory buffer to smooth throughput, formatted as <blocksize>,<buffersize> (e.g. 64k,512M). Runs natively on both ends (no external tool dependency). Independent of --compress -- usable alone or combined with it.")
	flag.StringVar(&cfg.BridgeHelperPath, "bridge-helper-path", "/usr/local/bin/vmsync-bridge-helper", "Remote path to the vmsync-bridge-helper binary, used when --compress/--netbuffer is set. Must already be deployed there by you (e.g. via scp) -- vmsync does not upload it.")
	flag.BoolVar(&cfg.Debug, "debug", false, "Enable debug logging")
	flag.BoolVar(&cfg.ShowVersion, "v", false, "Show version and exit")
	flag.BoolVar(&cfg.ShowVersion, "version", false, "Show version and exit")
	flag.Parse()

	if cfg.ShowVersion {
		trace.Info(fmt.Sprintf("vmsync Version: %s", VERSION))
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
		if err := nbdbridge.ValidateCompressLevel(cfg.CompressLevel); err != nil {
			trace.Error("invalid compress configuration", "error", err)
			os.Exit(2)
		}
	}
	if cfg.NetBuffer != "" {
		if _, _, err := nbdbridge.ParseNetBufferSpec(cfg.NetBuffer); err != nil {
			trace.Error("invalid netbuffer configuration", "error", err)
			os.Exit(2)
		}
	}

	trace.SetDebug(cfg.Debug)

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
	CompressLevel       int
	NetBuffer           string
	BridgeHelperPath    string
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
	var stopMu sync.Mutex
	targetStopCommands := make([]string, 0)
	sourceStopCommands := make([]string, 0)
	var checkpointName string
	var parent string
	var freezed bool = false
	var started bool = false

	netbufferBlock, netbufferSize, err := nbdbridge.ParseNetBufferSpec(cfg.NetBuffer)
	if err != nil {
		return err
	}
	bridgeCfg := nbdbridge.Config{
		Compress:       cfg.Compress,
		CompressLevel:  cfg.CompressLevel,
		NetBufferBlock: netbufferBlock,
		NetBufferSize:  netbufferSize,
		HelperPath:     cfg.BridgeHelperPath,
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

	trace.Info(fmt.Sprintf("%s, Version: %s", os.Args, VERSION))

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

	if srcState, err = libvirtsync.DomainRunning(srcDom); err != nil {
		return err
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
			cleanupWg.Add(4)
			go func() { defer cleanupWg.Done(); abortBackup(sig.String()) }()
			go func() { defer cleanupWg.Done(); cleanupTargetNBD(sig.String()) }()
			go func() { defer cleanupWg.Done(); cleanupSourceBridge(sig.String()) }()
			go func() {
				defer cleanupWg.Done()
				// Mirrors the deferred checkpoint cleanup further down in
				// run() -- duplicated here because that defer never gets a
				// chance to run if we os.Exit below without waiting for it.
				if checkpointName == "" {
					return
				}
				if err := callWithTimeout("delete checkpoint", 5*time.Second, func() error {
					return libvirtsync.DeleteCheckpointIfExists(srcDom, checkpointName)
				}); err != nil {
					trace.Error("failed to delete checkpoint after interrupt", "checkpoint", checkpointName, "error", err)
					if retryErr := libvirtsync.DeleteCheckpointViaReconnect(cfg.SourceURI, cfg.SourceDomain, checkpointName); retryErr != nil {
						trace.Error("failed to delete checkpoint via reconnect after interrupt", "checkpoint", checkpointName, "error", retryErr)
					}
				} else {
					trace.Info("removed checkpoint after interrupt", "checkpoint", checkpointName)
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
  
  nvram, err := libvirtsync.DetectNvram(srcXML)
	if err != nil {
		return err
	}
	if nvram != "" {
		x, _ := util.RemotePathExists(ctx, targetSSHClient, nvram)
		if !x {
			trace.Warning("nvram setting detected in vm config", "path", nvram, "but files do not exist on target host")
		}
	}

	loader, lerr := libvirtsync.DetectLoader(srcXML)
	if lerr != nil {
		return lerr
	}
	if loader != "" {
		x, _ := util.RemotePathExists(ctx, targetSSHClient, loader)
		if !x {
			trace.Warning("loader setting detected in vm config", "path", loader, "but files do not exist on target host")
		}
	}
  
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
		var info disk.QemuImgInfo
		if sourceNeedsSSH {
			trace.Info("running remote qemu-img info", "disk", d.TargetDev, "path", d.Source)
			info, err = disk.QemuImgInfoJSONRemote(ctx, sourceSSHClient, d.Source)
		} else {
			trace.Info("running local qemu-img info", "disk", d.TargetDev, "path", d.Source)
			info, err = disk.QemuImgInfoJSON(d.Source)
		}
		if err != nil {
			return err
		}
		qcowDisks[i].VirtualSize = info.VirtualSize
		qcowDisks[i].ClusterSize = info.ClusterSize
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

	if cfg.Reinit {
		trace.Warning("reinit requested: discarding checkpoint chain and existing target state", "domain", cfg.SourceDomain)
		if err := libvirtsync.AbortActiveBlockJobs(srcDom, qcowDisks); err != nil {
			return fmt.Errorf("reinit: abort active block jobs: %w", err)
		}
		if err := libvirtsync.DeleteAllManagedCheckpoints(srcDom); err != nil {
			return fmt.Errorf("reinit: delete existing checkpoints: %w", err)
		}

		if tgtDom, lookupErr := tgtMgr.LookupDomain(cfg.TargetDomain); lookupErr == nil {
			running, runErr := libvirtsync.DomainRunning(tgtDom)
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
			reinitTargetPath := util.SetTargetPath(cfg.TargetDiskPath, d.Source)
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
	checkpointName, parent, err = libvirtsync.NextCheckpointName(existing)
	if err != nil {
		return err
	}
	var targetPath string
	if parent == "" {
		// Preflight for full sync: fail before sync operations if target disk path exists.
		for _, d := range qcowDisks {
			targetPath = util.SetTargetPath(cfg.TargetDiskPath, d.Source)
			trace.Info("Using target", "path", targetPath, "disk", d.TargetDev)
			targetDir := path.Dir(targetPath)
			if _, err := targetSSHClient.Run(ctx, "mkdir -p "+util.ShQuote(targetDir)); err != nil {
				return fmt.Errorf("create remote target dir %s: %w", targetDir, err)
			}
			exists, _ := util.RemotePathExists(ctx, targetSSHClient, targetPath)
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
		if tgtState, err = libvirtsync.DomainRunning(tgtDom); err != nil {
			return err
		}
		if tgtState == true {
			return fmt.Errorf("target domain %s is active require shutoff before sync", cfg.TargetDomain)
		}
		trace.Info("Target domain exists, parse metadata info")

		tgtXML, err := tgtDom.GetXMLDesc(0)
		if err != nil {
			return fmt.Errorf("read source domain xml: %w", err)
		}

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
					targetPath = util.SetTargetPath(cfg.TargetDiskPath, d.Source)
					out, err := targetSSHClient.Run(ctx, "stat -c '%Y' "+targetPath)
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
		defer tgtDom.Free()
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

	if srcState {
		if err := srcDom.FSFreeze(nil, 0); err != nil {
			trace.Warning("Filesystem freeze failed", "error", err)
		} else {
			freezed = true
			trace.Info("Successfully freezed file systems using guest agent")
		}
	} else {
		trace.Info("VM is not in running state, skipping filesystem freeze")
	}

	if err := libvirtsync.CreateCheckpoint(srcDom, checkpointName, parent, qcowDisks); err != nil {
		libvirtsync.ThawFs(srcDom, freezed)
		return err
	}
	libvirtsync.ThawFs(srcDom, freezed)

	defer func() {
		if runErr == nil {
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
		trace.Info("starting incremental pull backup", "parent_checkpoint", parent, "new_checkpoint", checkpointName)
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

	nbdHost := cfg.SourceNBDHost
	if nbdHost == "" {
		nbdHost = util.ConnectHostFromBindOrURI(cfg.SourceNBDBind, cfg.SourceURI)
	}
	trace.Info("source nbd port in use", "side", "source", "kind", "nbd_export", "host", nbdHost, "port", cfg.SourceNBDPort)
	targetNBDHost := cfg.TargetNBDHost
	if targetNBDHost == "" {
		targetNBDHost = util.ConnectHostFromBindOrURI(cfg.TargetNBDBind, cfg.TargetURI)
	}

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
		localPort, counters, stopLocal, err := nbdbridge.StartLocal(ctx, sourceSSHClient, fmt.Sprintf("127.0.0.1:%d", sourceBridgePort), bridgeCfg)
		if err != nil {
			return fmt.Errorf("start local nbd bridge relay for source: %w", err)
		}
		defer stopLocal()
		effectiveSourceHost = "127.0.0.1"
		effectiveSourcePort = localPort
		sourceBridgeCounters = counters
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
	syncDisk := func(i int, d disk.QcowDisk) error {
		diskStart := time.Now()
		runTargetCommand := func(command, action string) error {
			trace.Debug(command)
			out, err := targetSSHClient.Run(ctx, command)
			if err != nil {
				return fmt.Errorf("%s: %w: %s", action, err, out)
			}
			return nil
		}

		trace.Info("reading disk via libvirt backup NBD tcp export", "disk", d.TargetDev, "export", d.TargetDev)
		extents, diskSize, dirty, err := nbdsync.ChangedExtentsTCP(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, bitmapForRead, incrementalMode)
		if err != nil {
			return err
		}
		if dirty == 0 {
			trace.Info("No changed extents selected, skipping copy", "disk", d.TargetDev, "elapsed", time.Since(diskStart).Round(time.Millisecond).String())
			return nil
		}

		// Avoid datarace in this goroutine by declaring targetPath as local var instead of a shared one
		targetPath := util.SetTargetPath(cfg.TargetDiskPath, d.Source)
		createCmd := "qemu-img create -f qcow2 " + util.ShQuote(targetPath) + " -o cluster_size=" + fmt.Sprintf("%d", d.ClusterSize) + " " + fmt.Sprintf("%d", d.VirtualSize)
		var targetPathInc string
		if incrementalMode {
			targetPathInc = targetPath + "_" + bitmapForRead
			trace.Info("Create temporary image", "disk", targetPathInc)
			createCmd = "qemu-img create -f qcow2 -F qcow2  -o cluster_size=" + fmt.Sprintf("%d", d.ClusterSize) + " " + util.ShQuote(targetPathInc) + " -b " + targetPath + " " + fmt.Sprintf("%d", d.VirtualSize)
		}
		if err := runTargetCommand(createCmd, fmt.Sprintf("create remote qcow2 %s", targetPathInc)); err != nil {
			return err
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
			return err
		}

		stopCmd := "kill -9 $(cat " + util.ShQuote(pidFile) + ")"
		stopMu.Lock()
		targetStopCommands = append(targetStopCommands, stopCmd+" || true")
		stopMu.Unlock()

		trace.Info("target nbd port in use", "side", "target", "kind", "nbd_export", "disk", d.TargetDev, "host", targetNBDHost, "port", targetPort)

		// Default to the direct, uncompressed path; overridden below when
		// --compress/--netbuffer are set (target SSH is always available).
		effectiveTargetHost := targetNBDHost
		effectiveTargetPort := targetPort
		var targetBridgeCounters *nbdbridge.ByteCounters
		if bridgeCfg.Enabled() {
			// All real qemu-nbd ports occupy [TargetNBDPort, TargetNBDPort+N),
			// so the bridge ports lay out right after them, as one contiguous
			// block [TargetNBDPort+N, TargetNBDPort+2N).
			targetBridgePort := targetPort + len(qcowDisks)
			bridgeStopCmd, err := nbdbridge.StartRemote(ctx, targetSSHClient, targetBridgePort, targetPort, bridgeCfg)
			if err != nil {
				return fmt.Errorf("start target nbd bridge for %s: %w", d.TargetDev, err)
			}
			stopMu.Lock()
			targetStopCommands = append(targetStopCommands, bridgeStopCmd)
			stopMu.Unlock()
			trace.Info("target nbd port in use", "side", "target", "kind", "bridge_remote", "disk", d.TargetDev, "host", targetNBDHost, "port", targetBridgePort)
			localPort, counters, stopLocal, err := nbdbridge.StartLocal(ctx, targetSSHClient, fmt.Sprintf("127.0.0.1:%d", targetBridgePort), bridgeCfg)
			if err != nil {
				return fmt.Errorf("start local nbd bridge relay for %s: %w", d.TargetDev, err)
			}
			defer stopLocal()
			effectiveTargetHost = "127.0.0.1"
			effectiveTargetPort = localPort
			targetBridgeCounters = counters
			trace.Info("target nbd port in use", "side", "target", "kind", "bridge_local", "disk", d.TargetDev, "host", "127.0.0.1", "port", localPort)
		} else {
			if err := nbdsync.WaitForTCPExport(targetNBDHost, targetPort, 10*time.Second); err != nil {
				return fmt.Errorf("wait for target nbd export %s:%d: %w", targetNBDHost, targetPort, err)
			}
		}

		trace.Info("copy extents to remote target", "extents", len(extents), "path", targetPath, "disk_size", diskSize)
		if err := nbdsync.CopyExtentsTCP(ctx, effectiveSourceHost, effectiveSourcePort, d.TargetDev, effectiveTargetHost, effectiveTargetPort, extents); err != nil {
			return err
		}

		if targetBridgeCounters != nil {
			logicalBytes := nbdbridge.SumLogicalDirtyBytes(extents)
			trace.Info("target nbd bridge compression", "disk", d.TargetDev, "savings", nbdbridge.FormatSavings(logicalBytes, targetBridgeCounters.SentSnapshot()))
		}
		if sourceBridgeCounters != nil {
			logicalBytes := nbdbridge.SumLogicalDirtyBytes(extents)
			trace.Info("source nbd bridge compression", "disk", d.TargetDev, "savings", nbdbridge.FormatSavings(logicalBytes, sourceBridgeCounters.SentSnapshot()))
		}

		trace.Info("Stopping remote daemon", "device", d.TargetDev)
		if err := runTargetCommand(stopCmd, fmt.Sprintf("stop qemu-nbd for %s", targetPath)); err != nil {
			return err
		}

		if incrementalMode {
			trace.Info("Committing changes to base", "image", targetPath)
			commitCmd := "qemu-img commit -b " + targetPath + " " + targetPathInc
			if err := runTargetCommand(commitCmd, fmt.Sprintf("committing changes for %s", targetPathInc)); err != nil {
				return err
			}
			trace.Info("Removing temporary", "image", targetPathInc)
			if err := runTargetCommand("rm -f "+targetPathInc, fmt.Sprintf("removing target image %s", targetPathInc)); err != nil {
				return err
			}
		}
		trace.Info("disk sync complete", "disk", d.TargetDev, "elapsed", time.Since(diskStart).Round(time.Millisecond).String())
		return nil
	}

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

	for err := range errCh {
		if err != nil {
			return err
		}
	}

	if incrementalMode {
		trace.Info("sync successful cleaning up parent checkpoint", "parent", parent)
		err := libvirtsync.DeleteCheckpointIfExists(srcDom, parent)
		if err != nil {
			return err
		}
	}
	trace.Info("Adding metadata information")
	var newXML string
	newXML, err = libvirtsync.UpdateSyncMetadata(srcXML, checkpointName)
	if err != nil {
		trace.Warning("Unable to add metadata info", err)
		newXML = srcXML
	}

	if err := libvirtsync.DefineDomain(tgtMgr, cfg.TargetDomain, newXML, cfg.TargetDiskPath); err != nil {
		return err
	}

	return nil
}
