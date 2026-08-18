# vmsync Grafana dashboard

`vmsync-dashboard.json` visualizes the metrics vmsync writes via `-prometheus-textfile`
(see [`pkg/nbdbridge/README.md`](../../pkg/nbdbridge/README.md) and `pkg/metrics`).

## Prerequisites

1. Run vmsync with `-prometheus-textfile /var/lib/node_exporter/textfile_collector/vmsync_<vm>.prom`
   (one distinct path per VM/job -- each file is overwritten wholesale on every run, so sharing
   one path across VMs means only the most recent one survives).
2. `node_exporter` must be started with `--collector.textfile.directory` pointing at that
   directory, and the `textfile` collector enabled (it is by default).
3. Prometheus must be scraping that `node_exporter`.

## Metrics this dashboard uses

- `vmsync_disk_size_bytes{source_host,target_host,vm,disk}`
- `vmsync_transferred_bytes{source_host,target_host,vm,disk}` -- logical bytes copied
- `vmsync_compressed_transferred_bytes{source_host,target_host,vm,disk}` -- actual wire bytes
  (equal to `vmsync_transferred_bytes` when `--compress`/`--netbuffer` aren't used)
- `vmsync_sync_duration_seconds{source_host,target_host,vm,disk}`
- `vmsync_sync_state{source_host,target_host,vm}` -- whole-run result (not per-disk): `0`=success,
  `1`=failure, `2`=succeeded but the guest filesystem freeze failed (checkpoint is only
  crash-consistent, not application-consistent)
- `vmsync_last_run_timestamp_seconds{source_host,target_host,vm}` -- Unix time the run
  finished (success or failure); used for staleness detection since the textfile itself
  doesn't expose a reliable "last written" signal on its own
- `vmsync_external_snapshot_count{source_host,target_host,vm}` -- number of external disk
  snapshots on the source domain; libvirt blocks new checkpoint creation while any exist.
- `vmsync_warning_count{source_host,target_host,vm}` -- how many WARNING-level lines this run
  logged
- `vmsync_error_count{source_host,target_host,vm}` -- how many ERROR-level lines this run
  logged

All of these are gauges written once per vmsync run, not counters -- Prometheus will show the
same value repeatedly between runs, then a step change when the next run completes. The
dashboard's time series panels use `stepAfter` interpolation to reflect that honestly, and
deliberately avoid `rate()`/`increase()` anywhere, since those are meaningless on a gauge like
this (there's nothing "accumulating" between samples).

That applies to `vmsync_warning_count`/`vmsync_error_count` too, despite the `_count` suffix
reading like a counter. vmsync is a one-shot CLI, not a daemon: each invocation is a fresh
process starting from zero, so the value describes **that run alone** and drops back down on
the next quiet one rather than climbing monotonically. `rate()`/`increase()` over them produce
nonsense; alert on the value itself (`vmsync_warning_count > 0`), or on
`max_over_time(...[24h])` for a "did anything degrade today" view.

## Alerting on warnings and errors

These two are deliberately finer-grained than `vmsync_sync_state`, which only reports the final
outcome. A run can finish as success while still having logged, then transparently recovered
from, real problems -- a reconnect fallback kicking in, a self-heal cleaning up leftover state
from an earlier crash, a UUID collision forcing a stripped-UUID redefine. `vmsync_sync_state`
stays `0` for all of those; `vmsync_warning_count` is what makes them visible.

Two caveats worth knowing before you build alerts on them:

- **A warning is not necessarily actionable on its own.** Some are informational-but-notable
  (the ones above), and a domain with an external snapshot present can log the same warning on
  every single run. Alert on a *change* in the steady-state value, or pair the alert with the
  specific VM's known baseline, rather than treating any non-zero value as a page.
- **One warning is emitted too late to be counted.** The textfile is written as the last thing
  the sync itself does, so the failure-counter bookkeeping that runs afterward
  (`RecordTargetSyncFailure`, only when `-reinit-after-failures` is set) is not reflected in
  `vmsync_warning_count`. That one reports trouble *writing the failure counter*, not trouble
  with the sync -- and the sync's own outcome is already in `vmsync_sync_state`. The error that
  actually failed a run **is** counted.

## Metrics vmsync also optionally emits with -verify flag

- `vmsync_verification_state{source_host,target_host,vm}` -- only present for runs that had
  `-verify` - `0`=success,`1`=failure
- `vmsync_verification_timestamp_seconds{source_host,target_host,vm}` -- only present for runs
  that had `-verify` set; Unix time the run last performed verification.

No panels reference these yet -- add them here (and to `vmsync-dashboard.json`) if you build
panels for them.