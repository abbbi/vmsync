# vmsync-agent

The per-hypervisor half of vmsync's control plane. It inventories the
domains on the host it runs on, assesses their replication health, and
reports that to a control-plane UI.

## What it does, and what it deliberately cannot

The agent inventories the local host's domains, assesses their replication
health, reports that to the control-plane UI, and **runs the syncs the
schedule names**.

What it still cannot do is act on a domain directly. It changes no
replication role and touches no VM; the only thing it does to a hypervisor
is run `vmsync`, which enforces its own refusals underneath — a target
marked `promoted` or `paused` is refused no matter what any schedule says.

The schedule is **typed data, never a command line**. The agent owns the
flag vocabulary, validates every field before building anything, supplies
SSH credentials from its own local flags, and executes with no shell — so
there is nothing for a compromised UI to inject. The pairing comes from
each VM's own `replica_targets` metadata rather than from anything the UI
sends, so a UI cannot redirect a sync at a host of its choosing.

Install with `--no-schedule` to keep a host reporting-only until you are
ready for it to run syncs, or with `--standalone` to run the scheduler with
no control plane at all.

The agent **dials out and never listens**, so a hypervisor needs no inbound
port. This matters for a DR site behind its own firewall, which is vmsync's
normal topology.

It **caches the UI's configuration on disk and keeps running from that
cache when the UI is unreachable**. The UI lives at the DR site and a WAN
partition is an ordinary event; an agent that stopped working without its
UI would make the control plane a single point of failure for the thing it
exists to protect.

## Install

```bash
install -m 0755 vmsync-agent /usr/local/bin/vmsync-agent
install -m 0644 contrib/systemd/vmsync-agent.service /etc/systemd/system/
mkdir -p /etc/vmsync
```

`/etc/vmsync/agent.env`:

```sh
VMSYNC_UI=https://vmsync-ui.dr.example.org
# Only needed when the UI uses a private or self-signed CA.
VMSYNC_UI_CA=/etc/vmsync/ui-ca.pem
```

## Enrol

Generate a single-use enrolment token in the UI for this host, then run the
agent once by hand:

```bash
vmsync-agent --ui https://vmsync-ui.dr.example.org \
             --enrol-token PASTE_TOKEN_HERE \
             --once
```

That exchanges the token for a long-lived credential in
`/var/lib/vmsync-agent/credentials.json` (mode 0600), sends one report, and
exits. The token is spent by that call and can be discarded — it is
worthless afterwards, which is why it is safe to move it around by whatever
means you have to hand.

Then start the service for real:

```bash
systemctl enable --now vmsync-agent
```

`--once` is also the way to check a running install: it reports and exits
without touching the service.

## Flags

| flag | meaning |
| --- | --- |
| `--ui` | Control-plane UI base address. Must be `https://`. |
| `--enrol-token` | Single-use token from the UI. Only needed until enrolment succeeds. |
| `--ui-ca` | PEM bundle to verify the UI's certificate against, for a private CA. |
| `--state-dir` | Where the credential and config cache live. Default `/var/lib/vmsync-agent`. |
| `--libvirt-uri` | Default `qemu:///system`. You should not need to change this — the agent reports the host it runs on. |
| `--hostname` | Name to report as. Defaults to the system hostname. |
| `--http-timeout` | Per-request timeout. **Must exceed the UI's long-poll hold time**, or every config poll ends in a client-side timeout. Default 2m. |
| `--once` | Report once and exit. |
| `--standalone` | Run as a scheduler only, from this JSON file, with no control plane at all. See below. |
| `--max-concurrent-syncs` | This host's ceiling on parallel vmsync jobs. Can only lower what the schedule asks for, never raise it. 0 (default) leaves it to the schedule. |
| `--prometheus-dir` | When set, each scheduled sync gets `--prometheus-textfile <dir>/vmsync_<vm>.prom`. |

## Logging, metrics and parallelism

**Logging** goes to stderr, one line per event, via the same `pkg/trace`
vmsync itself uses — so under systemd it lands in the journal with no file
to rotate:

```bash
journalctl -u vmsync-agent -f
```

`--debug` adds the per-tick scheduling decisions and the full argv of every
vmsync it runs. When a scheduled sync fails, the agent logs the exit code
**and vmsync's own output tail**, so the journal on this host explains the
failure without needing the UI.

**Prometheus.** Pass `--prometheus-dir`; the agent then hands each run
`--prometheus-textfile <dir>/vmsync_<vm>.prom`, one file per VM, which is
what node_exporter's textfile collector expects:

```bash
vmsync-agent --standalone /etc/vmsync-agent/schedule.json \
             --prometheus-dir /var/lib/node_exporter/textfile_collector
```

The agent also writes **its own** file, `vmsync-agent.prom`, describing the
scheduler rather than any one sync. (Hyphenated so a domain named `agent`
cannot collide with it.)

| metric | why |
| --- | --- |
| `vmsync_agent_scheduled_vms` | VMs configured here, and `_disabled` for entries present but not runnable. |
| `vmsync_agent_skipped_runs_total{reason}` | Due syncs that did not start: `host_concurrency`, `target_budget`, `already_running`, `invalid_profile`, `no_target`. |
| `vmsync_agent_ui_last_contact_timestamp_seconds` | Last successful exchange with the UI. 0 when there has never been one. |
| `vmsync_agent_config_age_seconds` | Age of the schedule actually in force. −1 if never fetched. |
| `vmsync_agent_syncs_running` / `_max_concurrent_syncs` | Current parallelism against the effective ceiling. |
| `vmsync_agent_sync_runs_total{result}` | Runs finished, success and failure. |
| `vmsync_agent_last_attempt_timestamp_seconds{vm}` | When a sync was last *started* for each VM. |
| `vmsync_agent_next_run_timestamp_seconds{vm}` | When each VM is next due. |
| `vmsync_agent_domains_total` / `_domains{status}` | Host inventory by assessed status. |
| `vmsync_agent_build_info{version}`, `_start_timestamp_seconds`, `_standalone` | Version, restart detection, whether a control plane is involved. |

These exist because the per-VM files cannot answer *"is anything actually
replicating?"*. A VM whose profile never validates, or that never wins a
concurrency slot, never runs vmsync at all — so it never writes a per-VM
file that could go stale and reveal it. The gap between
`next_run_timestamp` and `last_attempt_timestamp`, and any movement in
`skipped_runs_total`, is where that shows up.

Two subtleties worth knowing when alerting on them. Every `reason` series
exists from the first write at zero, so `increase()` over a window works
from the start rather than only after the first occurrence. And
`already_running` counts a slot the interval said a VM should have had —
not one tick of a long sync — so a sync that takes longer than its interval
increments it once per missed slot, which is the number you want.

`ui_last_contact` and `config_age` are emitted in standalone mode too, as
explicit zeros: a missing series and a UI that has never answered look
identical to a query, and one is a design choice while the other is an
outage.

The agent writes nothing per-VM itself — vmsync does, exactly as it does
when run from cron, so an existing dashboard keeps working unchanged.
Nothing at all is written when the flag is unset.

**Parallelism.** Each due VM runs in its own goroutine, one `vmsync`
process each, admitted against two independent limits:

| limit | set by | meaning |
| --- | --- | --- |
| `max_concurrent_syncs` | schedule (UI setting, or the standalone file) | jobs at once on this host. Default **4**. |
| `--max-concurrent-syncs` | this agent | host-local **ceiling**; only lowers the above. |
| `target_host_budget` | schedule | jobs at once *into* a given target host. |
| hard clamp | built in | 128, whatever anything asks for -- a backstop against a mistyped value, not a recommendation. |

`--max-concurrent-syncs` deliberately only lowers. How much concurrent I/O
a hypervisor can absorb depends on its disks, its NICs and what else it is
running — the host knows that and a control plane does not, so the UI can
ask for less than the host allows but never more.

The per-target-host budget is the one limit only a UI can compute: an agent
cannot see that four *other* hosts are also replicating into the same
target.

A VM refused admission is not deferred — it retries on the next tick, as
soon as a slot frees. A VM whose previous run is still going is skipped
rather than overlapped, and runs are staggered so a fleet that all wants
`interval_seconds: 900` does not fire on the same second.

There is deliberately **no option to skip TLS verification.** The agent
holds a credential and reports the estate's replication topology; an
`--insecure` flag would be the first thing reached for during a certificate
problem and the last thing anyone removed afterwards.

## Operations: one-shot instructions

Alongside the schedule, the UI can publish **operations** — a promotion, an
inversion, a clean shutdown, a role change. They travel in the same document
but are a different kind of thing, and the agent treats them differently at
every step.

The schedule is *desired state*: re-delivering it is harmless, which is what
lets an agent keep running it through a partition. An operation is an
*event*. Delivering it twice must not do it twice.

| rule | why |
| --- | --- |
| Executed only from a config received over the wire in this process | The on-disk cache is replayed on restart. An operation replayed from disk is a failover from an instruction nobody re-issued. `LoadCache` strips them, so this is structural rather than a convention. |
| Intent recorded **before** the work, in `operations.json` | Recording completion afterwards leaves the whole duration of a shutdown with no trace on disk; a crash there loses all knowledge of a half-performed failover. |
| A record in **any** state refuses re-execution | Including `failed`. Re-running a failover that already went wrong, unattended, compounds it. A person re-issues with a fresh ID. |
| `running` found at startup becomes `unknown`, never retried | The agent died mid-operation. What state the domain reached is something only an inspection establishes. |
| Results re-sent on **every** report until the UI stops publishing | A result sent once is lost if that report doesn't land, leaving the UI publishing forever and the agent skipping forever — both halves working, jointly stuck. |
| Hard expiry (`not_after_unix`) | A target host that was down when a promote was issued must not execute it days later, as a *first* delivery no replay guard covers. |
| The UI's peer is checked against local metadata, never used as an endpoint | Everywhere else the far end comes from the VM's own `replica_source`/`replica_targets`. This is the one channel that can stop a production VM. |

Operations run **one at a time**, and are **not** disabled by
`--no-schedule`. That flag means "do not run the schedule" — and a DR target
host is both the machine most likely to carry it and the machine a failover
must run on. Tying the two together would deliver a promotion to a visibly
healthy agent that silently ignores it.

Every outcome is reported, including refusals. An operator watching a
failover sit "pending" against a healthy agent, with nothing saying why, is
the worst thing this could do.

## Standalone: a scheduler with no control plane

`--standalone /path/to/schedule.json` runs the scheduler and nothing else.
No enrolment, no credential, no reporting, no polling — the agent never
opens a network connection to anything but the hypervisors it syncs to.

This is not a reduced mode bolted on. The scheduler always ran from a cached
configuration and always kept running while the UI was unreachable — that is
the partition-tolerance the control plane is built on. The only thing that
ever required a UI was the startup path, and `--standalone` skips it.

What you get over a cron job: a real interval per VM, a cap on how many
syncs run at once, a per-target-host budget so several sources cannot
stampede one target, staggering so everything does not fire on the same
minute, skip-if-still-running instead of overlapping runs, and each run's
outcome in the journal. An agent installed this way can be enrolled with a
UI later without changing anything about how it runs syncs.

```bash
vmsync-agent --standalone /etc/vmsync-agent/schedule.json
```

It cannot be combined with `--ui`, `--enrol-token`, `--ui-ca`, `--once` or
`--no-schedule`. Those are refused rather than ignored: silently accepting
them would leave you believing this host reports somewhere, which is exactly
the belief that gets a host forgotten.

### The file

The same object the UI would otherwise send. Everything except `schedule` is
optional.

```json
{
  "schedule": [
    {
      "vm": "web01",
      "interval_seconds": 900,
      "enabled": true,
      "target_host": "dr01",
      "profile": {
        "compress": "zstd",
        "compress_level": "5",
        "netbuffer": "128k,1G",
        "io_depth": 16,
        "verify": "online",
        "target_disk_path": "/data/replicas"
      }
    }
  ],
  "max_concurrent_syncs": 4,
  "target_host_budget": { "dr01": 2 }
}
```

| field | meaning |
| --- | --- |
| `vm` | Source domain on **this** host. Its targets are read from its own libvirt metadata, never from this file. |
| `interval_seconds` | How often to sync. Must be greater than 0. |
| `enabled` | `false` keeps an entry visible while stopping it from running. |
| `target_host` | Required only when the VM replicates to more than one target; otherwise the single target is used. |
| `profile` | vmsync settings for this VM: `compress`, `compress_level`, `netbuffer`, `use_ssh`, `io_depth`, `verify`, `reinit_after_failures`, `target_disk_path`, `source_port_range`, `target_port_range`. Omit any of them for vmsync's default. |
| `max_concurrent_syncs` | Cap on syncs running at once on this host. Default 4. |
| `target_host_budget` | Cap on concurrent syncs *into* a given target host. |

Note what is **not** in the file: no hostnames to replicate to, no
credentials, no command line. The pairing comes from each VM's own
`replica_targets` metadata, and SSH details come from the agent's own flags
(`--ssh-user`, `--ssh-key`, …). This file says *when*, not *where* or *how
to log in*.

Parsing is strict — an unrecognised key is an error naming the key, not a
silently ignored line. A misspelling that is quietly dropped does not look
like a mistake, it looks like the scheduler not working.

Changes take effect on restart; the file is read once at startup.

```bash
systemctl restart vmsync-agent
```

### It still says *when*, not *whether*

`--standalone` schedules syncs. It does not weaken any of vmsync's own
refusals: a target marked `promoted` or `paused` is still refused, the
target-side run lock still excludes concurrent operations, and a role
changed mid-sync still aborts the run before it redefines the domain. A
schedule cannot talk vmsync into overwriting something it would otherwise
protect.

### When you need `--ui-ca`, and when you don't

**If the UI has a publicly-trusted certificate, you don't.** Leave the flag
unset and the agent uses the host's system trust store. Renewing that
certificate changes the leaf, which validates against roots the host
already has — there is nothing to redistribute to agents, ever.

`--ui-ca` is only for:

- **A private/internal CA.** You distribute the **CA certificate**, not the
  leaf. Internal CAs are usually valid for years, so it is copied once and
  the leaf underneath rotates freely without touching any agent.
- **A bare self-signed certificate**, which is its own issuer. This is the
  one arrangement where every renewal means visiting every host. Prefer a
  small private CA, which turns that into a one-time copy.

Note that setting `--ui-ca` **replaces** the system trust store rather than
adding to it — only that CA is trusted for the UI. If you later move to a
publicly-trusted certificate, unset the flag; leaving it set fails with a
plain certificate error rather than anything subtle.

## What it reports

Every domain libvirt knows about, including inactive ones — a replication
target is *supposed* to be shut off, so listing only running domains would
hide every target in the estate. Domains with no vmsync metadata are
reported too, as `unreplicated`: "nobody configured replication for this"
and "this is protected" must never look alike.

Each domain gets a status and the reasons behind it:

| status | meaning |
| --- | --- |
| `ok` | Replicating, recent, no failures. |
| `unreplicated` | vmsync has no relationship with this domain. |
| `paused` / `promoted` | Administrative states, not faults. |
| `warning` | Degraded but still replicating — failures recorded, past its cadence, or no checkpoint to sync incrementally against. |
| `critical` | Never synced, or far past its cadence. |

Two rules worth knowing when reading the output:

- **`promoted` and `paused` suppress the staleness checks.** Those domains
  are supposed to stop receiving syncs, so a growing age is expected rather
  than a fault — reporting it would bury the real signal for exactly the
  VMs you are watching most closely.
- **A domain with no configured cadence is not judged on freshness at
  all.** Guessing a threshold would report a pair that legitimately syncs
  weekly as critical forever.

## The UI-facing API

The UI is a separate program, so the HTTP surface between them is a
contract between two codebases:

```
POST {base}/api/v1/agents/enrol             -> {"agent_id","token"}
POST {base}/api/v1/agents/{id}/report       bearer; inventory upload
GET  {base}/api/v1/agents/{id}/config       bearer; long-poll, ETag-aware
```

`client_test.go` drives a stub UI over TLS and pins every one of these —
the paths, the bearer header, the `If-None-Match`/`ETag` exchange, the
`304` and `401`/`403` semantics. **It is the executable specification for
the UI**, and is more reliable than this table, which can drift.

Two behaviours the UI must get right:

- **`401`/`403` means revoked.** The agent treats it as terminal rather
  than retryable and says so loudly, because it never succeeds again until
  an operator issues a fresh enrolment token.
- **Long-poll honestly.** The agent sends `wait=<seconds>` and expects the
  UI to hold the request open that long before answering `304`. That is
  what delivers an operator's change in seconds without the hypervisor
  accepting inbound connections.

## Troubleshooting

**"this agent has not enrolled yet and no `--enrol-token` was given"** —
first run on this host; generate a token in the UI.

**"stored credential was issued by X but `--ui` is Y"** — the agent is
being repointed at a different UI. Enrol again with a token from the new
one, or remove `credentials.json` to start over. It refuses rather than
presenting a token the new UI has never seen, which would otherwise show up
as an unexplained stream of 401s.

**Polls always time out** — `--http-timeout` is shorter than the UI's
long-poll hold time.

**The UI shows an agent as stale** — check `config_age_seconds` in its
reports. The agent keeps working from its cache during a partition; that
field is how the UI shows it is running on old instructions.
