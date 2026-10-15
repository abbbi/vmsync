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
  `-verify`. `0`=ran and matched, `1`=ran and found a difference (the
  replica is wrong), `2`=**could not be performed** (the replica's state is unknown; fix the
  tooling). The authoritative wording is the metric's own HELP text in `pkg/metrics`.
- `vmsync_verification_timestamp_seconds{source_host,target_host,vm}` -- only present for runs
  that had `-verify` set; Unix time the run last performed verification.

`2` is the value most alerts miss, and it is the one a wedged target produces: a helper that
stopped answering, an export that never came up, a plan that could not be built. It means
*nobody knows whether this replica is good*, which is not the same as `1` and must not be
handled as "the replica is fine". Alert on it with a `for:` clause to ignore transients.

## Metrics for the pre-commit integrity check

- `vmsync_checksum_state{source_host,target_host,vm}` -- what the pre-commit check concluded:
  `0`=ran and matched, `1`=ran and the target's bytes differ from what was sent, `2`=could not
  be performed **or was deliberately skipped**. Emitted whenever the run got as far as deciding
  about the check.

**Read `2` carefully here -- it is not the same as `vmsync_verification_state == 2.`** It
conflates three situations: `-no-checksum` was passed (so every such run reports `2`
routinely), no matching helper is deployed on the target, and the check was attempted and could
not complete. Only the last two are faults, and the series cannot tell them apart. So on an
estate that uses `-no-checksum` anywhere, `vmsync_checksum_state == 2` is not an alertable
condition on its own -- scope it to the pairs you expect to be checked, or alert on
`vmsync_sync_state == 1` and read this series as context. The skipped case is still worth
exporting, and the metric's own comment says why: a helper that is missing or version-skewed
silently disables a default-on check for every run on that host while each sync still reports
success.

**A pre-commit stall shows up here and nowhere else.** The check runs before the commit, so a
run that fails there never reaches the verify phase at all: `vmsync_checksum_state` goes to `2`
and `vmsync_verification_state` is **absent**, not `2`. An absent series resets a `for:` clause
rather than building it, so an estate that alerts only on `vmsync_verification_state == 2` sees
*less* from a stalled target, not more.

## Alerting on a run that never finishes

This changed, and a rule written before it will now stay silent.

`vmsync_last_run_timestamp_seconds` is refreshed on success **and on failure**, so the staleness
rule above (`time() - vmsync_last_run_timestamp_seconds > 86400`) only fires while a run is
failing to *finish*. That used to be the one rule that caught a wedged target: a run parked
forever on an unresponsive target never wrote a textfile at all, so the series went stale and the
alert fired.

vmsync now bounds those waits -- the target's digest and every supervised target command report
once a minute and are given up on when they go silent -- so a wedged target produces a run that
**finishes, as a failure, promptly**. The timestamp is refreshed, the staleness rule goes quiet,
and the fault shows up only in the state series. Pair them:

```yaml
# the run finished and failed
- expr: vmsync_sync_state == 1
  for: 30m
# nothing could be checked -- the replica's state is unknown
- expr: vmsync_verification_state == 2
  for: 24h
# only on pairs you expect to be checked: 2 also means -no-checksum
- expr: vmsync_checksum_state == 2
  for: 1h
# still needed: nothing is writing a textfile for this pair at all
- expr: time() - vmsync_last_run_timestamp_seconds > 86400
```

Keep the staleness rule: it still catches the cases no bound covers -- a host that is not
running vmsync at all, a crashed scheduler, a node_exporter that lost its textfile directory.

No panels reference the verification or checksum series yet -- add them here (and to
`vmsync-dashboard.json`) if you build panels for them.