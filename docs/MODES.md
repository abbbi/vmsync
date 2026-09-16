# How vmsync runs

vmsync itself is a replication engine, and can be driven in several ways.
This page details each way of running vmsync with it's perks and costs, and where to start.

If you are new, read [the model](HOWTO.md#the-model-checkpoints-not-snapshots) first —
it is four paragraphs and everything below assumes it.

## Three layers

Every mode is a choice about two questions, and nothing else:

1. **Who decides when a sync runs?**
2. **Who is watching?**

The replication itself is always the same binary, the same checkpoints, the same refusals.

| layer | what it is | what it does |
| --- | --- | --- |
| **Engine** | `vmsync` | Replicates **one VM, once**, and exits. Also carries the one-shot verbs: restore points, promotion, inversion, fencing. Every mode below runs this. |
| **Driver** | you, `cron`, `vmsync-parallel.sh`, or `vmsync-agent` | Decides **when**, and for which VMs. |
| **Control plane** | `vmsync-ui` | Estate-wide **visibility and orchestration**. Optional, and deliberately not required for replication to keep working. |

A useful consequence: **you can mix modes in one estate.** A site driven by cron and a
site driven by agents replicate to the same targets in the same format, and a
`--monitor` agent can report the cron-driven one into the same console as the rest.

## Modes

vmsync can be run invoked in multiples ways, each one representing run mode.
Here's a quick comparison of what each mode provides.

| | manual | cron | `vmsync-parallel.sh` | agent `--standalone` | agent `--monitor` | agent `--controlled` |
| --- | :-: | :-: | :-: | :-: | :-: | :-: |
| Replicates | ✅ | ✅ | ✅ | ✅ | — | ✅ |
| Who decides when | you | crontab | you/crontab | local schedule file | *nmonitor only* | the control plane |
| Finds the VMs for you | — | — | ✅ | ✅ | ✅ (reports them) | ✅ |
| Concurrency limit + NBD port accounting | — | — | ✅ | ✅ | — | ✅ |
| Per-VM cadence and templates | — | — | — | ✅ | — | ✅ |
| Verify windows (`verify_days` / `verify_window`) | — | — | — | ✅ | — | ✅ |
| Automatic reinit after N failures | flag | flag | ✅ | ✅ | — | ✅ |
| Refuses to run twice on one VM | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Durable record of every run | — | — | log file | run log | run log | run log |
| Prometheus metrics | per run | per run | per run | ✅ + agent metrics | ✅ agent metrics | ✅ + agent metrics |
| Estate dashboard, split-brain view | — | — | — | — | ✅ | ✅ |
| Failover from a console | — | — | — | — | — | ✅ |
| Restore points | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ + console |
| Autofencing | — | — | — | ✅ | **—** | ✅ |
| Needs a control plane | — | — | — | — | ✅ | ✅ |
| Keeps replicating if the control plane dies | n/a | n/a | n/a | n/a | n/a | ✅ |
| Needs an inbound port on the hypervisor | — | — | — | — | — | — |

Notes on the rows that are easy to misread:

- **Refuses to run twice** is an engine property, not a driver one. `vmsync` takes a
  per-domain lock under `/run/vmsync-locks` and exits **75** (`EX_TEMPFAIL`) if another
  run already holds it, so this protection is there even when you drive it by hand.
- **Restore points** are engine verbs (`-list-restore-points`, `-clone-restore-point`,
  `-restore-restore-point`), so they work in every mode. What `--controlled` adds is a
  console that lists them and performs a restore as part of a failover.
- **Autofencing** is what stops one VM serving in two places after a failover. It is an
  agent feature — nothing on a cron timer is watching for it. `--monitor` deliberately
  does not fence: see [the three modes](../cmd/vmsync-agent/README.md#the-three-modes).
- **Keeps replicating if the control plane dies** is `n/a` rather than `✅` for the modes
  that never had one. For `--controlled` it is a designed property: the agent caches its
  configuration on disk and keeps running from that cache through a WAN partition.

## The modes, and where to start

### Manual — `vmsync`

One command, one VM, one sync. This is how you learn the tool and how you test a pair
before automating it, and it is a perfectly reasonable way to run a handful of VMs.

**Start here:** [HOWTO — Your first sync](HOWTO.md#your-first-sync).

You give up nothing except automation: every refusal, every integrity check and every
failover verb is available. Read [Ports and firewalls](HOWTO.md#ports-and-firewalls)
before the first run over a WAN.

### cron

Manual mode on a timer. Right for a small, stable set of VMs where you already have
configuration management and do not want another daemon.

There is no separate guide because there is nothing to configure beyond the command
line you already tested by hand:

```bash
# /etc/cron.d/vmsync — one line per VM, staggered so they do not contend
7  * * * * root /usr/local/bin/vmsync -source-domain web01 -target-uri qemu+ssh://hv02/system -target-domain web01 -target-disk-path /vm_data >>/var/log/vmsync-web01.log 2>&1
22 * * * * root /usr/local/bin/vmsync -source-domain db01  -target-uri qemu+ssh://hv02/system -target-domain db01  -target-disk-path /vm_data >>/var/log/vmsync-db01.log 2>&1
```

What you take on yourself: staggering the VMs so they do not fight for bandwidth,
allocating non-overlapping NBD port ranges if you run any of them concurrently, and
noticing when one stops working. Exit **75** means "another run holds the lock" and is
not a failure — do not alert on it.

If you outgrow this, the next step up is `vmsync-parallel.sh`, which is the same idea
with the bookkeeping done for you. If you want a console, add a `--monitor` agent
alongside the crontab: it changes nothing about how replication runs and makes the site
visible.

### `vmsync-parallel.sh`

A launcher for **the whole host**: it enumerates the VMs itself, skips an ignore list,
runs several syncs concurrently, and — the part that is genuinely fiddly by hand —
allocates NBD port ranges so two concurrent VMs never overlap. It also carries a
bandwidth cap, an `-reinit-after-failures` policy and its own lock directory.

**Start here:** the configuration block at the top of
[`contrib/runner/vmsync-parallel.sh`](../contrib/runner/vmsync-parallel.sh). It is
configured by editing the script — set `DESTINATION_HOST`, `TARGET_VM_PATH`,
`DESTINATION_SSH_KEY`, the base ports and the compression settings, then run it from
cron once per interval.

What you give up against an agent: no per-VM cadence (every VM syncs on the same
timer), no verify windows, and no estate view. What you keep: no daemon, no enrolment,
nothing to run but a shell script.

### Agent — `--standalone`

A scheduler daemon with **no control plane at all**. The schedule is a local JSON file:
per-VM intervals, named templates, verify windows expressed as systemd-style calendar
expressions. It runs the syncs, records every one in a durable run log, exports its own
metrics, and fences a displaced source after a failover.

**Start here:**
[agent README — Install](../cmd/vmsync-agent/README.md#install), then
[Standalone: a scheduler with no control plane](../cmd/vmsync-agent/README.md#standalone-a-scheduler-with-no-control-plane).

```bash
vmsync-agent --standalone --config /etc/vmsync/agent.json
```

This is the sweet spot for a single pair of hypervisors: everything the agent does for
scheduling, none of the enrolment or the second service to run.

What you give up: no estate view, and no console to fail over from — a promotion is a
`vmsync` command you run by hand, which is
[documented](HOWTO.md#failover) and not difficult.

### Agent — `--monitor`

**Reports, and does nothing.** It enrols with a control plane and keeps reporting, but
runs no scheduler, executes no operations, and does not fence.

This is the mode for a host whose replication is driven by something else — cron or
`vmsync-parallel.sh` — where the point is to stop that site being invisible. The agent
reads what those runs left behind in libvirt metadata and reports it, so a cron-driven
VM appears in the console with real freshness, real roles and real verify state.

**Start here:**
[agent README — Install](../cmd/vmsync-agent/README.md#install) and
[Enrol](../cmd/vmsync-agent/README.md#enrol), then set
`VMSYNC_AGENT_MODE=--monitor`.

What you give up, stated plainly: **no split-brain protection from this agent**, because
fencing stops a running VM and a mode whose contract is "does not act" cannot carry it.
Anything you enter in the console for a monitored host is stored and never run; the
console marks those hosts `monitor` and says so on the schedule page. If you want
fencing without scheduling, that is `--controlled` with `"features": {"schedule": false}`.

### Agent — `--controlled` + `vmsync-ui`

The full control plane. The UI holds the schedule, the agents run it, and the console
gives you an availability dashboard, split-brain detection, a schedule and template
editor, orchestrated failover with restore points, and an audit log.

**Start here, in this order:**

1. **vmsync_ui** `README.md` → *Build and install*
2. **vmsync_ui** `README.md` → *Configure*
3. **vmsync_ui** `README.md` → *Enrol an agent*
4. [agent README — Install](../cmd/vmsync-agent/README.md#install) and
   [Enrol](../cmd/vmsync-agent/README.md#enrol), with
   `VMSYNC_AGENT_MODE=--controlled`

The agent **dials out and never listens**, so a hypervisor still needs no inbound port —
which is the point, because the UI normally lives at the DR site behind its own
firewall.

What you take on: one more service to run, and enrolment for each host. What the design
deliberately protects you from: the UI holds no credentials for your hypervisors, cannot
redirect a sync at a host of its choosing (pairing comes from each VM's own
`replica_targets`), and cannot grant an agent a mode — that is a flag in the unit file.

## Choosing

- **One or two VMs, or you are still evaluating** → manual.
- **A handful of VMs, stable, you already run config management** → cron.
- **A whole hypervisor, one timer, no daemon** → `vmsync-parallel.sh`.
- **One pair of hypervisors, you want per-VM cadence and verify windows** → agent `--standalone`.
- **Several sites, you want one screen that tells you whether you are protected** → `vmsync-ui` + agent `--controlled`.
- **You already have any of the above working and just want to view it in an UI** → add agent `--monitor`. It changes nothing about how replication runs.

None of these is a one-way door, but moving between the agent modes has a few
sharp edges worth reading first — mostly of the form "the host keeps running and
quietly does something other than what you assumed". See
[Changing a host's mode](../cmd/vmsync-agent/README.md#changing-a-hosts-mode).

## Reference

| | |
| --- | --- |
| Engine reference, flags, roles, failover | [README.md](../README.md) |
| Engine task guide | [docs/HOWTO.md](HOWTO.md) |
| Manual operations: failover, invert, recovery | [docs/RUNBOOK.md](RUNBOOK.md) |
| Agent reference | [cmd/vmsync-agent/README.md](../cmd/vmsync-agent/README.md) |
| Control plane reference | `vmsync_ui/README.md` *(separate repository)* |
| Scheduling, templates, verify windows | [docs/design/scheduling.md](design/scheduling.md) |
| Agent configuration file | [docs/design/agent-config.md](design/agent-config.md) |
| Integrity: the commit barrier | [docs/design/commit-barrier.md](design/commit-barrier.md) |
| Verification | [docs/design/verify.md](design/verify.md) |
| Restore points | [docs/design/restore-points.md](design/restore-points.md) |
| Benchmark and regression harness | [contrib/bench/README.md](../contrib/bench/README.md) |
| Grafana dashboards | [contrib/grafana/README.md](../contrib/grafana/README.md) |
