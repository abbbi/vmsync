# vmsync-agent

The per-hypervisor half of vmsync's control plane. It inventories the
domains on the host it runs on, assesses their replication health, and
reports that to a control-plane UI.

## What it does, and what it deliberately cannot

This is **phase 1 of the control plane, and it is read-only by
construction**. There is no code path in this binary that starts a sync,
changes a replication role, or modifies a domain in any way. The
configuration the UI hands out carries no executable instruction to ignore
— it holds a report interval, a poll hold time, and per-VM sync cadences
used only to judge staleness. Scheduling and operations arrive in later
phases.

That is what makes it safe to install on production hypervisors before the
rest of the control plane exists.

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

There is deliberately **no option to skip TLS verification.** The agent
holds a credential and reports the estate's replication topology; an
`--insecure` flag would be the first thing reached for during a certificate
problem and the last thing anyone removed afterwards.

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
