# vmsync-agent

The per-hypervisor half of vmsync's control plane. It inventories the domains
on the host it runs on, assesses their replication health, reports that to a
control-plane UI, and **runs the syncs the schedule names**.

It runs in one of three modes, named on the command line and never inferred:
`--standalone` (schedule from a local file, no control plane), `--monitor`
(report to a control plane, run nothing — for a host whose replication is driven
by cron) or `--controlled` (the control plane holds the schedule and this agent
runs it). See [Flags](#flags); exactly one is required.

## What it does, and what it deliberately cannot

What it cannot do is act on a domain directly. It changes no replication role
and touches no VM; the only thing it does to a hypervisor is run `vmsync`,
which enforces its own refusals underneath — a target marked `promoted` or
`paused` is refused no matter what any schedule says.

The schedule is **typed data, never a command line**. The agent owns the flag
vocabulary, validates every field before building anything, supplies SSH
credentials from its own local file, and executes with no shell — so there is
nothing for a compromised UI to inject. The pairing comes from each VM's own
`replica_targets` metadata rather than from anything the UI sends, so a UI
cannot redirect a sync at a host of its choosing.

The agent **dials out and never listens**, so a hypervisor needs no inbound
port. This matters for a DR site behind its own firewall, which is vmsync's
normal topology.

It **caches the UI's configuration on disk and keeps running from that cache
when the UI is unreachable**. The UI lives at the DR site and a WAN partition
is an ordinary event; an agent that stopped working without its UI would make
the control plane a single point of failure for the thing it exists to protect.

## Two files, two owners

Everything that used to be a flag now lives in JSON, split across two documents
by **who is allowed to write them**.

| | file | written by | contains |
| --- | --- | --- | --- |
| 1 | `/etc/vmsync/agent.json` | you, or config management. The agent never writes it, and nothing in it ever arrives over the network. | which binary runs, as which SSH identity, against which libvirt, where state lives |
| 2 | the schedule | the control plane, cached by the agent to `<state_dir>/config-cache.json`; or you, in standalone mode | which VMs sync, how often, with what transport settings |

The split is the security boundary. `ssh.key`, `vmsync_path` and `libvirt_uri`
together decide which binary runs as root with which credentials against which
host, so a control plane that could set them would not need any other
vulnerability. None of them is reachable from the network.

**The mode is not a field in either file.** It is a command-line flag — see
[Flags](#flags) — and the file only has to be *consistent* with it. That keeps
the original property intact: mode is not something a compromised UI can grant
itself, because the control plane writes neither the unit nor the environment
file. It also puts it where `systemctl cat` shows it, instead of requiring you
to read a JSON document and know a rule about it.

A `"mode": "standalone"` **string** in file 1 would still be the wrong shape for
a different reason: it could contradict a populated `control_plane` block, and
then something has to decide which wins. The flag cannot contradict the file
silently — a mismatch is refused at startup and on every reload, by name.

`"control_plane": null` is still refused outright rather than read as "no
control plane" — it decodes to a nil pointer, indistinguishable from the key
being absent, and a `--controlled` agent would then fail with *"nothing to
enrol with"* over a JSON literal that looks deliberate.

Both files carry `"config_version": 1`, checked in a pre-pass before anything
else is parsed, so an agent that is too old for a file says exactly that
instead of blaming whichever key happens to be new. (A strict decoder returns
on the *first* unknown field, so without the pre-pass a version-2 file would be
reported as `unknown field "…"` naming some innocent key.)

### Strictness, and why the two documents differ

| document | reader | unknown keys |
| --- | --- | --- |
| `agent.json` | the agent, at startup and on every reload | **strict** |
| a hand-written schedule (`schedule_file`) | the agent, once at startup | **strict** |
| `<state_dir>/config-cache.json` | the agent, at startup | lenient |
| the document the UI sends | the agent, on every poll | lenient, and no `config_version` check at all |

Strictness is a property of the reader, not of the type: the same document type
is decoded all four ways.

A **person** wrote the first two, and a misspelled key that silently keeps its
default is the failure mode a config file has and a flag does not — an unknown
flag was always an error, and moving to a file must not be a regression on
that. A misspelling that is quietly dropped does not look like a mistake; it
looks like the scheduler not working.

The **cache** is only ever written by this same binary, so an unknown key there
means a downgrade, and refusing to read it would strand a host with no schedule
during exactly the partition the cache exists for. The **UI's** document comes
from a separately-versioned program: refusing it outright would let a newer UI
take an estate offline over one out-of-range field, so its values are
normalised, clamped and complained about, never rejected.

## Install

```bash
install -m 0755 vmsync-agent /usr/local/bin/vmsync-agent
install -m 0644 contrib/systemd/vmsync-agent.service \
        /etc/systemd/system/vmsync-agent.service
install -m 0644 contrib/systemd/vmsync-agent.sysconfig \
        /etc/sysconfig/vmsync-agent        # /etc/default/ on Debian
mkdir -p /etc/vmsync
$EDITOR /etc/sysconfig/vmsync-agent        # set VMSYNC_AGENT_MODE
systemctl daemon-reload
```

`contrib/systemd/` ships **one** unit for all three modes. It used to ship two,
one per mode, identical apart from a single word in `ExecStart` — around fifty
lines of hardening and restart policy duplicated so one argument could differ.
Adding monitor would have made three copies, and the first hardening fix applied
to one and forgotten in the others is the drift nobody notices until it is the
host it mattered on that was missed.

The mode comes from `VMSYNC_AGENT_MODE` in the environment file. That file
carries what the **command line** carries — which agent this is, and what one
invocation is for — and nothing else: every setting stays in
`/etc/vmsync/agent.json`, where a reload can change it without restarting the
process. `/etc/vmsync/agent.env` is still gone, and with it the hazard that
anything in the unit's environment was inherited by every vmsync the agent
spawned.

`EnvironmentFile=-` (with the dash) means a missing file is not a unit failure.
That is deliberate rather than lenient: with no mode set the agent starts, finds
none, and refuses with a message naming all three, which is far more useful in
the journal than systemd's *"Failed to load environment files"*.

`ExecReload=/bin/kill -HUP $MAINPID` re-reads `agent.json` without interrupting
a sync in flight. The mode is the one thing a reload cannot change — it decides
which goroutines exist, and a SIGHUP cannot retract a goroutine — so changing
`VMSYNC_AGENT_MODE` needs `systemctl daemon-reload && systemctl restart`.

`/etc/vmsync/agent.json`, for a host with a control plane:

```json
{
  "config_version": 1,

  "ssh": { "user": "root", "key": "/etc/vmsync/id_ed25519" },

  "control_plane": {
    "url": "https://vmsync-ui.dr.example.org",
    "ca_file": "/etc/vmsync/ui-ca.pem"
  }
}
```

That is a complete file; everything else has a default. Strictly speaking even
`ssh` is optional, but a host that runs syncs needs it — vmsync dials SSH to
the target hypervisor and there is no agent socket under systemd to fall back
on.

Own the file root and keep it out of group- and world-write. The agent warns on
startup and on every accepted reload if it is writable by anyone else, because
it decides which binary this agent runs as root.

`StateDirectory=vmsync-agent` in the shipped units creates `/var/lib/vmsync-agent`
mode 0700. If you point `state_dir` somewhere else the agent creates it itself,
also 0700 — but see the note on `ProtectSystem=strict` below.

### The hardened unit and writable paths

Both units set `ProtectSystem=strict`, under which the only writable path a
unit and its children get is its `StateDirectory`. Two things need granting on
a host that runs syncs:

- **`/run/vmsync-locks`.** vmsync takes its run locks there and creates the
  directory itself, which a strict unit cannot do.
- **`prometheus_dir`, and any `state_dir` outside `/var/lib/vmsync-agent`.**

```bash
systemctl edit vmsync-agent
```

```ini
[Service]
RuntimeDirectory=vmsync-locks
RuntimeDirectoryMode=0755
RuntimeDirectoryPreserve=yes
# Only if prometheus_dir is set, or state_dir was moved.
ReadWritePaths=/var/lib/node_exporter/textfile_collector
```

`RuntimeDirectoryPreserve=yes` is not decoration: without it systemd removes
the lock directory when the unit stops, and something clearing that directory
out from under a held lock is precisely the hazard the locking code is written
to detect and work around.

## Enrol

Generate a single-use enrolment token in the UI for this host, put it in a
file, and run the agent once by hand:

```bash
umask 077 && printf '%s' 'PASTE_TOKEN_HERE' > /run/vmsync-enrol-token
vmsync-agent --config /etc/vmsync/agent.json \
             --enrol-token-file /run/vmsync-enrol-token \
             --once
```

A file, not a flag value: `/proc/<pid>/cmdline` is world-readable and shell
history keeps a pasted token indefinitely.

That exchanges the token for a long-lived credential in
`/var/lib/vmsync-agent/credentials.json` (mode 0600), sends one report, and
exits. **The token file is deleted the moment it is read**, before enrolment is
even attempted — the token is spent by that call and a copy left on disk is a
credential-shaped thing that is no longer a credential, and at worst something
config management re-deploys forever because it looks like configuration. If
that run fails for any reason, generate a fresh token; the file is gone.

A missing token file is not an error. It is the ordinary state of an enrolled
host whose unit still names one. An *empty* one is refused, and so is
`--enrol-token-file` given to an agent whose file has no `control_plane`: there
is nothing to enrol with, and accepting it silently would leave you believing
this host reports somewhere.

A failure to save the credential is fatal, deliberately: an agent running on an
unpersisted credential works until the next restart and then needs a token
nobody knows is required.

Then start the service for real:

```bash
systemctl enable --now vmsync-agent
journalctl -u vmsync-agent -f
```

`--once` is also the way to check a running install: it reports and exits
without touching the service. It needs a control plane; on a standalone agent
there is nowhere to report to, and it is **ignored rather than refused** — the
process runs as a daemon.

A reload never re-reads or re-deletes the token file, and never re-enrols.

## Flags

None of them is a *setting*: they select a file, they say what this one
invocation is for, or they say **which agent this is**. Go's `flag` accepts
`-x` and `--x` identically.

| flag | meaning |
| --- | --- |
| `--standalone` | Mode: run the schedule from a local file, with no control plane. Requires `schedule_file`. |
| `--monitor` | Mode: report to a control plane and run nothing. Requires `control_plane`. |
| `--controlled` | Mode: take the schedule from the control plane and run it, execute its operations, fence on its behalf. Requires `control_plane`. |
| `--config` | Path to file 1. Default `/etc/vmsync/agent.json`. |
| `--once` | Report once and exit, instead of running as a daemon. For verifying a new install. |
| `--debug` | Force debug logging on, whatever `log.debug` says, until this agent is restarted. |
| `--enrol-token-file` | Path to a file holding a single-use enrolment token. Read once and then **deleted**. Only needed until enrolment succeeds. |
| `-v`, `--version` | Print the version and exit. |

**Exactly one mode flag is required, and there is no default.** The mode used to
be inferred from the file — a `schedule_file` meant standalone, a
`control_plane` meant control-plane — which worked while there were two modes
and they needed different files, but it made the most consequential property of
a host (whether a network service can start and stop things on it) something you
established by reading a JSON document and knowing a rule about it. The only
plausible default would be `--controlled`, which is the mode where the control
plane can fail this host over and fence its VMs; a default is how a host ends up
in a mode nobody chose for it.

The mode is also the one piece of configuration that genuinely cannot be
reloaded: it decides which goroutines exist, and a SIGHUP cannot retract a
goroutine. Being a flag rather than a file key removes a whole class of reload
the agent would otherwise have had to detect and refuse.

The flag and the file are checked against each other at startup **and on every
reload** — `--controlled` with no `control_plane` has nothing to be controlled
by, `--standalone` with no `schedule_file` has nothing to run, and each is a
host that starts, logs nothing alarming and replicates nothing.

The agent takes no positional arguments; a leftover one means a flag was
mistyped and silently dropped, so it is refused (exit 2) rather than ignored.

### The three modes

| | `--standalone` | `--monitor` | `--controlled` |
| --- | --- | --- | --- |
| schedule from | local `schedule_file` | *nothing — cron owns it* | the control plane |
| enrols with a UI | no | **yes** | yes |
| reports to a UI | no | yes | yes |
| polls the UI | no | yes, inbound only | yes |
| runs syncs | yes | **no** | yes |
| executes operations | no | **no** | yes |
| fences | yes | **no** | yes |

**`--monitor` reports, and does not act.** It is for a host whose replication is
driven by something else — cron, usually — where the point is to make what cron
actually did visible in the console instead of leaving a whole site invisible.
It still enrols, because a host that reports to a control plane needs an
identity there.

It still *polls*, which is not the contradiction it looks like. The poll is
inbound-only: it fetches configuration and caches it, and it is what carries the
expected cadence — without which every report would say *"no configured
cadence"* and the console could never show a cron-driven VM as overdue, which is
most of the reason to watch one. What makes the mode read-only is not a refusal
to receive configuration, it is that **the goroutines which could act on it are
never started**.

Two things follow that are worth stating plainly:

- **A monitored host has no split-brain protection from this agent.** Fencing is
  the only thing here that stops a running VM, and a mode whose contract is
  *"this host does not act"* cannot carry it — otherwise adding `--monitor` to
  watch a cron-driven site would bring unattended VM shutdowns nobody asked for,
  since `features.autofence` is on by default. If you want fencing without
  scheduling, use `--controlled` with `"features": {"schedule": false}`, which
  is a supported combination and does exactly that.
- **`features.schedule` and `features.autofence` are ignored in monitor mode.**
  Not refused — ignored, and said so in the journal at startup. They cannot be
  refused honestly: both default to `true` before the file is fully loaded, so a
  file that never mentioned either is indistinguishable from one that asked for
  both.

### Changing a host's mode

Always the same two mechanical steps — **edit `VMSYNC_AGENT_MODE`, then
`systemctl restart`**. Never `reload`: the mode decides which goroutines exist
and a SIGHUP cannot retract a goroutine, so a reload will not change it. If the
new mode needs a different `agent.json` shape, edit that too, or the agent
refuses to start and names the mismatch.

Nothing clears `state_dir`, and nothing should. It holds the enrolment
credential, the operation ledger and the **fence ledger** — and that last one is
what makes a fence single-use, so wiping it would let the agent perform a second
unattended shutdown of a VM it had already fenced.

| from → to | `agent.json` | other steps |
| --- | --- | --- |
| standalone → controlled | drop `schedule_file`, add `control_plane` | enrol (`--enrol-token-file`) |
| standalone → monitor | drop `schedule_file`, add `control_plane` | enrol |
| monitor → controlled | *unchanged* | — |
| controlled → monitor | *unchanged* | cancel pending operations first |
| controlled → standalone | drop `control_plane`, add `schedule_file` | write the schedule file; revoke in the UI |
| monitor → standalone | drop `control_plane`, add `schedule_file` | write the schedule file; revoke in the UI |

Four of them have a sharp edge, and in each case it is the same shape: the
host keeps running and quietly does something other than what you assumed.

**standalone → controlled: the host can stop replicating the moment you
restart.** The local schedule is no longer read, and the control plane's
schedule for a newly enrolled agent is empty. Unless a `default` template covers
the VMs, nothing runs. Create the entries in the console **before** the restart,
and keep the old `schedule_file` — nothing deletes it, and it is the record of
what the cadences used to be.

**monitor → controlled: the host starts running whatever the console holds, at
once.** If that host was `--controlled` months ago, its old entries are still
there and it resumes them on the first tick. Look at the schedule page for that
host before you flip it, not after.

**controlled → monitor: operations already published will never execute.** The
agent runs no operations loop, so anything pending sits in the console forever
looking merely slow. Cancel them first. The agent also stops fencing, so that
host loses split-brain protection — which is the deliberate contract of the
mode, but it is a real capability to give up knowingly. Its results ledger is
not loaded either, so any result it had not yet had acknowledged is dropped
rather than republished.

**controlled → standalone: you write the schedule file by hand.**
`config-cache.json` holds what the console last published, but it is not a
schedule file — it wraps the document as `{etag, fetched_at_unix, config}`, and
`LoadScheduleFile` decodes strictly and rejects it. Read the cadences off the
console (or out of the cache) and write the file yourself. Afterwards **revoke
the agent in the UI**, or the console shows a host that is never coming back as
permanently stale.

`--debug` is applied *before* the config file is read, so a file that fails to
load can be diagnosed with the flag that exists for diagnosing it. It can only
turn debug **on**: it survives every reload, a reload cannot switch it off, and
the agent says so once at startup when `log.debug` disagrees. Without the flag,
`log.debug` is fully live in both directions.

## `agent.json` reference

Every key is optional except `config_version`, and except that a file must have
either `control_plane` or `schedule_file`. Parsing is strict: an unrecognised
key is an error naming the key.

| key | default | meaning |
| --- | --- | --- |
| `config_version` | — | Must be `1`. Required. |
| `hostname` | the system hostname | Name this agent reports under and is identified by in replication metadata. May not contain a comma, slash or whitespace — replica metadata is a comma-separated list of `host:domain` entries, and a comma splits one entry into two, so it never matches again, a fresh one is appended every sync, and the VM eventually looks like it replicates to a dozen targets until picking one is refused. A colon is fine. The system fallback is validated by the same rule. |
| `libvirt_uri` | `qemu:///system` | Which hypervisor to inventory and sync from. You should not normally change this; the agent reports the host it runs on. If it names a remote host, the agent warns at startup that it reports under one name and appears in replication metadata under another. |
| `state_dir` | `/var/lib/vmsync-agent` | Credential, schedule cache, both ledgers, run log. **Cannot be changed on a running agent.** |
| `schedule_file` | — | **Standalone only**, and required there. Path to file 2. Cannot be set together with `control_plane`. |
| `vmsync_path` | `/usr/local/bin/vmsync` | The binary every sync, operation and fence executes. Warned about if group- or world-writable. |
| `bridge_helper_path` | — | Passed to vmsync as `-bridge-helper-path`. |
| `target_runtime_dir` | — (vmsync's own default, `/run/vmsync`) | Passed to vmsync as `-target-runtime-dir`: where the **target-side** qemu-nbd exports put their sockets and pidfiles. Leave it unset unless that path is unsuitable on the far host. Never point it at a directory the target polyinstantiates per SSH session — SELinux with `pam_namespace` does that to `/tmp` and `/var/tmp`, and an export started by one command is then invisible to the next, so the helper cannot find the socket and an abandoned export can never be reclaimed. |
| `target_uri_pattern` | `qemu+ssh://%s/system` | How a target host's name becomes a libvirt URI. Must take exactly one `%s`; it is trial-rendered at load, which catches zero verbs, two verbs and a stray `%d` alike while still accepting a legitimate `%%s`. |
| `prometheus_dir` | — | When set, the agent writes `vmsync-agent.prom` here and hands each sync `-prometheus-textfile <dir>/vmsync_<vm>.prom`. The directory must already exist and be writable; the agent does not create it. |
| `ssh.user` | — | |
| `ssh.key` | — | A **path**, never key material — that is what keeps this file secret-free and safe to manage from a repository. Warned about if readable by anyone but its owner. |
| `ssh.port` | `0` | 0 leaves vmsync's own default. Refused outside 0–65535. |
| `ssh.known_hosts` | — | |
| `limits.max_concurrent_syncs` | `0` | This host's **ceiling** on parallel syncs. 0 expresses no opinion and leaves it to the schedule. Refused above 128 rather than clamped: a person typed this, and quietly running 128 when they asked for 5000 is not what they meant either way. It is also the multiplier on port usage — see below. |

Concurrency and ports interact, and this is the one place the relationship is
visible. Each concurrent sync binds up to four ports per disk on the target
(one for the export, plus one each for a bridge, a verify export and the
verify export's bridge, as `-compress`/`-verify` are enabled), so the ceiling
on parallel syncs multiplies straight into how much of the target port range
is in use.

Nothing needs configuring for this to work. The agent does **not** share a
port allocator between the vmsync processes it launches, because there is
nothing to share: each export binds its own port and a process that loses a
race is told no by the kernel and takes another. What the range has to be is
simply large enough — `max_concurrent_syncs` × disks × 4 in the worst case,
which at the default 200-port range is 12 four-disk VMs replicating at once
with everything enabled. Raising the ceiling far above that wants the range
widened with it; a run that cannot find a port says so, naming the range.
| `features.schedule` | `true` | `false` is the old `-no-schedule`: report, run no schedule. Operations and fencing still run. |
| `features.autofence` | `true` | `false` is the old `-no-autofence`: keep the split-brain check and its warnings, take no action. |
| `log.debug` | `false` | Per-tick scheduling decisions and the full argv of every vmsync launched. |
| `log.run_log_probes` | `false` | Accepted, and **does nothing today**: no code reads it. Listed because the strict decoder would otherwise reject an existing file that sets it. |
| `control_plane.url` | — | Required inside `control_plane`. Must be `https://`. |
| `control_plane.ca_file` | — | PEM bundle, **read and parsed at load**, so a missing or unusable CA fails at startup rather than at first contact. |
| `control_plane.http_timeout_sec` | `120` | Per-request timeout. Must be positive, and must exceed the UI's `poll_wait_seconds`, or every poll ends in a client-side timeout. |

`features` is stated positively on purpose — `"no_schedule": false` is a double
negative nobody reads correctly. Both defaults are `true`, and both are
pointers internally so that an absent key means "not mentioned" rather than
"turned off": a plain boolean would silently disable scheduling and autofencing
on every host whose file omits them.

Every path must be **absolute and canonical**, judged by POSIX rules whatever
the build host. Relative would resolve against whatever working directory
systemd handed the unit; non-canonical because `/var/lib/../lib/vmsync-agent`
and `/var/lib/vmsync-agent` are the same directory but not the same string, and
several things here compare paths. The error contains the canonical spelling.

There is deliberately **no option to skip TLS verification.** The agent holds a
credential and reports the estate's replication topology; an `--insecure` flag
would be the first thing reached for during a certificate problem and the last
thing anyone removed afterwards.

## Reload: changing settings without a restart

A daemon whose settings can only be changed by restarting it is one an operator
hesitates to touch during an incident — which is when they most need to.

**What triggers a reload.** Two things, funnelling through one serialised code
path:

- `SIGHUP` — `systemctl reload vmsync-agent`.
- A **content poll every 10 seconds**. Not a convenience on top of SIGHUP:
  making "settings can be changed without a restart" true only for operators
  who remember to signal makes it a ritual rather than a property.

The poll hashes the file's bytes with no `(size, mtime)` pre-filter. A gate
like that would defeat the hash it gates — a same-second, equal-length in-place
rewrite would be invisible, and your edit would silently never apply while the
log said nothing changed. The digest is committed only after the reload fully
succeeds, so a truncate-in-place editor is safe: the half-written file fails to
parse, and the next poll picks up the finished one with no second signal.

Registering SIGHUP matters beyond the feature: Go terminates on an unhandled
SIGHUP, so `systemctl reload` used to be `systemctl kill`, and the natural
fallback `systemctl kill -s HUP` signals the whole control group and would take
every in-flight vmsync down without its unwind path.

**What is re-read.** File 1, in full, and nothing else. The schedule is not
re-read: in control-plane mode it arrives from the poll loop, and in standalone
mode it is read once at startup.

**What a reload does not disturb.**

- Syncs, operations and fence sweeps in flight. Each takes one configuration
  snapshot when it starts and uses it throughout, so a sync that started under
  one `ssh.key` finishes under it — never generation N's argv with generation
  N+1's binary.
- The run log's open file and this process's session id.
- Every metric counter, and the metrics identity.
- The enrolled credential, both ledgers, the cached schedule.
- The scheduler's per-VM next-run times, in-flight set and per-target tallies.

Everything that can fail happens **before** anything is published, and the new
configuration is swapped in as a single pointer — so a half-applied generation,
with the new `ssh.key` beside the old `ssh.user`, is structurally impossible
rather than merely unlikely.

### What you see in the journal

Four outcomes, one line each.

**Unchanged.** Silent for the poll — it is answering "no" every ten seconds
forever — but never silent for an explicit signal, because an operator who
asked for a reload is owed an answer:

```
INFO reload: the configuration file is unchanged, nothing to do path=/etc/vmsync/agent.json
```

**Changed.** One summary line and one line per setting, at WARNING so it
survives a normal log filter:

```
WARNING reload: configuration changed cause=SIGHUP generation=3 changes=2
WARNING reload: ssh.key: "/etc/vmsync/id_ed25519" -> "/etc/vmsync/id_ed25519.new"
WARNING reload: limits.max_concurrent_syncs: 4 -> 2
```

`cause` is `SIGHUP` or `file changed`. The list is exhaustive by construction:
every field the comparison covers is a field a reload can change, and anything
missing from it would apply silently.

**Refused.** Nothing is adopted, the running configuration is untouched, and
the next poll retries by itself:

```
ERROR reload REFUSED; the running configuration is unchanged cause=file changed error=/etc/vmsync/agent.json: "ssh.port" 70000 is out of range; 0 leaves vmsync's own default
```

Repeated identical failures are suppressed, so editing a file over several
minutes does not produce the same error every ten seconds.

**Bytes changed but no setting did** — a comment, a reordering, whitespace.
Worth one line so you know the edit was seen and had no effect, rather than
wondering:

```
INFO reload: the file changed but no setting did cause=file changed generation=4
```

Hygiene warnings are re-emitted on every accepted reload:

```
WARNING configuration hygiene detail=/etc/vmsync/agent.json is mode 0664: it is group- or world-writable, and it decides which binary this agent runs as root
```

### Live, or restart

| setting | when it takes effect |
| --- | --- |
| `hostname` | Next report, next fence sweep, next sync's `-local-host-name`. |
| `libvirt_uri` | Next inventory scan, fence sweep or sync. |
| `vmsync_path` | Next launch of any kind. |
| `bridge_helper_path` | Next sync. |
| `target_uri_pattern` | Next sync, next fence probe. |
| `ssh.user` / `ssh.key` / `ssh.port` / `ssh.known_hosts` | Next sync. |
| `limits.max_concurrent_syncs` | Next scheduler tick (10s). |
| `features.autofence` | Next fence sweep (60s) — it is re-read per sweep. |
| `log.debug` | Immediately, unless `--debug` is pinning it on. |
| `prometheus_dir` | Next metrics write (15s) and next sync — **but only if it was already set at startup**. See below. |
| `state_dir` | **Refused.** Restart. |
| adding or removing `control_plane` | **Refused.** Restart. |
| `control_plane.url` / `ca_file` / `http_timeout_sec` | Restart. Reported as changed, but the HTTPS client is built once at startup and is not rebuilt. |
| `features.schedule` | Restart. The scheduler goroutine is started, or not, at startup. |
| `schedule_file` (one path to another) | Restart. Neither refused nor listed as a change, so editing it produces the "file changed but no setting did" line and nothing else. |

Two refusals, and they are refused **whole** — the rest of the file is not
applied either, because a generation that is neither the old file nor the new
one is something no operator can reason about.

- **`state_dir`** is not a config change at all. It is a move of five files
  other goroutines are writing right now: the credential, the schedule cache,
  both ledgers and the run log. The fence ledger is the sharp one — both
  ledgers snapshot under their mutex and write outside it, so a relocate
  interleaved with a fence's intent write can land that record in the directory
  nobody reads next, and after a restart the agent performs a second unattended
  shutdown of a production VM. Move it by hand with the agent stopped.
- **Mode** — it changes who is in charge of this host and which goroutines
  exist, and costing you a deliberate restart for that is the feature. Since the
  mode moved to the command line this is no longer reachable by editing
  `agent.json` at all; what a reload *can* still reach is a file that stops
  matching the running mode (deleting `control_plane` from a `--controlled`
  agent's file, say), and that is refused the same way.

Both increment `vmsync_agent_config_rejected_total`, which is its own series
precisely because the remedy is a restart rather than another edit: watching
only the generation gauge, you would see it stop advancing with no idea why.

Two more things frozen at startup and worth knowing:

- **Whether metrics are emitted at all.** The metrics object is constructed
  only if `prometheus_dir` was set when the agent started. Adding it on a
  reload starts nothing; clearing it does not stop the writer. Restart to turn
  metrics on or off. The *path* itself is live.
- **The `host=` label on every metric.** Changing `hostname` changes reports
  and replica identity immediately, but the metric label keeps the old name
  until a restart.

In standalone mode the scheduler always runs; `features.schedule` is consulted
only in `--controlled`. In `--monitor` neither it nor `features.autofence` is
consulted at all — that mode starts no scheduler and no fence loop whatever they
say, and says so in the journal at startup.

### An invalid file: startup versus reload

**At startup**, an unusable file is fatal: the error is logged as
`invalid configuration`, naming the JSON key, and the agent exits 2. Under the
shipped units, `Restart=always` with `RestartSec=30` brings it back every thirty
seconds and it fails the same way until the file is fixed. There is no fallback
to a previous copy — this file is the only thing that says which binary to run
as root, and guessing is worse than not starting.

**At reload**, the same file is refused and the agent keeps running exactly as
it was. That asymmetry is the point: turning a validation error into an outage
across an estate at once, on a reload, is the failure this mechanism exists to
avoid.

## When the control plane sends a bogus document

The UI is a separately-versioned program. Refusing its document outright would
let a newer UI take an estate offline by overshooting a range, so a bad value is
**clamped or ignored, exactly as before** — what changed is that it is no longer
silent.

The failure this closes: you set an interval of 0, or a slot count of -1, and
the agent quietly substitutes something else. Nothing is wrong, nothing is
logged, and the setting you typed simply never applies. That does not look like
a mistake; it looks like the feature not working.

Complaints are logged when a **new** configuration is adopted, not on every
poll — an unchanged document comes back `304`, so this fires once per actual
change rather than every thirty seconds forever. A standalone agent complains
about its own file once at startup, for the same reasons.

```
WARNING the configuration just received has problems; it has been accepted with the corrections below, because refusing a control plane's document outright would take this host offline over a setting source=control plane problems=2
WARNING configuration problem: max_concurrent_syncs is 5000, clamped to 128
WARNING configuration problem: schedule entry 3 (db02) has interval_seconds 0 and will never run
```

Every entry says what was asked for **and** what the agent will do instead;
"invalid interval" tells you nothing you can act on.

| complaint | what happens |
| --- | --- |
| `report_interval_seconds` ≤ 0, or over 3600 | Floored to 60 / capped at 3600. |
| `poll_wait_seconds` ≤ 0, or over 600 | Floored to 30 / capped at 600. |
| `max_concurrent_syncs` negative | Meaningless; the default of 4 is used. |
| `max_concurrent_syncs` over 128 | Clamped to 128. |
| `target_replication_slots` for a host negative | **Ignored**, so there is no limit into that host at all — the exact opposite of what someone typing `-1` intends. |
| `shutdown_timeout_sec` out of 30–3600, estate-wide or per entry | Clamped. This number decides how long a production VM is given before its shutdown is called a failure. |
| a schedule entry with no `vm` | Will never run. |
| a schedule entry with `interval_seconds` ≤ 0 | Will never run. |
| the same `vm` in more than one entry | Only one can ever run; the other is skipped as already running. |
| a schedule entry with an unusable `profile` | Skipped every tick, with the validation error quoted. |

Those last three are what the scheduler used to skip with neither a log line
nor a metric — a VM that never runs and never says why is the failure this
agent exists to make visible.

The two intervals are corrected *before* the complaint check ever sees them, so
in standalone mode they can never fire; and a hand-written file refuses most of
the rest outright, which leaves the `> 128` clamp as the one complaint that
survives there in practice.

One cross-file check has nowhere else to live, since neither validator can see
both documents:

```
WARNING the control plane holds polls open at least as long as this agent will wait, so every poll will time out and no configuration change will arrive; raise control_plane.http_timeout_sec above it poll_wait_seconds=180 http_timeout=2m0s
```

A warning, never a refusal: `poll_wait_seconds` is the control plane's to change
at any moment, and refusing on it would hand the UI a lever over its own
reachability.

## Logging, metrics and parallelism

**Logging** goes to stderr, one line per event, via the same `pkg/trace` vmsync
itself uses — so under systemd it lands in the journal with no file to rotate:

```bash
journalctl -u vmsync-agent -f
```

Debug adds the per-tick scheduling decisions and the full argv of every vmsync
it runs. When a scheduled sync fails, the agent logs the exit code **and
vmsync's own output tail**, so the journal on this host explains the failure
without needing the UI — and in standalone mode instead of it, since there is no
report for the tail to travel in.

**Prometheus.** Set `prometheus_dir`; the agent then hands each run
`-prometheus-textfile <dir>/vmsync_<vm>.prom`, one file per VM, which is what
node_exporter's textfile collector expects:

```json
"prometheus_dir": "/var/lib/node_exporter/textfile_collector"
```

The agent writes nothing per-VM itself — vmsync does, exactly as it does when
run from cron, so an existing dashboard keeps working unchanged. Nothing at all
is written when the key is unset.

It also writes **its own** file, `vmsync-agent.prom`, describing the scheduler
rather than any one sync. (Hyphenated so a domain named `agent` cannot collide
with it.) Rewritten every 15 seconds through a temp file and a rename, mode
0644, so a scrape mid-write sees the previous content rather than a parse
error. A timer rather than an event, because the values that matter most here
age on their own — how long since the UI answered, how overdue a sync is — and
must refresh even when nothing is happening, which is exactly the state being
alerted on.

Every series carries `host="<hostname>"` in addition to the labels below.

### Every metric

**Identity**

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `vmsync_agent_build_info` | gauge | `version` | Always 1. |
| `vmsync_agent_start_timestamp_seconds` | gauge | — | When this process started. Restart detection. |
| `vmsync_agent_mode` | gauge | `mode` | Which mode this agent runs in: one series per mode, exactly one of them 1. Replaces `vmsync_agent_standalone`, which could only ask "is there a control plane?" — a question with three answers now. The one worth alerting on is `vmsync_agent_mode{mode="monitor"} == 1`: a host that reports beautifully and replicates nothing on its own is invisible in every other series here, which is exactly how a site everybody assumed was covered turns out not to be. |

**Configuration**

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `vmsync_agent_config_generation` | gauge | — | Which generation of `agent.json` is in force: 0 at startup, +1 per accepted reload. Tells an agent that adopted your edit from one still running the file as it was at boot. |
| `vmsync_agent_config_rejected_total` | counter | — | Reloads refused because the new file asked for something a running agent cannot do. Needs a restart, not another edit. |
| `vmsync_agent_config_age_seconds` | gauge | — | Age of the schedule actually in force. `-1` when it was never fetched. Distinct from UI contact: an agent can be talking happily to the UI while running instructions from before a partition it has not noticed ended. |

**Schedule and runs**

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `vmsync_agent_scheduled_vms` | gauge | — | Entries that are enabled, named and have a positive interval. |
| `vmsync_agent_scheduled_vms_disabled` | gauge | — | Entries present but not runnable. |
| `vmsync_agent_max_concurrent_syncs` | gauge | — | The **effective** ceiling, after every limit is applied. |
| `vmsync_agent_syncs_running` | gauge | — | Syncs running right now. |
| `vmsync_agent_sync_runs_total` | counter | `result="success"\|"failure"\|"busy"` | Scheduled syncs that finished. |
| `vmsync_agent_skipped_runs_total` | counter | `reason` | Due syncs that did not start. Seven reasons, below. |
| `vmsync_agent_last_attempt_timestamp_seconds` | gauge | `vm` | When a sync was last *started* for each VM, whatever the outcome. |
| `vmsync_agent_next_run_timestamp_seconds` | gauge | `vm` | When each scheduled VM is next due. |
| `vmsync_agent_run_log_writable` | gauge | — | 1 when the run log can be written. **At 0 this host launches no syncs at all.** |

`result="busy"` is its own label rather than folded into success or failure,
because it answers a question neither can: a rising busy count with a flat
success count is a VM whose sync outlasts its interval, or an agent that
restarted into a run it did not start. Both look like healthy replication on
every other series here.

`run_log_writable` is a gauge and not just the skip counter beside it, because
under the fail-closed contract an unwritable run log stops every sync on this
host — and a counter that has stopped incrementing looks exactly like a host
with nothing due. One of those is idleness and the other is an outage.

The seven skip reasons:

| `reason` | meaning |
| --- | --- |
| `host_concurrency` | The host-wide parallel-sync ceiling was full. |
| `target_replication_slots` | The per-target fan-in limit was full. |
| `already_running` | This VM's previous run, started by **this** process, has not finished. The interval is shorter than the sync takes, which no extra capacity fixes. |
| `foreign_run` | A vmsync **this agent did not start** holds this domain's run lock — almost always its own previous instance's child, still running across a restart. Distinct from `already_running`, which is this process's own in-memory bookkeeping; this one is what that bookkeeping cannot see. Read from the lock's identity, never by taking the lock, and every uncertain answer means "launch anyway and let the engine decide". |
| `invalid_profile` | The entry's profile does not validate. Logged with the offending field, and deferred a whole interval so it says so once per interval rather than once per tick. |
| `no_target` | The request could not be built: no such domain here, no `replica_targets`, a fan-out with no `target_host` chosen, a `target_host` that is not among them, or local libvirt unreachable. Also deferred an interval. |
| `run_log_unwritable` | The launch record could not be written, so the launch did not happen. Its own reason because the remedy is a disk, not a schedule. **Not** deferred: it retries the moment the disk frees. |

**Inventory**

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `vmsync_agent_domains_total` | gauge | — | Domains on this host. |
| `vmsync_agent_domains` | gauge | `status` | Domains by assessed replication status. |

**Control plane**

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `vmsync_agent_ui_last_contact_timestamp_seconds` | gauge | — | Last successful exchange, including a `304`. 0 when there has never been one. |
| `vmsync_agent_ui_failures_total` | counter | — | Failed exchanges with the UI. |
| `vmsync_agent_ui_clock_skew_seconds` | gauge | — | Control-plane clock minus this host's. **Emitted only once measured** — a hardcoded 0 would be indistinguishable from "perfectly in sync", the one reading nobody should get for free. Free to collect: every HTTP response already carries a `Date` header. Never present in standalone. |

**Split brain**

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `vmsync_agent_split_brain_vms` | gauge | — | VMs running here that a peer reports having been failed over from. **The one to alert on.** |
| `vmsync_agent_split_brain` | gauge | `vm` | 1 while this host still runs a VM another host has been promoted for. |
| `vmsync_agent_fences_total` | counter | `result="success"\|"failure"` | Fences acted on. A failure needs a person: fences are never retried automatically. |
| `vmsync_agent_fences_unrecorded_total` | counter | — | Fences that proceeded without a durable ledger record, because writing it failed and a split brain is the worse outcome. Independent of the two above — a fence can be unrecorded and still succeed. Non-zero means the audit trail has a hole and the ledger's filesystem is in trouble. |

These exist because the per-VM files cannot answer *"is anything actually
replicating?"*. A VM whose profile never validates, or that never wins a
concurrency slot, never runs vmsync at all — so it never writes a per-VM file
that could go stale and reveal it. The gap between `next_run_timestamp` and
`last_attempt_timestamp`, and any movement in `skipped_runs_total`, is where
that shows up.

Two subtleties worth knowing when alerting. Every `reason` and `result` series
exists from the first write at zero, so `increase()` over a window works from
the start rather than only after the first occurrence. And `already_running`
counts a slot the interval said a VM should have had — not one tick of a long
sync — so a sync that takes longer than its interval increments it once per
missed slot, which is the number you want.

`ui_last_contact` and `config_age` are emitted in standalone mode too, as an
explicit zero and `-1`: a missing series and a UI that has never answered look
identical to a query, and one is a design choice while the other is an outage.

### Parallelism

Each due VM runs in its own goroutine, one `vmsync` process each, admitted
against two independent limits:

| limit | set by | meaning |
| --- | --- | --- |
| `max_concurrent_syncs` | the schedule (UI setting, or the standalone file) | Jobs at once on this host, whatever their destination. Default **4**. |
| `limits.max_concurrent_syncs` | `agent.json` | Host-local **ceiling**; only lowers the above. |
| `target_replication_slots` | the schedule | Jobs at once *into* a given target host. **Per agent** — see below. Opt-in: no default, and an absent, `0` or negative entry means no limit. |
| hard clamp | built in | 128, whatever anything asks for — a backstop against a mistyped value, not a recommendation. |

The two count different things and protect different machines.
`max_concurrent_syncs` bounds outbound load — this host's disks, NICs and CPU —
and applies no matter where the copies are going. `target_replication_slots`
bounds *fan-in*: how many copies land on one target at once.

`limits.max_concurrent_syncs` deliberately only lowers. How much concurrent I/O
a hypervisor can absorb depends on its disks, its NICs and what else it is
running — the host knows that and a control plane does not, so the UI can ask
for less than the host allows but never more.

Replication slots are the one limit only a UI can compute: an agent cannot see
that four *other* hosts are also replicating into the same target.

**They are counted per agent, not estate-wide.** Each agent holds its own tally
of syncs into each target and the UI serves every agent the same map, so
`{"dr01": 2}` means *each* agent may have two syncs into `dr01` — with five
source hosts that is ten concurrent writers, not two. To cap the real
aggregate, divide by the number of agents replicating into that host and set
that. Enforcing a true estate-wide total would need the agents to coordinate,
or the UI to hand out slots, and neither exists today.

A VM refused admission is **not deferred** — it retries on the next tick, as
soon as a slot frees. A VM whose previous run is still going is skipped rather
than overlapped, and first runs are staggered deterministically per VM across
the whole interval, so a fleet that all wants `interval_seconds: 900` does not
fire on the same second and a given VM keeps its slot across restarts. The next
slot is `now + interval`, not `previous + interval`: an agent that was down for
three hours runs each entry once when it comes back and then resumes its
cadence, rather than firing every interval it missed in a burst.

Loop intervals, for reference: scheduler tick 10s, reload poll 10s, metrics
rewrite 15s, operations tick 5s, fence sweep 60s (peer query bounded at 30s),
config-poll backoff 5s doubling to 5m on failure only.

## The run log

Every vmsync process this agent starts is recorded at `<state_dir>/runs.jsonl`.

It is **not a ledger**. Nothing branches on it, nothing is looked up in it, no
decision consults it — that is what separates it from `operations.json` and
`fences.json`, whose contents gate execution. It is evidence, and its job is to
answer "what did this host actually run, and when" after the fact.

| property | |
| --- | --- |
| path | `<state_dir>/runs.jsonl`, default `/var/lib/vmsync-agent/runs.jsonl` |
| mode | **0600**, unlike the 0644 state files beside it |
| format | append-only, one JSON object per line, `fsync` before each append returns |
| rotation | at 32 MiB, renamed to `runs.jsonl.1`; one generation kept, so the worst case is twice the cap |

0600 because this file *aggregates*: every argv ever run, every SSH key path,
every target URI, every disk path. Collectively that is a map of this host's DR
topology, and it is also the file most likely to leave the host attached to a
ticket.

Append-only rather than a rewritten document: a whole-file rewrite has a
truncate-then-write window, while `O_APPEND` plus one write has none, tolerates
a torn last line after power loss (readers skip what does not parse), and
rotates by rename with no reader coordination. Rotation is by size, not age,
because this agent already knows its clock may be wrong — it measures skew
against the UI for exactly that reason — and a clock that jumps backward makes
an age-capped log immortal.

Four event kinds. A run produces a `launch` and an `exit` joined by `run_id`,
never one mutable record, because a mutable record needs a rewrite and a
rewrite reintroduces the truncation window this format avoids.

| `event` | |
| --- | --- |
| `session` | this agent process started |
| `launch` | a vmsync process is about to be started — with `origin` (`scheduled`, `operation` or `fence`), `vm`, `target_host`, `binary` and the redacted `args` |
| `exit` | how it ended — `pid`, `exit_code`, `duration_s`, `outcome` (`success`, `failure` or `busy`), and a `log_tail` on anything but success |
| `rotate` | this file was rotated, so a reader of the new generation knows why it starts where it does |

**Argv is redacted through an allowlist**, not a denylist. A denylist fails
open: the day somebody adds a flag carrying a secret, an unfiltered log writes
it to disk with rotation as the only expiry. Here an unrecognised flag keeps its
name — knowing *which* unknown flag appeared is what makes it diagnosable — and
loses its value to `[redacted]`. Any userinfo password in a libvirt URI is
replaced, and a URI that will not parse is redacted whole. Note what this is
*not* doing: the unredacted argv is already in `/proc/<pid>/cmdline`,
world-readable on a default Linux. This declines to make a transient exposure
durable and archivable.

### What fails closed, and what does not

| | |
| --- | --- |
| The log cannot be opened at startup | **Fatal, exit 2.** An agent that cannot record what it runs would refuse every sync under the rule below — looking healthy in every other respect while replicating nothing. A host that will not start says so. |
| A **scheduled sync's** launch record cannot be written | The sync **does not start**. Counted as `run_log_unwritable`, and not deferred: the next slot is left in the past so it retries the moment the disk frees, rather than adding an interval of outage to a condition that may clear in seconds. |
| An **operation's** launch record cannot be written | The operation does not run; its intent record is released so the next tick tries again. |
| A **fence's** launch record cannot be written | **The fence proceeds anyway**, loudly. Everywhere else a refused launch costs one interval of replication; here it would leave one workload live in two places writing to two diverging copies. |
| Any **exit** record cannot be written | Best-effort. The process has already run, so refusing now would change nothing, and losing the record leaves an open run the log itself reports rather than a silent gap. |
| The rotation rename fails | Best-effort. The current file stays open and appends past the cap, which is strictly better than discarding the audit trail to honour a size limit. |

Whenever the state changes, `vmsync_agent_run_log_writable` follows it — logged
on the transition rather than per attempt, because at 10-second ticks across
fifty VMs, per-attempt logging would fill the journal of a host whose disk is
already full.

## Operations: one-shot instructions

Alongside the schedule, the UI can publish **operations** — a promotion, an
inversion, a clean shutdown, a role change. They travel in the same document
but are a different kind of thing, and the agent treats them differently at
every step. They exist only in control-plane mode; standalone starts no
operations loop at all.

| kind | runs on | what it does |
| --- | --- | --- |
| `promote` | the agent holding the replica | Makes a replica the live copy, optionally arming a fence against the old source. Acts on local libvirt, so a failover needs no credentials to reach the site that just failed. |
| `shutdown-domain` | that domain's agent | Clean shutdown, never a destroy; leaves replication `paused`. |
| `set-role` | that domain's agent | Writes `replication_role` and nothing else. |
| `restore` | the agent holding the replica | Rolls a replica back to one of its restore points, in place, and pauses replication into it. Always forced: an operation that only printed an assessment would report success having done nothing. |
| `invert` | the **old source**'s agent | Reverses the pair's direction. Needs the promoted peer named on the operation, and spans both ends — so it runs where the old source's disks are, which is also the side that already has the SSH path. |
| `reinit` | the **source**'s agent | A one-shot full resync. |
| `force-clean` | the **source**'s agent | A `reinit` that also removes the target domain and overrides the `promoted`/`paused` interlock. See the [main README](../../README.md#when-a-reinit-itself-will-not-go-through). |

The split in that middle column is the one operators get wrong. Most kinds act
on a domain where it sits and use a local URI. But `invert`, `reinit` and
`force-clean` span both ends and run on the source; the last two are **syncs**,
so they need the pair's whole transport configuration, which lives on the
schedule the *source*'s agent holds. An agent asked to reinit a VM it has no
schedule entry for refuses and says which mistake was made.

`reinit` and `force-clean` are deliberately operations rather than schedule
settings. On a schedule either would be desired state with nobody to clear it —
every cycle would delete the target's disks again, forever, and `force-clean`
would undefine the target domain each time too. They are also two kinds rather
than one kind with a flag, so the audit trail records which of them ran: they
destroy different amounts of the target.

The schedule is *desired state*: re-delivering it is harmless, which is what
lets an agent keep running it through a partition. An operation is an *event*.
Delivering it twice must not do it twice.

| rule | why |
| --- | --- |
| Executed only from a config received over the wire in this process | The on-disk cache is replayed on restart. An operation replayed from disk is a failover from an instruction nobody re-issued. This is structural, not a runtime guard: the type every *file* decoder is instantiated with — read and write alike — has no field for an operation at all, so none can reach the disk in either direction. |
| Intent recorded **before** the work, in `operations.json` | Recording completion afterwards leaves the whole duration of a shutdown with no trace on disk; a crash there loses all knowledge of a half-performed failover. |
| A record in **any** state refuses re-execution | Including `failed`. Re-running a failover that already went wrong, unattended, compounds it. A person re-issues with a fresh ID. |
| `running` found at startup becomes `unknown`, never retried | The agent died mid-operation. What state the domain reached is something only an inspection establishes. |
| Results re-sent on **every** report until the UI stops publishing | A result sent once is lost if that report doesn't land, leaving the UI publishing forever and the agent skipping forever — both halves working, jointly stuck. |
| Hard expiry (`not_after_unix`) | A target host that was down when a promote was issued must not execute it days later, as a *first* delivery no replay guard covers. |
| An unknown `kind` is refused and **reported** | A kind from a newer UI. Silence would leave an operator watching a failover sit pending against a healthy agent with nothing saying why. |
| The UI's peer is checked against local metadata, never used as an endpoint | Everywhere else the far end comes from the VM's own `replica_source`/`replica_targets`. This is the one channel that can stop a production VM. |

One exception to the replay guard: if vmsync stands down because another vmsync
already holds that domain's lock, the operation is **deferred**, not recorded.
Its intent record is removed and the next tick tries again — the reason it
could not run clears up by itself when that sync finishes. Bounded by the right
thing: the operation's own `not_after_unix` still expires it if the blocking
sync outlasts the deadline.

Operations run **one at a time**, on a five-second tick, and are **not**
disabled by `features.schedule: false`. That setting means "do not run the
schedule" — and a DR target host is both the machine most likely to carry it
and the machine a failover must run on. Tying the two together would deliver a
promotion to a visibly healthy agent that silently ignores it.

Every outcome is reported, including refusals. An operator watching a failover
sit "pending" against a healthy agent, with nothing saying why, is the worst
thing this could do.

## Fencing: stopping a VM that has been failed over

Every 60 seconds the agent asks, for each **running** VM this host is the
source for, whether any of its replica targets has been promoted with a fence
armed against this host. If one has, it shuts that VM down — cleanly, via
`vmsync -shutdown-domain`, which never falls back to destroying a domain and
leaves replication `paused`.

This is what stops one VM serving in two places after a failover. The rules
that decide it live in `vmsync` and are described in the [main
README](../../README.md#fencing-not-running-one-vm-in-two-places); what matters
here is how the agent behaves around them.

| rule | why |
| --- | --- |
| The token is read from the **peer's own libvirt**, never from the UI | A UI-issued fence operation is an instruction to go and *check*. A wrong or compromised control plane cannot manufacture a token without also holding the target hypervisor. |
| Runs regardless of `features.schedule` | A displaced source is very often one whose replication was disabled — by the operator, or by the failover itself. Gating on the schedule would switch the protection off exactly where it is needed. |
| Runs in standalone too | That is the case with no control plane to notice a failover at all, so it is the one that most needs a host able to work this out for itself. |
| Only **running** domains that record replica targets, in the source role, are checked | A stopped domain cannot be half of a split brain; a `paused`, `promoted` or target-role domain is not serving. This is also what keeps the sweep cheap: one remote query per genuinely at-risk pair, not per defined domain. |
| Intent recorded in `fences.json` **before** the shutdown | Same reasoning as operations: a crash between deciding and acting must not leave a token looking untouched. |
| …but a fence is **not refused** when that record cannot be written | A shutdown performed twice costs one more ACPI request to a guest already ignoring the first; a fence that does not fire is unrecoverable. Logged loudly and counted in `vmsync_agent_fences_unrecorded_total`. |
| A fence is acted on **once, ever** — including one that failed | Kept in a ledger keyed by `fence_id`, separate from `operations.json` precisely because that one is pruned when the UI stops publishing, and a fence has no such acknowledgement. Sharing it would silently destroy the single-use property. |
| An unreachable peer changes nothing | Not being able to ask is not evidence. Silence means keep serving. |

One configuration snapshot is taken per sweep and used for the whole of it: a
sweep that started under one `libvirt_uri` must not finish under another, or
half its peer queries would name a different host.

`features.autofence: false` keeps the check and the warnings but takes no
action, for hosts where stopping a VM must stay a human decision. It is **not**
latched: nothing was acted on, so the warning repeats every cycle until
somebody resolves it. Unlike `features.schedule`, it is read fresh on every
sweep, so a reload switches it on or off from the next one.

### How long a guest gets

A shutdown that overruns is reported as a **failure**, and a failed fence is
latched — so a guest that simply needed longer looks exactly like one that
refused to stop, and nothing will try again. Getting this number right matters
more than it sounds.

Resolved in three steps: the VM's own `shutdown_timeout_sec` on its schedule
entry, then the schedule's estate-wide `shutdown_timeout_sec`, then 300
seconds. A **shutdown operation** instead carries a value the UI already
resolved, so the instruction means the same thing whenever it runs.

Two guards worth knowing about:

- Whatever the schedule says is **clamped to 30–3600 seconds**. It arrives from
  a separately-versioned program over the network, and this number decides how
  long a production VM is given before its shutdown is called a failure. A
  hand-written standalone file is *refused* rather than clamped, because that
  one was typed by a person and silently changing their number is the failure
  mode that file's strict parsing exists to prevent.
- vmsync itself is given **two minutes more** than the guest is. Bounding it at
  exactly the guest timeout would kill it just as the guest ran out, turning
  "the guest did not stop in time" — a clear diagnosis — into "the process
  disappeared", and leaving a `running` ledger entry behind a fence that will
  never be retried.

### When it doesn't work, it says so

A failed fence is not retried, so it must not also go quiet. While this host is
running a VM that a live promoted peer holds a fence against:

```
vmsync_agent_split_brain{host="prod01",vm="web01"} 1
vmsync_agent_split_brain_vms{host="prod01"} 1
vmsync_agent_fences_total{host="prod01",result="failure"} 1
```

The journal lines to grep for are `SPLIT BRAIN:`, `FENCING:` and
`FENCING FAILED:`.

`vmsync_agent_split_brain_vms` is the one to alert on. It is rebuilt from each
**complete** sweep rather than updated as findings arrive, so it clears by
itself once the condition ends — a gauge that latched at 1 forever after the
first detection would be an alert nobody could clear, which is how people learn
to ignore the metric that must never be ignored. A sweep that could not run, or
that was cut short, leaves the previous value alone rather than clearing it:
failing to look is not evidence the problem went away.

The same facts also ride in every report to the control plane, per domain, as a
`fenced` object carrying the fence id, its state, when it acted, which peer
displaced this domain and who performed that promotion. That is the only route
by which a console can tell a **fenced** VM from one an operator paused — both
are just `paused` in libvirt — or notice a fence that failed, which leaves no
mark in libvirt at all. A promoted domain additionally reports the fence its own
promotion armed, so "did this failover authorise stopping anything" is
answerable without reading metadata on a hypervisor.

Reported by `--once` as well as by the daemon. A report replaces what the UI
holds for that host, so a one-shot run that omitted this would quietly erase the
console's record of which VMs were fenced — precisely while somebody is poking
at a machine mid-incident.

## Standalone: a scheduler with no control plane

Omit `control_plane` and set `schedule_file`, and the agent runs the scheduler,
the fence sweep and nothing else. No enrolment, no credential, no reporting, no
polling, no operations — it never opens a network connection to anything but the
hypervisors it syncs to.

```json
{
  "config_version": 1,
  "schedule_file": "/etc/vmsync/schedule.json",
  "ssh": { "user": "root", "key": "/etc/vmsync/id_ed25519" }
}
```

This is not a reduced mode bolted on. The scheduler always ran from a cached
configuration and always kept running while the UI was unreachable — that is the
partition-tolerance the control plane is built on. The only thing that ever
required a UI was the startup path, and this skips it.

What you get over a cron job: a real interval per VM, a cap on how many syncs
run at once, per-target replication slots so several sources cannot stampede one
target, staggering so everything does not fire on the same minute,
skip-if-still-running instead of overlapping runs, detection of a sync left
behind by a previous agent process, split-brain protection, a durable run log,
and each run's outcome in the journal. An agent installed this way can be
enrolled with a UI later by editing the same file — though the switch costs a
restart, deliberately.

Setting both `schedule_file` and `control_plane` is refused: in control-plane
mode the schedule is agent-written state under `state_dir`, and pointing it
elsewhere would have the agent overwrite a config-managed file.

A standalone agent reloads `agent.json` exactly like a control-plane one — the
file is the only way to configure either, so it must be the only way to
reconfigure either. The **schedule** file is not reloaded; it is read once at
startup.

Look for this at startup:

```
running standalone: no control plane, scheduling from file file=/etc/vmsync/schedule.json entries=1 enabled=1 vmsync=/usr/local/bin/vmsync max_concurrent=4
```

`max_concurrent` there is the *resolved* number, after `limits.max_concurrent_syncs`
has had its say, so a host ceiling that quietly overrides the file is visible.

### The schedule file

The same document the UI would otherwise send. Everything except
`config_version` and `schedule` is optional.

```json
{
  "config_version": 1,
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
        "verify": "full",
        "target_disk_path": "/data/replicas",
        "retention": "24,3h"
      },
      "shutdown_timeout_sec": 900
    }
  ],
  "max_concurrent_syncs": 4,
  "shutdown_timeout_sec": 300,
  "target_replication_slots": { "dr01": 2 }
}
```

| field | default | meaning |
| --- | --- | --- |
| `config_version` | — | Must be `1`. Required in a hand-written file. |
| `schedule[]` | — | Required unless the file defines a `default` template — see below. Without one, this file is the only thing telling the agent what to sync, and an empty schedule is a misconfiguration. |
| `templates` | — | Named cadences and profiles that entries inherit from. See **Schedule templates**. |
| `schedule[].vm` | — | Required. Source domain on **this** host. Its targets are read from its own libvirt metadata, never from this file. |
| `schedule[].interval_seconds` | — | How often to sync. Required and greater than 0 **unless** the entry inherits one from a template. |
| `schedule[].template` | `default` | The template this entry inherits unset fields from. Naming one this file does not define is an error. |
| `schedule[].verify_interval_seconds` | `0` (every sync) | How often a sync of this VM should ALSO verify. Needs a verify mode on the entry or its template. Negative is refused. |
| `schedule[].verify_days`, `schedule[].verify_window` | — | The calendar form of the same cadence — see **Verify windows**. Mutually exclusive with `verify_interval_seconds`. |
| `schedule[].enabled` | `false` | `false` keeps an entry visible while stopping it from running. |
| `schedule[].target_host` | — | Required only when the VM replicates to more than one target; otherwise the single target is used. |
| `schedule[].profile` | `{}` | vmsync settings for this VM; see below. |
| `schedule[].shutdown_timeout_sec` | `0` (inherit) | How long THIS guest gets to stop cleanly. 30–3600; out of range is an error, not a clamp. |
| `max_concurrent_syncs` | `4` | Cap on syncs running at once on this host. Negative is refused; above 128 is clamped, and complained about. |
| `shutdown_timeout_sec` | `0` | Default for every VM with no value of its own. Omit for vmsync's own 300s. 30–3600 otherwise. |
| `target_replication_slots` | — | Cap on concurrent syncs *into* a given target host, counted by **this** agent alone. Omit for no limit; a negative value is refused. |

The profile fields, all optional, all validated with the engine's own parsers so
that a value the agent accepts is one vmsync will accept:

| field | accepted |
| --- | --- |
| `compress` | `""`, `"zstd"`, `"s2"` |
| `compress_level` | `1`–`19` for zstd; `default`, `better`, `best` for s2. Refused without a `compress`. |
| `netbuffer` | `"<blocksize>,<buffersize>"`, e.g. `"128k,1G"` |
| `use_ssh` | boolean. Refused without `compress` or `netbuffer`, which are what it tunnels. |
| `io_depth` | 0 for vmsync's default, otherwise 1–64 |
| `no_checksum` | boolean, default `false` — the check is **on**. Disables the pre-commit integrity check: the copy hashes every chunk it reads, `vmsync-bridge-helper` hashes the same ranges back off the target, and an incremental sync's overlay is removed instead of committed if they disagree. Rarely needed: where the helper is missing or version-skewed vmsync already skips the check with a warning rather than failing the sync, so this is for stating that intent deliberately and silencing that warning. |
| `verify` | `""`, `"fast"`, `"full"`, `"qemu-img"`. All three compare against the same frozen source snapshot the copy read from; only `qemu-img` suspends the source, and only to keep the snapshot's scratch space empty. |
| `verify_failure_reinit` | boolean, default `false`. Answers a verification failure with one full recopy and a second verification. If that also fails, the replica is recorded as faulty on its own domain XML: later syncs into it are refused, and a promotion reports it as untrustworthy, until a human clears it. Refused without `verify`. Worth setting here even though vmsync defaults it off — a one-off run has an operator to decide what to do about a mismatch, a scheduled one does not, and without it the next run syncs straight over the finding. |
| `reinit_after_failures` | 0 to disable, otherwise up to 100 |
| `target_disk_path` | absolute and clean |
| `timestamp_tolerance_sec` | 0 to compare exactly, otherwise up to 3600 |
| `retention` | `"<count>,<interval>"`, e.g. `"24,3h"`. Empty keeps no restore points, so an estate run entirely through the schedule takes none. |
| `source_port_range`, `target_port_range` | a range (`20000-20100`), or one fixed port to pin it. Both **default to a range**, so leaving these empty is right for almost every profile — and is what makes two concurrent syncs on one host not collide. They used to default to a single fixed port, and since nothing here or in the UI ever set them, every VM in an estate shared it. |

`report_interval_seconds`, `poll_wait_seconds` and `cadence_seconds` are part of
the same document type and are accepted here, but do nothing: there is nothing
to report to and nothing to poll.

Note what is **not** in the file: no hostnames to replicate to, no credentials,
no command line, and **no `operations` block** — the type this is decoded into
has no field for one. Standalone mode starts no operations loop, so an
`operations` key used to parse cleanly and then vanish without a word; now the
strict decoder reports it by name.

The pairing comes from each VM's own `replica_targets` metadata, and SSH details
come from `agent.json`. This file says *when*, not *where* or *how to log in*.

A duplicate `vm`, a missing `vm`, a non-positive interval, an unusable profile,
a negative concurrency or slot count, or an out-of-range shutdown timeout are
all **refused outright** rather than discovered one VM at a time at run time. A
file where nothing is enabled is accepted — parking a host is legitimate — but
says so, because silence there looks identical to a schedule that is not being
read at all.

Changes take effect on restart:

```bash
systemctl restart vmsync-agent
```

### Schedule templates

A `templates` block holds named cadences and profiles that entries inherit
from. It is the only way to use templates without a control plane, and it works
identically either way — the agent resolves them, so a standalone host and an
enrolled one behave the same.

```json
{
  "config_version": 1,
  "templates": {
    "default": {
      "name": "default",
      "interval_seconds": 900,
      "enabled": true,
      "verify_days": "Sun *-*-01..07",
      "verify_window": "02:00-12:00",
      "profile": {
        "compress": "zstd",
        "compress_level": "5",
        "verify": "full",
        "verify_failure_reinit": true,
        "retention": "24,3h"
      }
    },
    "hourly": {
      "name": "hourly",
      "interval_seconds": 3600,
      "enabled": true,
      "profile": { "compress": "s2", "verify": "fast" }
    }
  },
  "schedule": [
    { "vm": "db01", "template": "hourly", "enabled": true },
    { "vm": "web01", "enabled": true, "interval_seconds": 300 },
    { "vm": "lab01", "enabled": false }
  ]
}
```

The map key and the template's own `name` must match — an entry can only refer
to it by the key, so letting the two differ would be a trap. Omitting `name`
fills it in from the key.

**Unset inherits.** `0` and `""` on an entry mean "take the template's", field
by field, so overriding one setting does not cost a full restatement — `web01`
above keeps the default's compression, verify mode and calendar and changes
only its interval. `enabled` is the exception and is **never** inherited,
because `false` cannot be told from unset in a boolean.

**A `default` template covers VMs with no entry at all.** That inverts the
failure mode: forgetting to add a VM used to mean "silently not replicated",
and now means "replicated with generic settings". It reaches only VMs whose
domain records `replica_targets` — written by a *successful* sync — so it
applies to pairs somebody established by hand at least once, never to a VM
nobody has ever synced. **Without a `default` template nothing is synthesised**, which is the
feature's off switch. Auto-applying a schedule to VMs nobody wrote an entry
for has to be something somebody asked for, and creating a template called
`default` is that request -- stated once, and visible in the file.

To exclude a VM the default would otherwise cover, give it an entry with
`"enabled": false` — as `lab01` does above.

An entry naming a template the file does not define is refused, by VM and by
the name that was typed. So is a template with no cadence: a bad template is a
bad hundred entries, and discovering it one VM at a time at run time would name
the VM rather than the template they all share.

### Verify windows: `verify_days` and `verify_window`

`verify_interval_seconds` says how *often* to verify. The calendar form says
*when*, which an interval cannot express — and an interval drifts: a VM
verified at 02:00 today is verified mid-morning two months later, during
exactly the hours somebody was avoiding.

```json
"verify_days":   "Sun *-*-01..07",
"verify_window": "02:00-12:00"
```

That is *the first Sunday of the month, between 2am and noon*. `verify_days` is
the **day half** of a systemd `OnCalendar` expression — `[weekdays]
[year-month-day]`, either part optional — and the two parts are matched with
**AND**, which is what makes `Sun` plus `*-*-01..07` mean the first Sunday
rather than "Sundays or the first week".

| form | example | means |
| --- | --- | --- |
| weekday | `Sun` | every Sunday |
| weekday list | `Sat,Sun` | weekends |
| weekday range | `Mon..Fri` (`Mon-Fri` also accepted) | working days. Wraps: `Fri..Mon` is Fri–Mon |
| day of month | `*-*-01` | the 1st |
| day-of-month range | `*-*-01..07` | the first week |
| month | `*-01,07-*` | January and July |
| combined | `Sun *-*-01..07` | the first Sunday of the month |
| omitted | `""` | every day |

`verify_window` is `HH:MM-HH:MM`. The start is **inclusive** and the end
**exclusive**, so two abutting windows cannot both claim a moment.
`22:00-04:00` crosses midnight and belongs to the day it *opened* on — under
`"Sun"`, that runs into Monday morning. Omitting the window means the whole
matching day.

Check an expression before pasting it in:

```bash
systemd-analyze calendar "Sun *-*-01..07"
```

Only a **subset** of `OnCalendar` is implemented, and everything else is
**refused by name** rather than half-understood: time components in the day
expression (`Sun *-*-01..07 02:00:00` — the window carries the time), `~` for
last-of-month, `/` for repetition, timezone suffixes in any spelling (`UTC`,
`CET`, `Europe/Paris`, `+0200`), and the `daily`/`weekly` shorthands. Silently
ignoring the time component would verify at the right frequency on the wrong
hours and look like it worked.

Two malformed shapes are refused for the same reason, because both parse as
something the operator did not write:

- **An empty date component.** `"*-*-"` is a trailing-hyphen slip for
  `"*-*-01"`, and it still splits into three fields. Read as "any day" it
  would match 365 days a year instead of 12 — a monthly full-image verify
  quietly becoming a daily one.
- **A date that cannot exist.** `"*-02-30"` is a valid month and a valid day
  and matches nothing, ever. A schedule that silently never fires looks
  exactly like one that works, until somebody asks why a replica has not been
  checked in a year.

A few things worth knowing:

- **This still selects which syncs also verify.** A window does not launch a
  sync; it marks the syncs that land in it. So the window must be long enough
  to contain one — a five-minute window on a VM that syncs hourly will usually
  miss.
- **The agent's own local time**, from its system clock. A window means the
  quiet hours where the disks are, so an estate spanning timezones gets each
  host's night rather than the console's.
- **A missed window is not made up.** If the agent is down for the whole
  window, that occurrence does not happen and nothing fires outside it. That is
  deliberate: a verify is a full-image read on both sides, and repaying one on
  a Tuesday afternoon is the thing the window exists to avoid. Two missed
  windows in a row is a human problem.
- **`verify_interval_seconds` and the calendar are two spellings of one
  setting**, so an entry or template carrying both is refused. For the same
  reason the verify cadence inherits *as a unit*: an entry stating either form
  states its whole cadence and inherits neither half of the other. The verify
  *mode* still inherits normally — the calendar says how often, `profile.verify`
  says what.
- Either form needs `profile.verify` to name a mode, on the entry or its
  template. A cadence says how often to verify, not whether to. The check runs
  on the **resolved** entry, so a template may carry only the estate-wide
  window and let each VM name its own mode — which is the shape a verify
  window usually wants, since the window is estate policy and the mode is not.
- **The `default` template is the exception: it must be complete on its own.**
  It synthesises entries for VMs that have none, and those have nothing else
  to supply a mode — so a default carrying a cadence without one is refused
  rather than left to give every uncovered VM a schedule that can never fire.
- A **failed** sync gives its window back. The occurrence is claimed when the
  run is launched, so that one sync in a ten-hour window verifies rather than
  all of them; if that run then fails or stands down on lock contention, the
  claim is released and the next sync inside the window takes it. Without
  that, one transient failure at 02:05 would cost the VM its whole month.

### It still says *when*, not *whether*

Standalone mode schedules syncs. It does not weaken any of vmsync's own
refusals: a target marked `promoted` or `paused` is still refused, the
target-side run lock still excludes concurrent operations, and a role changed
mid-sync still aborts the run before it redefines the domain. A schedule cannot
talk vmsync into overwriting something it would otherwise protect.

## When you need `control_plane.ca_file`, and when you don't

**If the UI has a publicly-trusted certificate, you don't.** Leave it unset and
the agent uses the host's system trust store. Renewing that certificate changes
the leaf, which validates against roots the host already has — there is nothing
to redistribute to agents, ever.

It is only for:

- **A private/internal CA.** You distribute the **CA certificate**, not the
  leaf. Internal CAs are usually valid for years, so it is copied once and the
  leaf underneath rotates freely without touching any agent.
- **A bare self-signed certificate**, which is its own issuer. This is the one
  arrangement where every renewal means visiting every host. Prefer a small
  private CA, which turns that into a one-time copy.

Setting it **replaces** the system trust store rather than adding to it — only
that CA is trusted for the UI. If you later move to a publicly-trusted
certificate, remove the key; leaving it set fails with a plain certificate error
rather than anything subtle.

The file is read and parsed when the configuration loads, so a path that does
not exist or a bundle with no usable certificate refuses the whole file — at
startup, or as a refused reload. Changing it needs a restart: the HTTPS client
is built once.

## What it reports

Every domain libvirt knows about, including inactive ones — a replication target
is *supposed* to be shut off, so listing only running domains would hide every
target in the estate. Domains with no vmsync metadata are reported too, as
`unreplicated`: "nobody configured replication for this" and "this is protected"
must never look alike.

Each domain gets a status and the reasons behind it:

| status | meaning |
| --- | --- |
| `ok` | Replicating, recent, no failures. |
| `unreplicated` | vmsync has no relationship with this domain. |
| `paused` / `promoted` | Administrative states, not faults. |
| `warning` | Degraded but still replicating — failures recorded, past its cadence, or no checkpoint to sync incrementally against. |
| `critical` | Never synced, or far past its cadence. |

Two rules worth knowing when reading the output:

- **`promoted` and `paused` suppress the staleness checks.** Those domains are
  supposed to stop receiving syncs, so a growing age is expected rather than a
  fault — reporting it would bury the real signal for exactly the VMs you are
  watching most closely.
- **A domain with no configured cadence is not judged on freshness at all.**
  Guessing a threshold would report a pair that legitimately syncs weekly as
  critical forever. Cadences come from the UI, so in standalone mode nothing is
  judged on freshness — there is no estate-wide view to draw one from.

A report also carries the host's filesystem usage for the paths its domains'
disks live on, each domain's disks and restore points, the recent sync outcomes,
stored operation results, and the fence state described above.

A libvirt failure while building a report is logged loudly but is not fatal:
libvirtd restarts, and an agent that exited would then need systemd to bring it
back rather than simply recovering.

## The UI-facing API

The UI is a separate program, so the HTTP surface between them is a contract
between two codebases:

```
POST {base}/api/v1/agents/enrol             -> {"agent_id","token"}
POST {base}/api/v1/agents/{id}/report       bearer; inventory upload
GET  {base}/api/v1/agents/{id}/config       bearer; long-poll, ETag-aware
```

`client_test.go` drives a stub UI over TLS and pins every one of these — the
paths, the bearer header, the `If-None-Match`/`ETag` exchange, the `304` and
`401`/`403` semantics. **It is the executable specification for the UI**, and is
more reliable than this table, which can drift.

Two behaviours the UI must get right:

- **`401`/`403` means revoked.** The agent treats it as terminal rather than
  retryable and says so loudly, because it never succeeds again until an
  operator issues a fresh enrolment token.
- **Long-poll honestly.** The agent sends `wait=<seconds>` and expects the UI to
  hold the request open that long before answering `304`. That is what delivers
  an operator's change in seconds without the hypervisor accepting inbound
  connections. A `304` counts as a successful exchange: counting only changed
  configs would make a healthy, stable estate look like an unreachable one.

Response bodies are read through a limit. A compromised or malfunctioning UI
must not be able to exhaust an agent's memory.

## Exit codes

| code | meaning |
| --- | --- |
| `0` | `--version`; a successful `--once`; a clean shutdown on SIGTERM or SIGINT. |
| `1` | The agent stopped on an error after startup — enrolment failed, a ledger or the cached configuration could not be loaded. Logged as `agent stopped`. |
| `2` | Bad command line, unusable configuration, or the run log could not be opened. Includes **no mode flag, more than one, or a mode the file does not match** — all four are refused before anything starts. |

**75 is not one of the agent's.** It is `EX_TEMPFAIL` from a *vmsync child*, and
it means the engine stood down without touching anything because another vmsync
already holds that domain's lock. The agent reads it and neither counts it as a
success — which would have an agent restarted mid-sync report a phantom healthy
run every interval for as long as the real one lasted — nor as a failure, which
would drive `reinit_after_failures` toward discarding a replica because its own
previous run was still going. A scheduled sync that exits 75 is journalled and
counted as `vmsync_agent_sync_runs_total{result="busy"}` and is not shipped as a
result; an operation that exits 75 is deferred and retried on the next tick.

A missing `VMSYNC_AGENT_MODE` lands here, and `Restart=always` will retry it
every 30 seconds forever rather than giving up. That is intentional: the journal
says exactly what is wrong on every attempt, and an agent that gave up would be
a host silently absent from the console.

Under the shipped unit, `Restart=always` with `RestartSec=30` and
`StartLimitIntervalSec=0` means a failing agent keeps retrying forever.
systemd's default — five starts in ten seconds puts the unit in `failed` — is
designed for services where a crash loop means give up; here it means the
opposite, since permanently stopping the thing that reports replication health
is exactly the outcome to avoid.

## Troubleshooting

**`flag provided but not defined`** — you are following instructions for a
version before the config file. Everything except the five flags above moved
into `/etc/vmsync/agent.json`; see [the reference](#agentjson-reference).

**`no "config_version". Add "config_version": 1`** — every file, both of them,
must declare it.

**`json: unknown field "libvirt_url"`** — a misspelled key, or a key from a
newer agent. Both hand-written files are parsed strictly, and the message names
the key.

**`"control_plane" is null`** — remove the key entirely to run standalone, or
give it a URL. An explicit null is indistinguishable from a typo that would
silently take this host off its control plane.

**`"state_dir" /var/lib/../lib/vmsync-agent is not canonical; write it as
/var/lib/vmsync-agent`** — the message contains the fix.

**`this agent has not enrolled yet and no -enrol-token was given`** — first run
on this host; generate a token in the UI and pass it with
`--enrol-token-file`. (The message still names the old flag.)

**`stored credential was issued by X but -ui is Y`** — the agent is being
repointed at a different UI. Enrol again with a token from the new one, or
remove `credentials.json` to start over. It refuses rather than presenting a
token the new UI has never seen, which would otherwise show up as an unexplained
stream of 401s. (`-ui` here means `control_plane.url`.)

**`--enrol-token-file was given but … has no "control_plane"`** — a token pushed
estate-wide has landed on a standalone host. Refused, so the host does not sit
silently un-enrolled forever.

**`cannot open the run log, refusing to start`** — `state_dir` is unwritable,
full, or read-only. Under `ProtectSystem=strict` a `state_dir` outside
`StateDirectory=` needs a `ReadWritePaths=` drop-in.

**Every sync fails around `/run/vmsync-locks`** — the run-lock directory is not
writable under the hardened unit. Add the `RuntimeDirectory` drop-in from the
install section.

**Polls always time out** — `control_plane.http_timeout_sec` is shorter than the
UI's `poll_wait_seconds`. The agent warns about this by name the first time it
sees a document that says so.

**A reload seems to do nothing** — check `vmsync_agent_config_generation`. If it
has not advanced, look for `reload REFUSED` in the journal, or
`vmsync_agent_config_rejected_total` moving, which means a restart is needed
rather than another edit. If it advanced but the setting did not take, check it
against the [live-or-restart table](#live-or-restart).

**Nothing is replicating, and nothing looks wrong** — check
`vmsync_agent_run_log_writable`. At 0 this host launches no syncs at all. Then
check `vmsync_agent_skipped_runs_total` by reason, and
`vmsync_agent_next_run_timestamp_seconds{vm}` — on a freshly started agent the
first run of each VM is placed up to a whole interval away.

**Scheduling is off entirely** — the startup line says which:
`scheduler running` or `scheduling disabled by -no-schedule; this agent will
report and nothing else`. The second means `"features": {"schedule": false}`,
and turning it back on needs a restart.

**The UI shows an agent as stale** — check `config_age_seconds` in its reports,
or `vmsync_agent_config_age_seconds` locally. The agent keeps working from its
cache during a partition; that field is how the UI shows it is running on old
instructions.

**`this agent reports under one name and appears in replication metadata under
another`** — `libvirt_uri` names a remote host, so replication metadata
identifies this agent by the URI's host rather than by `hostname`. Not an error,
but everything about a pair — fence tokens, `replica_source`, which row in the
console — then keys off a name that is not the one in the file.

**`this host's clock disagrees with the control plane's`** — NTP. vmsync compares
timestamps written by different machines, so replication ages and failover
data-loss windows are wrong until it is fixed. The threshold is 30 seconds, the
warning repeats at most hourly, and `vmsync_agent_ui_clock_skew_seconds` carries
the value continuously.

## What this deliberately does not do

- **No `--insecure`, and no way to skip TLS verification.** See above.
- **No hot move of `state_dir`, and no hot mode flip.** Both are refused on
  reload and cost a deliberate restart.
- **No live `features.schedule`, and no live control-plane address.** Both are
  reported as changed and both need a restart; the scheduler goroutine and the
  HTTPS client are startup decisions.
- **No turning metrics on or off by reload.** Whether the agent emits its own
  textfile at all is fixed at startup by whether `prometheus_dir` was set; only
  the path is live.
- **No estate-wide replication-slot total.** Slots are counted per agent; a true
  aggregate would need the agents to coordinate or the UI to hand out slots, and
  neither exists.
- **No catching up on missed intervals.** An agent that was down runs each entry
  once when it returns, then resumes its cadence.
- **No fence retry.** A fence is acted on once, ever, including one that failed;
  the metrics and the journal are what tell you, and a person resolves it.
- **No operation replayed from disk, ever.** The cache cannot express one.
- **No profile presets in either file.** `wan`, `lan` and `direct` exist as UI
  conveniences; no `preset` key is accepted from a file.
- **No inbound port, no listener, no API on the hypervisor.** The agent dials
  out and nothing dials in.