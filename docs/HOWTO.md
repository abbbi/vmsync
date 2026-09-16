# vmsync HOWTO

`-help` lists every flag in one line each, grouped by what it is for. This
document is the other half: why the defaults are what they are, what breaks if
you change them, and which combinations are refused.

It is organised by **task**, not by flag, because that is how the questions
arrive. If you came here from a one-liner that said "see HOWTO", the heading
you want is in the table of contents below.

- [The model: checkpoints, not snapshots](#the-model-checkpoints-not-snapshots)
- [Your first sync](#your-first-sync)
- [Making it fast over a slow link](#making-it-fast-over-a-slow-link)
- [Ports and firewalls](#ports-and-firewalls)
- [Integrity: two different checks](#integrity-two-different-checks)
- [When a sync refuses, and what to do](#when-a-sync-refuses-and-what-to-do)
- [Disks on the target](#disks-on-the-target)
- [Restore points](#restore-points)
- [Failover](#failover)
- [Reporting to something else](#reporting-to-something-else)
- [Deliberate faults, for testing](#deliberate-faults-for-testing)

---

## The model: checkpoints, not snapshots

vmsync replicates by creating a libvirt **checkpoint** — a persistent qcow2
dirty bitmap — and reading the source through a pull-mode backup job's NBD
export. It is not a snapshot: nothing is frozen on disk, the guest keeps
running, and there is no overlay accumulating on the source.

The consequence that matters operationally: **the chain is state.** Each
incremental run reads the bitmap laid down by the previous one. Anything that
destroys a checkpoint or its bitmap forces a full resync, which is why
`-reinit` is a bigger hammer than it looks and why vmsync refuses several
things rather than risk the chain.

The replica on the target is an ordinary qcow2 plus a libvirt domain
definition. vmsync records its own bookkeeping in that domain's XML under a
private namespace — the last checkpoint, when it was taken, which source it
came from, the replication role, and any verification finding.

---

## Your first sync

```bash
vmsync -source-uri qemu:///system \
       -target-uri qemu+ssh://root@dr01/system \
       -source-domain web01 \
       -target-disk-path /data/replicas
```

That is a full sync: the target domain does not exist yet, so every allocated
extent is copied and the domain is defined at the end. Run the same command
again and it is an incremental — vmsync finds its own checkpoint and copies
only what changed.

`-target-domain` defaults to `-source-domain`. Set it when the replica is
named differently on the target.

**If the source is shut off**, vmsync cannot read it through a backup job
without starting it. `-start` boots it in paused mode for the duration and
destroys it again afterwards, which is enough for the read and never runs the
guest's workload.

**`-local-host-name`** matters only when a URI names no host — a local
`qemu:///system`. vmsync records "which host is this" in `replica_source`,
`replica_targets` and `promoted_from`, and defaults to the system hostname.
Set it when something else refers to this machine by a different name;
`vmsync-agent` passes its own `--hostname` here, because the control plane
matches those references against the name an agent reports under.

---

## Making it fast over a slow link

By default vmsync copies over a plain NBD connection between the two hosts.
On a LAN that is usually the right answer. Over a WAN it is not, and there are
three independent knobs.

All three need **`vmsync-bridge-helper`** on the target host, at
`-bridge-helper-path` (default `/usr/local/bin/vmsync-bridge-helper`). It is a
single static binary; deploying it is copying a file.

### `-compress`

Compresses NBD traffic between the hosts. Bare `-compress` selects **s2**;
`-compress=zstd` selects zstd.

s2 is the default because the choice is usually not about ratio. On a link
fast enough that CPU matters, s2 compresses at several hundred MB/s per core
where zstd at a comparable ratio is far slower, and a compressor that cannot
keep up with the link is just a slower link. Reach for zstd when the link is
genuinely narrow and CPU is free.

### `-compress-level`

- `-compress=zstd`: a number **1–19**, defaulting to **3**.
- `-compress=s2`: one of `default`, `better`, `best` — s2 has no numeric
  levels. The default here is `better`.

There is deliberately no single default printed by `-help`: `3` is a valid
zstd level and an invalid s2 mode, `better` is the reverse, so whichever were
declared would be a value the other algorithm refuses outright.

### `-netbuffer`

Buffers bridge traffic through a bounded in-memory buffer to smooth
throughput, written `<blocksize>,<buffersize>` — for example `64k,512M`. Bare
`-netbuffer` is `128k,1G`.

This helps a link whose throughput is bursty or whose latency is high enough
that the copy's own pipelining cannot keep it full. It is independent of
compression; either, both or neither.

### `-use-ssh`

Routes the bridged traffic through the **existing SSH connection** as an
encrypted tunnel, rather than over a direct TCP connection between the hosts.
Refused without `-compress` or `-netbuffer`, since those are what create the
bridge it tunnels.

Use it where only SSH is permitted between the two sites. Note the
consequence for ports: with `-use-ssh` the target's NBD port is not reachable
directly from the machine running vmsync at all, which is why readiness checks
dial the local end of the tunnel rather than the remote export.

### `-io-depth`

How many NBD read/write pairs stay in flight during the copy. Default **8**.

Raise it for a high-latency link where the copy is latency-bound rather than
bandwidth-bound; lower it if the target's storage is the bottleneck and deep
queues are making latency worse. It is a throughput knob, not a correctness
one.

---

## Ports and firewalls

Both sides default to a **port range**, and a run takes what it needs from it:

```
-source-nbd-port   10809-10908   (100 ports)
-target-nbd-port   20809-21008   (200 ports)
```

**How many ports a run uses**, for `N` disks:

| side | plain | `-compress`/`-netbuffer` | `-verify` | both |
| --- | --- | --- | --- | --- |
| target | `N` | `2N` | `2N` | `4N` |
| source | 1 | 2 | 1 | 2 |

Source counts are per **run**, not per disk — `-verify` reuses the same source
export rather than opening a second one.

**Target ports are not contiguous.** Each export binds its own port and logs
which one it got, so a four-disk VM with compression and verification needs 16
ports *anywhere* in the range, not a 16-port consecutive span. That is what
makes concurrent syncs work: the bind is the reservation, so whoever gets
there second is refused by the kernel and takes another port — and a port held
by something that is not vmsync at all is handled the same way.

**Every port a run binds appears in its log** as
`target nbd port in use ... port=N`, as it is bound. The run also logs the
random offset it started from. Read a firewall log against those lines rather
than against any prediction.

**Passing a single port** makes it the *starting* point rather than the only
option: a multi-disk run has always spread upward from it, and now a busy port
inside that span is skipped instead of failing the run. Use it where firewall
rules are pinned to specific ports, and open as many as the table says the run
needs.

**`-source-nbd-bind` / `-target-nbd-bind`** are the addresses the exports
listen on, default `0.0.0.0`. Narrow them where the hosts have a dedicated
replication network. **`-source-nbd-host` / `-target-nbd-host`** are the
addresses vmsync *connects* to, derived from the URIs when unset — set them
when the host is reachable under a different name from the one in the libvirt
URI.

**Concurrency and range size.** With `vmsync-agent`, `max_concurrent_syncs`
multiplies straight into port usage: worst case
`max_concurrent_syncs × disks × 4`. The default 200-port target range covers
12 four-disk VMs replicating at once with everything enabled. Nothing needs
configuring for concurrent runs not to collide; the range just has to be big
enough, and a run that cannot find a port says so and names the range.

### Where the target's exports keep their sockets and pidfiles

Not a port question, but the same family of problem. Each target-side export
also writes a **pidfile**, and the pre-commit checksum export additionally
binds a **Unix socket** — all under `-target-runtime-dir`, default
**`/run/vmsync`**, created `0700` on the target if it is not there.

```
/run/vmsync/vmsync-qemu-nbd-<domain>-<dev>.pid            the write export
/run/vmsync/vmsync-checksum-qemu-nbd-<domain>-<dev>.pid   the checksum export
/run/vmsync/vmsync-checksum-<domain>-<dev>.sock           its socket
/run/vmsync/vmsync-verify-qemu-nbd-<domain>-<dev>.pid     the verify export
```

**Do not move this to `/tmp` or `/var/tmp`**, and be careful moving it
anywhere else. On a host running SELinux with `pam_namespace`, those two
directories are **polyinstantiated**: every SSH session gets a private mount
namespace with its own instance of them. vmsync drives the target over SSH, so
a socket created by one command is simply not there for the next, and a pidfile
written by the run that started an export is invisible to the run that has to
stop it. Nothing errors — the paths just resolve somewhere else — so it shows
up as the bridge helper failing to find the socket, and as exports that can
never be reclaimed because their pidfile was never visible to begin with.

`/run` is the default for three reasons: it is tmpfs, so a stale pidfile cannot
outlive a reboot and name a PID that now belongs to something else; default
`pam_namespace` configurations do not polyinstantiate it; and `/run/vmsync-locks`
is already created there over SSH for the target-side run lock, so any host
where that works is one where this works.

Change it with `-target-runtime-dir` (or `target_runtime_dir` in the agent's
config). It must be an absolute path — a relative one would resolve against
whatever directory the SSH session lands in, which works but puts the files
somewhere nobody looks, so it is refused at startup.

---

## Integrity: two different checks

vmsync has two, they answer different questions, and they deserve different
cadences.

| | question | when | flag |
| --- | --- | --- | --- |
| pre-commit checksum | *did this run land correctly?* | every sync, on by default | `-no-checksum` disables |
| `-verify` | *is this replica still intact?* | when asked | `-verify=...` enables |

### The pre-commit checksum (on by default)

Every chunk the copy reads is hashed as it passes. Before the commit,
`vmsync-bridge-helper` hashes the same ranges back **off the target** and the
two sets of digests are compared. If they disagree, an incremental sync's
overlay is *removed instead of committed*.

So a successful run means the bytes are on the target — not merely that the
writes were issued and nothing complained.

It costs no extra I/O on either side (the source bytes are already in the
copy's buffers; the target read is the only new work) and a few bytes per
megabyte on the wire. It needs a matching `vmsync-bridge-helper` on the
target — the same binary and `-bridge-helper-path` that `-compress` uses, but
no compression, buffering or bridge port is involved; it is run as a one-shot
command over a Unix socket, so it occupies **no port**.

If the helper is absent or a different version, the check is **skipped with a
warning** rather than failing the sync. Pass `-no-checksum` to state that
intent deliberately and silence the warning.

On a full sync there is no overlay to discard, so a mismatch cannot be undone;
the run still fails loudly, which is the whole value.

### `-verify`

After syncing, compares every disk on the target against **the same frozen
source snapshot the copy read from** — the backup job's export, still open. Both
sides are therefore the same point in time and must be byte-identical. No mode
is confused by a running guest.

| mode | what it does | cost |
| --- | --- | --- |
| `fast` | stops at the first differing range | the right choice for a scheduled run |
| `full` | scans the whole image, reports how many ranges and bytes differ | tells a bad cluster apart from a bad copy path |
| `qemu-img` | runs `qemu-img compare`, an **independent** implementation | the tie-breaker; **suspends the source** |

`fast` and `full` exchange digests rather than transferring the replica: vmsync
hashes the source while the helper hashes the same ranges locally, both at
once, and only digests cross the wire. Under `-no-checksum` they fall back to
byte comparators instead.

**Which side is the wall.** `fast` and `full` hash both sides *at once* — that
is what took a real pair from 45s to 27s — so the slower of the two is the
verify's cost. Two log lines say which, in the same fields:

```
nbd digest complete              export=vda  blocks=... bytes=... elapsed=27s  mib_per_sec=...
checksum: target digest complete disk=vda    blocks=... bytes=... elapsed=12s  mib_per_sec=...
```

The first is the source, read through the fleecing export. The second is the
target, and its `elapsed` is a **round trip**: it includes the SSH command and
the helper starting, which are milliseconds against a hashing pass measured in
seconds — but enough to explain an odd-looking number on a tiny delta. The
target line's `via` field says which check it belongs to: a Unix socket path is
the pre-commit check, `127.0.0.1:<port>` is the verify pass.

`qemu-img` is deliberately excluded from the digest path — it is the
independent oracle, and re-expressing it through vmsync's own digest code
would make it agree with vmsync by construction. It is the mode to reach for
when another mode reports a mismatch and you need to know whether to believe
it. It suspends the source only to keep the source snapshot's scratch space
empty for the duration, not because the comparison needs a stopped guest.

### `-verify-failure-reinit`

When `-verify` finds the replica differing from its source, recopy it in full
and verify it **again, once**. Requires `-verify`.

If that second verify also fails, the replica is recorded as **faulty** on its
own domain XML: later syncs into it are refused, and a promotion reports it as
untrustworthy, until a human clears it — with another `-verify-failure-reinit`,
or by discarding the replica with `-force-clean`.

Off by default, because the automatic response to "the replica does not match"
is to destroy its disks and copy them again, which is right when the cause was
a bad write and wrong when the cause will corrupt the recopy too.

Never retried beyond that one attempt. "Failed, and failed again after a full
recopy" already distinguishes a transient error from a real one, and each pass
destroys the previous one's evidence.

It is worth setting for a **scheduled** pair even though vmsync defaults it
off: a one-off run has an operator watching who can decide what to do about a
mismatch, and a scheduled one does not — so without it the next run syncs
straight over the finding.

**Cost to know before enabling estate-wide:** on a 50 GiB VM a mismatch turns
a ~30-minute run into a full recopy plus another full verify — hours, holding
an agent concurrency slot throughout.

---

## When a sync refuses, and what to do

vmsync refuses rather than risking a replica. Each refusal names itself in the
log; this is the map.

### The replication role

Every domain can carry a `replication_role` in its own vmsync metadata, and
vmsync **refuses to sync into any domain whose role is not `target` or unset**.

| role | means | how a sync gets past it |
| --- | --- | --- |
| `target` | the normal receiving side | — |
| *(unset)* | never recorded; a first sync may create it | — |
| `source` | this domain is the primary of its pair | **never overridden.** Your `-source-uri`/`-target-uri` are reversed |
| `promoted` | failed over to; serving live | `-update-role=target` (discards its disks), or `-force-clean` |
| `paused` | a person suspended replication — maintenance, or a restore | `-update-role=target`, or `-force-clean` |
| `fenced` | an automatic fence stopped it after a peer was promoted | usually `-invert`; or `-update-role=target` if the fence was wrong |

`paused` and `fenced` are distinct on purpose. `paused` means somebody chose
it and will resume when ready. `fenced` means nobody here chose anything and a
peer took over, so the pair's direction has probably reversed — resuming the
sync in the *same* direction would overwrite the copy that took over. `fenced`
can also be recorded while the domain is **still running**, which `paused`
never legitimately is: a fence that could not stop its guest records it anyway,
because at that moment it is the only thing refusing a sync into a live split
brain.

Set a role with `-update-role`, which changes it and exits. It addresses the
domain with `-target-uri`/`-target-domain` regardless of which direction it
currently replicates in. `-update-role=none` clears the field.

### A recorded verification failure

If `-verify` found this replica differing from its source and the finding has
not been cleared, syncs into it are refused — including a plain `-reinit`.

That last part is the non-obvious half. A reinit *does* recopy everything, so
it plausibly repairs the replica — but it does not verify the result, so
allowing it would move the domain from "known bad" to "assumed good,
unverified" while erasing the record that said otherwise.

Two deliberate acts get past it: `-verify-failure-reinit`, which recopies and
then proves the result before clearing anything, and `-force-clean`, which
discards the replica and logs that it threw a finding away.

The metadata records only the **verdict**. Which blocks differed is in that
run's log, in far more detail than a metadata field could hold.

### "Target file on system is newer" — out-of-band writes

Before an incremental, vmsync checks whether the replica's disks have been
written since its own last sync. Something writing to a replica behind
vmsync's back is a real hazard, and the check exists to catch it.

It compares the disk's mtime against `replica_written_at`, which vmsync
records **per disk, stat'd on the target host**, so both sides of the
comparison come from the same clock and drift cannot trigger it.

**`-timestamp-tolerance-sec`** is for a replica last written by an older
vmsync, where the only record is `last_sync_timestamp` taken on *this* host's
clock — a target running even a second fast then fails every incremental with
an error blaming out-of-band modification. Set it above the drift the error
reports, and pass it for **one run** rather than persisting it: that run
records `replica_written_at` for every disk it writes, even if it then fails,
which is enough to make every later comparison exact.

Fixing NTP is still the real repair.

### External snapshots on the source

libvirt forbids creating a checkpoint while an external snapshot exists on the
domain. If one appears — a backup tool, a manual `snapshot-create-as` — vmsync
cannot advance its chain.

**`-ignore-external-snapshot`** skips the run entirely in that case, cleanly
and early, rather than failing. Use it where something else legitimately takes
external snapshots and you would rather miss a sync window than log an error.

Without it, vmsync syncs against the existing checkpoint instead of creating a
new one, and says so.

### A wedged target: `-reinit` and `-force-clean`

**`-reinit`** discards the replica and does a full sync. It deletes the
target's disks, leaves its domain definition alone, and starts the chain
again.

**`-force-clean`** is `-reinit` for a target that is wedged. It additionally:

- removes the target **domain** before syncing rather than redefining it at
  the end, so a broken definition cannot block the run;
- overrides the `replication_role` interlock for a `promoted`, `paused` or
  `fenced` target, **discarding its current disks**;
- discards a recorded verification failure, logging that it did so;
- clears the source's checkpoint chain even when the source is shut down,
  removing the qcow2 bitmaps that would otherwise make every later sync fail
  with "Bitmap already exists".

It never touches a **running** target, and never overrides `role=source` —
that means the pair is configured backwards, and no amount of force makes
destroying the primary the intent.

**`-reinit-after-failures=N`** forces a full sync after N consecutive
failures. The count lives in the target's own domain XML, so it survives being
tracked from a different host.

Four things are exempt from that counter, because a full resync cannot fix
any of them and a non-zero count blocks promotion while it climbs: a
verification that ran and found a difference; the role refusal; the
recorded-verification-failure refusal; and an NBD export that could not be
reached.

---

## Disks on the target

**`-target-disk-path`** is the directory the replica's disks live in. Without
it, vmsync uses the same paths the source uses, which is rarely what a
separate target host wants.

**`-replaced-disk-action`** decides what happens to a target disk that is
about to be discarded and rebuilt — currently only `-reinit` does this.

- `rename` (default) moves it to `<path>.replaced-<unixtime>` so its contents
  survive.
- `delete` removes it.

The default is `rename` because the target of a reinit may be a former primary
whose disks still hold everything written after the last successful sync, and
that is unrecoverable once deleted. Renaming needs room for both copies, and
the aside files are **never reaped automatically**.

**`-target-disk-owner`** decides who owns disk files created on the target:
`auto` (default), `off`, or an explicit `user`, `user:group` or `:group`.

This exists because vmsync creates those files by running `qemu-img` over
SSH, so they are owned by that SSH user (usually root) — while qemu runs as
`qemu` on RHEL and `libvirt-qemu` on Debian, and cannot open a root-owned
disk. libvirt's `dynamic_ownership` usually hides this, but it is off in
plenty of deployments and cannot work at all on NFS with `root_squash`.

`auto` preserves whatever owned the file before — which is what makes
`-reinit` safe, since it replaces a correctly-owned disk with a fresh
root-owned one — and otherwise takes what the target's libvirt `qemu.conf`
sets. It never guesses, and warns instead. `off` is the old behaviour.

---

## Restore points

A sync faithfully replicates a source that has already gone bad. Restore
points are what you step back to.

**`-retention COUNT,INTERVAL`** — for example `24,3h` for twenty-four copies
at least three hours apart. Disabled by default.

The COUNT is a guarantee; the window it covers is **not**, because vmsync does
not decide when it runs. The interval is a floor — "take one if at least this
long has passed" — so a pair syncing every 4h gets 4h spacing, and a pause
leaves a gap.

Copies are made with **reflink**: they share storage with the replica and cost
almost nothing until they diverge. The target filesystem must support it (XFS
with `reflink=1`, or btrfs) and vmsync refuses the run at startup where it does
not, rather than silently making every copy a full one.

### Using them

**`-list-restore-points`** lists what is on the target and exits. Needs
`-target-uri` and `-target-disk-path`; reads the target filesystem only, and
touches neither the replica nor libvirt.

**`-clone-restore-point TAG -clone-to DIR`** copies one restore point's disks
to a directory and exits. This is how to answer *"is that copy clean?"* — boot
a throwaway domain from the clone. It changes nothing about the replica, its
metadata or its role. `-clone-to` is created if missing.

**`-restore-restore-point TAG`** puts one back over the replica **in place**,
discarding its current contents. `-target-disk-path` is optional here, unlike
the read-only verbs: a restore needs the target domain to exist, so where its
disks are is read from the domain itself.

Without **`-force-restore`** it only prints an assessment and changes nothing.
That is required because a restore replaces the replica's disks and cannot be
undone once the displaced contents are removed.

A restore leaves replication **paused**, because the next sync from the same
source would otherwise overwrite exactly what was rolled back to.
**`-restored-by`** records who asked for the rollback — the counterpart of
`-promoted-by`: a promoted domain's data-loss window says how far back its
contents are, but only this says somebody chose to put them there.

---

## Failover

A failover is two local operations rather than one remote one, and that is
deliberate. Promotion has to work when the primary site is unreachable, so it
cannot depend on reaching anything. It also keeps the credential graph as it
is: every SSH path vmsync provisions runs source→target, and a DR host holding
credentials that can shut down production VMs is a much worse thing to own
than a small restriction on where a command may be typed.

So the failover verbs refuse a remote URI and must run on the host of the
domain they act on. `-read-fence` is the one exception, because asking the
other site *is* the operation.

### Planned

```bash
# on the source's host
vmsync -shutdown-domain -target-uri qemu:///system -target-domain web01

# on the target's host
vmsync -promote -target-uri qemu:///system -target-domain web01 \
       -promote-mode planned -promoted-by "$USER" -start
```

`-shutdown-domain` stops the domain cleanly and records `role=paused`. It
**never** destroys a guest that ignores ACPI: a graceful shutdown that does not
complete means the guest is not responding, and pulling its power is a decision
with consequences inside that guest, so it belongs to a person rather than to a
timeout expiring. **`-shutdown-timeout-sec`** (default 300) is how long it
waits; on expiry it fails and leaves the domain running.

The role is recorded even when the shutdown fails. Every caller of this mode is
asking for one thing — stop this domain and suspend its replication — and there
is no reason to make half of that conditional on the other half succeeding.

### Forced

```bash
vmsync -promote -target-uri qemu:///system -target-domain web01 \
       -promote-mode forced -promoted-by "$USER" -start -fence-source
```

`-promote-mode` is recorded on the domain: `planned` when the source was
cleanly shut down first (nothing lost), `forced` when it was never reached.
Only a **planned** promotion of a source that was recorded as stopped when its
checkpoint was taken can honestly claim a zero data-loss window; everything
else reports a lower bound, or "unknown".

`-promote` refuses unless the target actually holds a usable replica — missing
disks, no completed sync, an interrupted copy. **`-force-promote`** proceeds
anyway, and the data-loss window is then reported as *unknown* rather than
guessed.

### Fencing the displaced source

`-fence-source` with `-promote` arms a fence so the displaced source shuts
*itself* down, instead of leaving one VM running in two places. Bare
`-fence-source` takes the source from the target's own `replica_source`; an
explicit `host:domain` names it directly.

Off by default, because a DR drill is a promotion too and must not stop
production.

The promoted domain records the decision. The source's `vmsync-agent` notices
it and acts on it **once, ever** — a fence that failed is not retried, because
the realistic failure is a guest ignoring ACPI, and retrying that on a timer
means either an unbounded queue of pending shutdowns or an escalation to
destroying a running VM. Neither is a decision an unattended agent should reach
by repetition.

The agent carries out the fence with **`-fence-domain`**, which is
`-shutdown-domain` differing only in what it records: `role=fenced` rather than
`role=paused`. Same shutdown, same refusal to destroy a guest.

**`-read-fence`** asks the peer named by `-target-uri`/`-target-domain` whether
its promotion armed a fence against this host, and prints the answer as JSON.
Reads only. An unreachable peer is reported as *unreachable*, not as an absence
of fencing.

### Afterwards

**`-invert`** reverses a pair's direction: `-source-uri`/`-source-domain` name
the OLD source, `-target-uri`/`-target-domain` the promoted replica. Run it on
the old source's host. It swaps the roles, moves the replica bookkeeping, and
drops checkpoint objects that are meaningless once the direction reverses.

---

## Reporting to something else

**`-prometheus-textfile`** writes this run's metrics in
textfile-collector format. Point it somewhere node_exporter reads, e.g.
`/var/lib/node_exporter/textfile_collector/vmsync_web01.prom`.

The metrics that repay attention: `vmsync_sync_state`,
`vmsync_verification_state` and `vmsync_checksum_state`. The last two are
tri-state — passed, mismatch, or *not performed* — because "the comparator
could not run" and "the replica differs" call for opposite responses and must
not be the same number.

**`-result-json`** writes this run's *degradations* to a path as JSON, for a
supervising agent to read back. A degradation is something an exit code cannot
carry: a guest left frozen by a failed thaw, or a copy that is
crash-consistent because the freeze did not take. Both can happen to a run
that otherwise succeeds.

**`-run-id`** is an opaque identifier written into the run lock, so a
supervising agent can join it to its own record of having started the process.

`vmsync-agent` sets all three. Nothing needs them when vmsync is run by hand.

**`-debug`** enables debug logging, which includes every remote command
vmsync runs.

---

## Deliberate faults, for testing

**`-test`** makes vmsync fail at a chosen point, so error-recovery paths that
cannot be reached from outside the process can be exercised. A run with this
set **will** fail and its result means nothing as a replication.

It is documented rather than hidden, so an operator who finds one in a log can
look it up.

| fault | what it does |
| --- | --- |
| `failure-define` | makes the target's redefine fail, exercising the rollback to the previous definition |
| `corrupt-before-checksum` | writes garbage into the image the copy just wrote, just before the pre-commit check reads it back |
| `corrupt-after-commit` | writes garbage over the committed replica after that check passed, so `-verify` finds a genuine mismatch |

**The two `corrupt-` faults damage real data**, and are not merely failure
injection:

- `corrupt-before-checksum` is expected to be *caught*. On an incremental it
  costs only the discarded overlay; a full sync has no overlay, so the
  replica's base is left damaged. It requires the pre-commit check to be
  running, and writes inside a range the run actually wrote — the check only
  hashes those.
- `corrupt-after-commit` leaves the replica genuinely not matching its source.
  It must be rebuilt, and the verification failure is recorded on it like any
  other. It requires `-verify`, which is what turns the damage into a test
  result.

Both need `qemu-io` on the target host, checked before the copy rather than
after it.
