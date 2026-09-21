# vmsync
 
incrementally replicate libvirt based virtual machines to remote hosts using
dirty bitmaps.

This utility can be used to sync or "replicate" virtual machines to other
libvirt hosts. On the first execution, a complete (full) replication will be
executed and an dirty bitmap is created. For any following command calls, it
will only synchronize incremental changes since the last checkpoint.

**[docs/MODES.md](docs/MODES.md)** is where to start if you are new. vmsync can
be run by hand, from cron, from a parallel launcher, or by an agent with or
without a web control plane — that page is the map of what each gives you, with
a table of which features exist in which mode and a quick start for each.

**`vmsync -help`** lists every flag in one line each, grouped by what it is
for — actions, connection, ssh, sync, storage, transport, ports, integrity,
restore points, failover, reporting.

**[docs/RUNBOOK.md](docs/RUNBOOK.md)** is what to type when something needs
doing by hand: planned and unplanned failover, fencing, reversing a pair,
undoing a promotion, rolling back to a restore point, and recovering from each
of vmsync's refusals. Commands, in order, with the host each runs on.

**[docs/HOWTO.md](docs/HOWTO.md)** is the other half: why the defaults are
what they are, what breaks if you change them, and which combinations are
refused. It is organised by task, so if `-help` said "see HOWTO", the heading
you want is in its table of contents.

# Workflow
 
The current operation workflow is:

 * Identify the virtual disks (qcow2 images only)
 * Create a checkpoint and pull based libvirt backup job, expose checkpoint via NBD
 * Connect to the source NBD port and query block regions
 * Connect to the remote system via SSH and create qcow files on target system
 * Start an NBD target service backing the qcow2 files on the target system
 * Synchronize block regions
 * Stop backup job
 * Stop target NDB Service
 * Define VM on target system with latest configuration

# Screenshot

![Alt text](screenshot.jpg?raw=true "Title")


# Example command:

```
./vmsync -source-domain SOURCE_DOMAIN \
        -source-uri qemu+ssh://hostA/system \
        -target-domain TARGET_DOMAIN \          # optional, otherwise use original VM name
        -target-uri qemu+ssh://hostB/system \
        -ssh-user root                          # user for all ssh related
```

# NBD ports

Every port a run uses is derived from two base ports. `-source-nbd-port`
and `-target-nbd-port` each accept two forms, and **default to a range**:

```bash
vmsync                                 # 10809-10908 / 20809-21008, the defaults
vmsync -target-nbd-port 20000-20100    # pick a free block inside this range
vmsync -target-nbd-port 20809          # pin one exact base port
```

With a range, vmsync asks the host which ports are listening and takes a free
contiguous block, so two concurrent syncs to the same host work with nothing
configured. Pinning a single port is worth doing only for a firewall that
cannot open a range — two runs then both try that port and one fails to bind.

The default used to be a single fixed port, with the range reachable only by
typing `auto`. That was the wrong way round: a range costs nothing when one
run is active and is the difference between working and colliding when two
are. `auto` is gone as a keyword, since it now means exactly what passing
nothing means; it is still accepted for one release so existing unit files
keep working.

How many ports a run occupies, for `N` disks:

| side | plain | `-compress`/`-netbuffer` | `-verify` | both |
| --- | --- | --- | --- | --- |
| target | `N` | `2N` | `2N` | `4N` |
| source | 1 | 2 | 1 | 2 |

Source counts are per **run**, not per disk — `-verify` reuses the same source
export rather than opening a second one — so four concurrent syncs use four to
eight ports of the hundred in the default range.

**Target ports are not contiguous.** Each export binds its own port and logs
which one it got, so a 4-disk VM with compression and verification needs 16
ports *anywhere* in the range rather than a 16-port consecutive span. That is
what makes two concurrent syncs work: the bind is the reservation, so whoever
gets there second is told no by the kernel and takes another port, and a port
held by something that isn't vmsync at all is handled the same way.

`-verify` used to cost `3N` here rather than `2N`. That was not what it bound
— it bound `2N` — but the old fixed-offset layout put the verify block at
`+2N` whether or not bridging was on, so the middle block was reserved and
left idle. A 4-disk `-verify=qemu-img` run with no compression demanded twelve
consecutive ports to use eight.

Size ranges from the largest VM you replicate, not the average. If a run wants
more ports than the range holds, vmsync says so up front, and the export that
runs out names the range and the shortfall.

Each run starts at a **random** offset inside the range. Not to reduce
collisions — a collision is already handled, since the run just binds the next
candidate — but because an offset derived from the VM's name makes a VM
*permanently* unlucky: if it hashes to where some unrelated long-lived service
sits, it starts there on every run for the life of the deployment and pays the
same wasted attempts each time. A random start makes that a one-off.

The cost is that a VM no longer lands on the same ports run after run. That
matters less than it sounds: every port a run binds appears in its log as
`target nbd port in use ... port=N` as it is bound, and the run logs its
starting offset too — so a firewall log is read against what actually
happened rather than against what a hash predicted.

Passing a fixed port makes it the **starting** point rather than the only
option: a multi-disk run has always spread upward from it, and now a busy port
inside that span is skipped instead of failing the run. Use it where firewall
rules are pinned to specific ports, and open as many as the table above says
the run needs.

# Replication roles and failover

Each VM can carry a persistent `replication_role` in its own vmsync
metadata. vmsync **refuses to sync into any domain whose role is not
`target`**:

| role | syncing into it | meaning |
| --- | --- | --- |
| *(unset)* | allowed | no role recorded — the state of every VM that predates this feature |
| `target` | allowed | normal receiving side of a replication pair |
| `source` | refused | direction is reversed; syncing in would overwrite the original with its own replica |
| `promoted` | refused | failed over to, now serving live |
| `paused` | refused | replication administratively suspended |

Set it with `-update-role`, which changes only that one field and exits
without syncing anything:

```bash
vmsync -target-uri qemu+ssh://hostB/system -target-domain myvm -update-role promoted
```

Roles are opt-in. vmsync never assigns one by itself, so nothing changes
for an existing setup until you set one.

**Why this matters.** vmsync already refuses to overwrite a target that is
*currently running*. That is not enough on its own: a VM you failed over
to, and then shut down for ten minutes of maintenance, looks exactly like
an ordinary idle target. The next scheduled sync from the old source would
overwrite live data with a stale replica — and if `-reinit-after-failures`
had been counting up during the failover, what fires is not an incremental
sync but a full reinit, which removes the target's disks first. Marking the
promoted VM `promoted` makes the refusal permanent and independent of
whether it happens to be powered on.

The check lives in vmsync itself, not in whatever schedules it, so cron
jobs, manual invocations and any external tooling are all bound by it. It
is also re-checked immediately before the target's definition is rewritten,
not only at startup: a sync runs for minutes or hours, and a role set part
way through would otherwise be silently reverted by the run that was
already in flight when it was set.

**`-promote` is gated separately, on evidence.** The role says what a domain
*is*; promoting it also asks whether there is a usable replica there to make
live. `-promote` refuses on missing disks, no completed sync, no recorded
source, an uncommitted overlay left by an interrupted copy, a non-zero
`failure_count` — and on a recorded verification failure, which is the one
reason that means the copy is *wrong* rather than merely *stale*: a `-verify`
compared it against its source, found the contents differing, and stamped
`verify_state` on the domain. `-force-promote` proceeds past any of them and
then reports the data-loss window as unknown rather than guessing; it does not
clear the verification record, so a replica forced live stays marked as one
known not to match. [docs/RUNBOOK.md](docs/RUNBOOK.md) has what to do about
each refusal instead.

## Two runs, one target

vmsync takes two run locks. The first is on the **source** host, keyed by
the source domain, and serialises that domain's checkpoint chain. The
second is on the **target** host, keyed by the target domain, and is what
stops anything else acting on a domain while it is being written — two
different sources replicating into one target, or a sync overlapping some
other operation on the same domain, are collisions the source-side lock
cannot see.

The target-side lock is held over the SSH connection for the run's
duration. That is deliberate: it is released when the connection closes,
which covers a clean exit, a `SIGKILL` and a network partition alike. There
is no lease to renew and no stale lock to clear by hand after a crash. It
does require `flock(1)` on the target host; vmsync refuses to start rather
than run unprotected if it is missing.

## Replacing a target's disks

`-reinit` discards the target's disks and copies everything again.
`-replaced-disk-action` chooses what "discard" means:

| Value | Effect |
|-------|--------|
| `rename` (default) | each disk is moved to `<path>.vmsync-replaced-<unixtime>` |
| `delete` | each disk is removed |

The default is `rename` because the two mistakes are not symmetric. A
reinit's target may be a former primary whose disks still hold everything
written between the last successful sync and the moment it went down —
exactly the data a failover accepted losing. Deleting that is
unrecoverable; keeping it costs disk space and a file to clean up later.
Nothing reaps the aside files: that is deliberately a decision, not a
background job.

## When a reinit itself will not go through

`-force-clean` is a `-reinit` for a target that is wedged. It implies
`-reinit` and removes three obstacles that stop an ordinary one:

| Obstacle | What `-force-clean` does |
|----------|--------------------------|
| The target domain's own definition is the problem — a half-applied redefine, a UUID collision, checkpoint metadata libvirt will not undefine around | Undefines the target domain up front, instead of letting `DefineDomain` replace it at the end |
| The target is marked `promoted` or `paused`, so the role interlock refuses the sync | Overrides that refusal, **discarding the replica's current disks**, and says so in the log |
| The source is shut down, so libvirt refuses to delete its checkpoints ("cannot delete checkpoint for inactive domain") | Removes the qcow2 bitmaps with `qemu-img` first, then the metadata — always both |

That last one matters beyond this flag. A checkpoint *is* a persistent
bitmap in the qcow2, and dropping libvirt's record of it without removing
the bitmap leaves the image carrying a bitmap named after a checkpoint
libvirt no longer knows about. The next sync restarts its chain at
`vmsync-cpt-000001`, qemu refuses with `Bitmap already exists`, and
`virsh checkpoint-list` shows nothing at all. If you ever land in that
state, the way out is per disk:

```bash
qemu-img bitmap --remove -f qcow2 /path/to/disk.qcow2 vmsync-cpt-000001
```

Two refusals `-force-clean` does **not** override:

- **A running target.** Undefining a domain and replacing its disks under a
  live guest corrupts what that guest is writing, and re-running nothing
  undoes it. Shut it down first.
- **`replication_role=source`.** That says the domain is the primary of its
  pair, so syncing into it would overwrite the original with its own
  replica. It means `-source-uri` and `-target-uri` are the wrong way
  round, which is not a mess to clean up. An unrecognised role — one
  written by a newer vmsync — fails closed for the same reason.

If the sync then fails, there is no target domain left. That is the trade
being asked for by name, and it is why this is a separate flag rather than
something `-reinit` escalates to on its own. Through the agent it is a
distinct operation kind (`force-clean`, not `reinit`) so the audit trail
records which of the two actually ran.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | the run succeeded — or stood down cleanly because another vmsync already held the lock for this domain |
| 1 | the run failed |
| 2 | the flags were wrong; nothing was attempted |
| 75 | **an operator verb** stood down without doing anything, because another vmsync held the lock — retry when it finishes |

A **sync** that finds the lock held exits 0 and writes no metrics record. It is
not a failure: another vmsync is doing the work right now, and a scheduler
running every few minutes simply tries again. Counting it would climb
`-reinit-after-failures` on a perfectly healthy replica — and a non-zero
failure count also blocks promotion.

An **operator verb** — `-restore-restore-point` in particular — cannot use 0
for that, because 0 would report a rollback that never happened as done, and 1
would report one that never started as failed. 75 is `EX_TEMPFAIL` from
`sysexits.h`, which means precisely "temporary failure, retry later". The agent
reads it and defers the operation rather than recording a terminal result,
which is what lets the same operation succeed on a later attempt instead of
having to be reissued by hand.

## Clock drift between the two hosts

Two different checks, with very different consequences.

**The clock comparison is advisory.** Before each sync vmsync compares its own
clock with the target's and warns past 30 seconds. It never refuses: a sync
with skewed clocks still copies the right bytes.

**The out-of-band-modification check is not.** Before each incremental sync
vmsync compares every replica disk's mtime against the newer of two recorded
values, and refuses if the file looks newer still, on the reasoning that
something wrote to the replica behind its back:

- `replica_written_at` — per disk, when vmsync itself last wrote that file,
  taken with `stat` **on the target host**. Written whenever the disks are
  written, whether or not the run then goes on to succeed.
- `last_sync_timestamp` — one value for the pair, written only when a whole
  run succeeds, by whichever host ran vmsync.

Where a `replica_written_at` entry exists, both sides of the comparison come
from the *same* clock — the target's — so the check is exact and clock drift
cannot trigger it. That is the normal case once a replica has synced under a
vmsync that records it.

**Two older failure modes, both now closed for such replicas.** The first was
clock drift: `last_sync_timestamp` is the *source* side's clock while the mtime
is the *target's*, so a target running even a second fast failed the check on
every incremental sync, permanently, blaming a modification that never
happened. The second was self-inflicted: `last_sync_timestamp` is only written
on a fully successful run, so a run that copied the disks and then failed — a
failed `-verify`, most often — left every replica disk freshly written and the
timestamp untouched. Every later run then refused, and refused again, because
each refusal happened *before* the copy that would have moved the timestamp on.
One failed verify wedged the pair.

A replica that predates `replica_written_at`, or a disk with no entry yet,
falls back to `last_sync_timestamp` alone and behaves exactly as before —
including both failure modes above, until one successful sync records a stamp.
That is what the tolerance below is still for.

```
-timestamp-tolerance-sec=60
```

is the way back without a full `-reinit`, for a pair wedged by a vmsync that
predates `replica_written_at`. It is how far the mtime may be ahead before the
check fires, and it defaults to `0` — an exact comparison, which is the
behaviour that predates the flag. The error names the drift it actually saw, so
one failed run tells you what to set.

**Pass it for one run, not forever.** That run only has to *reach* the disk
copy: it records `replica_written_at` for every disk it writes even if it then
fails again, so a single run is enough to make every subsequent comparison
exact. Leaving it in a config or a schedule entry keeps a real check narrowed
long after the reason for it is gone.

Fixing NTP is the real repair. This narrows the check rather than removing it:
an out-of-band write is normally minutes or hours after a sync, not inside the
window two NTP clients disagree by. Values above an hour are refused by the
control plane, because past that the check stops catching anything.

It is settable per pair on the schedule page ("clock tolerance"), since the
outage it ends is one an agent-managed estate hits hardest — every scheduled
sync for that pair fails, and a knob only reachable by hand-running vmsync
would not fix a schedule.

## Restore points: going back to an earlier replica

A replica answers "what does the source look like now". It cannot answer
"what did it look like before the source was ransomwared", because the sync
that copied the damage was working perfectly. `-retention` keeps a series of
earlier copies on the target so there is something to go back to.

```
vmsync ... -retention=24,3h      # keep 24, take one at most every 3 hours
```

They live in a `.vmsync-rp/` subdirectory beside the replica's own disks,
named by the instant and checkpoint they correspond to, each with a
`status.json` recording what is known about it. Each is a **reflink copy**:
it shares extents with the replica rather than duplicating it, so taking one
costs milliseconds and a few metadata blocks whatever the image size, and
twenty-four of them cost roughly one image plus the deltas between them.

That is also the constraint. `-retention` is **refused** on a filesystem that
cannot reflink (use XFS with `reflink=1`, the default on RHEL 8+, or btrfs)
rather than silently making twenty-four real terabytes out of a 1 TB replica.

The interval is a **floor, not a schedule.** vmsync does not decide when it
runs — cron or the agent does — so `3h` means "not more often than every
three hours", never "every three hours". If syncs run every four hours the
copies are four hours apart. The count is the guarantee; the window it
nominally covers is not.

Measure the cost with `df`, never `du`. `du` counts a shared extent once for
*every* file referencing it, so it reports a directory of restore points at
many times its real size, and capacity alerting built on it will page someone
about a disk that is nearly empty.

### Looking at one

```
vmsync -target-uri ... -target-domain web01 -target-disk-path /data/replicas -list-restore-points
vmsync ... -clone-restore-point 1756041600-vmsync-cpt-000042 -clone-to /var/tmp/inspect
```

All three restore-point verbs act on the host holding the disks, so they work
both ways round: over `qemu+ssh://` from elsewhere, or with a local
`qemu:///system` when run on the target itself — which is where an operator
during a site failure usually is. A `qemu+tcp://` URI is refused: it reaches
libvirt but offers no way to run a command on that host, and running one here
instead would touch the wrong machine's filesystem.

`-clone-restore-point` copies that point's disks where you ask and stops. It
changes nothing: not the replica, not its metadata, not its role,
`last_checkpoint` stays valid. Point a throwaway domain at the clone and boot
it. During an incident the real question is usually "is Tuesday's copy
clean?" rather than "make the replica be Tuesday", and this answers it
without touching replication at all.

### Putting one back

```
vmsync ... -restore-restore-point 1756041600-vmsync-cpt-000042                  # assess
vmsync ... -restore-restore-point 1756041600-vmsync-cpt-000042 -force-restore   # do it
```

`-target-disk-path` is optional here, unlike the two verbs above. A restore
refuses without the target domain — rolling the disks back and failing to
invalidate the domain's replication metadata is the one outcome the next sync
cannot detect — so where its disks live is read off the domain itself. Give the
flag anyway and it wins, and is checked against that domain.

Without `-force-restore` this only prints an assessment — every file it would
replace, where the current contents would go, and the exact replication state
that would follow — and writes nothing. It is safe to run while deciding.

**A restore is done in order to promote.** If the goal were to resume
replicating, restoring would be pointless: the next sync from the same source
overwrites the restored data with exactly what you rolled away from. So a
restore leaves the replica **paused** (`replication_role=paused`), and the
next sync refuses with a message saying so instead of quietly undoing it.
A paused domain is still promotable — that is deliberate, and predates this
feature.

The metadata is rewritten to describe the restored point rather than cleared,
so `-promote` reports an honest data-loss window measured from when that copy
was taken, instead of refusing for lack of evidence.

The replica's displaced contents follow `-replaced-disk-action`: kept at
`<path>.vmsync-replaced-<unixtime>` by default, removed with `delete`.
Restoring never consumes the restore point, so a first choice that turns out
to be wrong can be followed by a second.

To go back to replicating instead of promoting: `-update-role=target`, then a
`-reinit` full sync. That rebuilds from the source and **discards what was
just restored** — which is the point of it being an explicit two-step act.
(`-force-clean` does both at once, because it ignores the `paused` role a
restore leaves behind. Convenient, and correspondingly easier to fire by
accident — see [When a reinit itself will not go
through](#when-a-reinit-itself-will-not-go-through).)

### Promoting a copy from six hours ago

There is no separate command for it: restore, then promote.

```
vmsync ... -restore-restore-point <tag> -force-restore -restored-by alice
vmsync -promote -target-uri qemu:///system -target-domain web01 -start
```

The promotion's data-loss window is measured from the restore point's own
checkpoint, so an eleven-hour rollback reports eleven hours rather than the
minutes since the last sync. `-restored-by` is the counterpart of
`-promoted-by`: the restore records `restored_from`, `restored_at` and
`restored_by` on the domain itself, and they survive the promotion. Without
them a promoted domain shows only an unusually wide loss window, with nothing
saying somebody chose it — and a control plane's audit log does not survive
losing the control plane.

They are cleared by the next successful full sync, because a replica that has
been completely recopied is no longer a restored one.

Not covered: UEFI varstores. A restore point holds disks and nothing else, so
rolling a UEFI guest's disks back leaves its NVRAM at present-day state.

## Reversing a pair

`-invert` swaps which end is the source, after the replica has been promoted.
**The old source must be shut down first.** The inversion turns it into a
replication target, and a running target is one scheduled sync away from being
overwritten under a live workload — so vmsync refuses rather than stopping a
production domain as a side effect of a metadata command.

It also drops that domain's checkpoint chain, which described a chain running
the other way and which a later fail-back would otherwise chain onto.

That is more involved than it sounds, and it is why **`-invert` must run on the
old source's own host, with a local `-source-uri`** — the same restriction
`-promote` and `-shutdown-domain` already have. Deleting a checkpoint normally
merges its dirty bitmap into the next one, which only a running qemu can do;
the domain here is shut down by definition. So vmsync removes the bitmaps from
the images directly with `qemu-img`, then drops libvirt's record of the
checkpoints — and it can only reach those images where they are. Given a remote
`-source-uri` it refuses and says so rather than doing half the job.

Both halves always happen together. Doing only the metadata half leaves each
disk carrying a bitmap named after a checkpoint libvirt no longer knows about,
so the next sync restarts its chain at `vmsync-cpt-000001` and qemu refuses —
"Bitmap already exists" — for that pair, permanently, with
`virsh checkpoint-list` showing nothing that would explain it. If you ever meet
that state, `qemu-img info` on each disk shows the orphans and
`qemu-img bitmap --remove -f qcow2 <disk> <name>` clears them, with the domain
shut off.

## Disk paths across an inversion

`-target-disk-path` says where **this direction's** replicas go. After an
inversion that direction has reversed, so the same value now names where the
new *source* keeps its disks — not where the new *target* keeps its own.

With an asymmetric layout:

| | before invert | after invert |
| --- | --- | --- |
| A `/var/lib/libvirt/images/web01.qcow2` | source | **target** |
| B `/data/replicas/web01.qcow2` | target (`-target-disk-path /data/replicas`) | source |

Reusing `-target-disk-path /data/replicas` on the reversed sync aims the copy
at `/data/replicas` **on A**, which is not where A keeps its disks. Either it
fails because that directory does not exist there, or — where it does — the
replica is written to it, the domain is redefined to match, and A's original
disk is silently orphaned: still on disk, still consuming space, no longer
referenced by anything.

Symmetric layouts are unaffected. With `-target-disk-path` unset the target
path is the source's own path, which survives an inversion by itself.

Two halves handle this:

- **The control plane re-aims it.** When an inversion completes, the schedule
  entry that moves to the new source gets its `target_disk_path` recomputed
  from where the new target's disks actually are, read out of that host's own
  report. Left alone when the disks span more than one directory, since a
  single value cannot express that in either direction — the entry arrives
  disabled regardless, so an operator sees it before it runs.
- **`vmsync -invert` warns**, naming the value to use. It cannot do more: it
  does not run the reversed sync and owns no schedule, so the next
  invocation's flags are the operator's to type.

## Who owns the target's disks

vmsync creates the target's disk files by running `qemu-img` over SSH, so
they belong to **that SSH user** — root, in any realistic deployment. qemu
does not run as root: it is `qemu` on RHEL and Fedora, `libvirt-qemu` on
Debian and Ubuntu. A root-owned disk is therefore one the promoted domain
may be unable to open, and that is discovered during a failover, on the copy
that was supposed to take over.

libvirt's `dynamic_ownership` normally chowns disks as it starts a domain,
which is why this can go unnoticed for a long time. It is not something to
rely on: it is disabled in plenty of deployments, and on **NFS with
root_squash it cannot work at all** — which is exactly where a DR replica
often lives.

`-target-disk-owner` decides what vmsync does about it:

| Value | Effect |
|-------|--------|
| `auto` (default) | Resolve it, in the order below. |
| `user`, `user:group`, `:group` | Force exactly this. |
| `off` | Never chown. The behaviour before this existed. |

`auto` resolves in three steps, strongest evidence first:

1. **Whatever owned the file before.** Applies from the second sync onward,
   and to every `-reinit`.
2. **`user`/`group` in the target's `/etc/libvirt/qemu.conf`** — but only if
   *uncommented*. Every distribution ships that setting commented out, so
   this usually finds nothing.
3. **Which well-known qemu account the target host actually has** —
   `qemu` (RHEL, Fedora, SUSE) or `libvirt-qemu` (Debian, Ubuntu), via
   `getent`, so an account in LDAP or SSSD is found too. This is what covers
   a **first-ever sync**, which reaches nothing else.

Step 3 is inference rather than configuration, and it is worth being precise
about why acting on it is nonetheless right: **being wrong is no worse than
doing nothing.** The file is root-owned either way, and root-owned is already
unusable by a non-root qemu; if libvirt happens to run qemu as root, a
`qemu`-owned file is still perfectly openable by it. Doing nothing, by
contrast, leaves a replica that cannot boot — found during a failover.

A host carrying *both* accounts is reported rather than resolved silently,
and a host carrying neither gets the same warning as before. Set the flag
explicitly on those.

**Why `auto` preserves rather than detects first.** After the first sync,
whatever owns the file is ownership that demonstrably worked — this pair has
synced before, and libvirt has been opening these files. That is evidence,
where a configuration lookup is inference.

**The sharper problem it fixes is `-reinit`.** That renames the existing,
correctly-owned disk aside (or deletes it) and creates a fresh root-owned one
in its place — silently converting a replica that *was* bootable into one
qemu cannot open. `auto` remembers the displaced file's ownership before it
is moved and restores it afterwards, so a reinit is no longer a way to break
a working replica.

Only the base image is chowned, and only on a **full** sync. An incremental
overlay is written by the same SSH user that created it and is committed and
deleted before any domain starts; and an incremental leaves the base in place
— `qemu-img commit` writes into it without touching its ownership — so a
chown there would only overrule something an administrator or the storage
layer had set deliberately.

Where nothing at all can be determined, vmsync **warns and leaves the file
alone**, naming the flag to set and what to set it to.

A role this build does not recognize is also refused, on the assumption it
was written by a newer version — failing closed costs an error message,
failing open would cost data.

## Fencing: not running one VM in two places

Promoting a replica does not, by itself, stop the original. If the old
source is still up — a partition rather than an outage, say — the same VM is
now serving in two places, both accepting writes, diverging. Nothing
detects this later and nothing merges it.

`-fence-source` makes a promotion authorise stopping the displaced source:

```bash
vmsync -promote -start -fence-source -target-uri qemu:///system -target-domain web01
```

Bare `-fence-source` takes the source from the target's own
`replica_source`; `-fence-source=host:domain` names it explicitly. The
promotion records a token on the promoted domain:

| field | meaning |
| --- | --- |
| `fence_source` | the one `host:domain` that must not run |
| `fence_id` | this specific decision, used once and never again |
| `fence_armed_at` / `fence_armed_by` | when, and by whom |

The displaced host's agent reads that token **from the promoted domain's own
libvirt** and shuts itself down — a clean guest shutdown that never falls
back to destroying the domain, followed by `replication_role=paused`.
Exactly what `-shutdown-domain` does, because it is `-shutdown-domain`.

**Why a token rather than working it out.** A DR drill, a real failover, and
a promotion somebody resolved by hand months ago all leave identical
metadata: `role=promoted` and a `promoted_from` naming the source. Two of
those three must not stop anything. The difference is intent, which is not
observable after the fact — so it is recorded at the moment the decision is
made. **A promotion without `-fence-source` authorises nothing**, which is
what makes drills safe.

Five conditions must all hold before anything is stopped, and each rules out
a specific way of stopping a VM that should have stayed up:

1. a fence was armed at all — otherwise it was a drill;
2. the token names **this** host — a token can never take down a bystander;
3. the peer is still `promoted` — otherwise the failover is over;
4. the peer is **running** — stopping this one while the promoted copy is
   down would leave *zero* copies serving, and "promoted but not started" is
   a real state, since promotion writes metadata before booting;
5. this token has not been acted on before.

That last one is a durable, per-agent latch keyed on `fence_id`, written
*before* the attempt. **A fence that failed is not retried.** The realistic
failure is a guest ignoring ACPI, and retrying that on a timer means either
an unbounded queue of pending shutdowns or an escalation to destroying a
running VM — a decision no unattended agent should reach by repetition. It
stays visible instead: `vmsync_agent_split_brain` stays at 1 and the log
keeps saying so until a person resolves it.

An **unreachable** peer is never treated as an absence of fencing. A
partition is precisely when a promotion is most likely to have happened and
least likely to be visible, so silence means "keep serving" — the safe
direction — and the check simply runs again later.

Reading a peer's token without acting on it:

```bash
vmsync -read-fence -target-uri qemu+ssh://dr01/system -target-domain web01
```

The only failover mode that accepts a remote URI, because asking the other
site *is* the operation. It reads and prints JSON; it changes nothing. An
unreachable peer is reported as `"reachable":false` and still exits 0, so
"the peer says no fence" and "the peer could not be asked" never collapse
into one answer.

# Limitations

 * Both source and target libvirt host should run on the same libvirt/qemu
   version/distribution
 * Both source and target hosts need to be time synced via NTP or else
 * The utility at its current state does not copy custom specified
   kernel/tpm devices
 * Special devices like iso files attached to the cdrom are not copied
 * UEFI varstores **are** copied, and repathed for the target domain — see below

## UEFI guests: the varstore

A UEFI domain has two firmware files, and only one of them belongs to the VM:

| element | example | copied? |
| --- | --- | --- |
| `<loader>` | `/usr/share/OVMF/OVMF_CODE_4M.fd` | **no** — read-only firmware from the target host's own OVMF package |
| `<nvram>` | `/var/lib/libvirt/qemu/nvram/web01_VARS.fd` | **yes** — the boot entries and any enrolled Secure Boot keys |

The varstore is replicated on every successful sync, and only when its contents
actually differ from the replica's. Without it a UEFI replica had no boot entry
for its own bootloader — a Windows guest registers
`\EFI\Microsoft\Boot\bootmgfw.efi`, and a fresh varstore has no such entry, so
the replica booted only if the fallback `\EFI\BOOT\BOOTX64.EFI` happened to
exist — and a guest with enrolled Secure Boot keys lost them outright. Both are
the kind of thing you discover at failover.

It cannot be copied atomically: qemu holds it open as a pflash device and
nothing outside qemu can lock it. Rather than pause the guest for a 128 KiB
file, vmsync reads it, hashes what it read, and re-hashes the source; a write
landing mid-read makes those disagree and the read is retried. A torn copy is
therefore never installed. On the target it is written to a temp file, verified,
chowned, and only then renamed into place, so the replica never holds a
half-written varstore.

A failure here never fails the sync — the disks are already committed and
correct — but it is always reported, because a DR copy that will not boot is not
something to find out later.

### The varstore path follows the domain name

libvirt derives this path from the domain **name**
(`/var/lib/libvirt/qemu/nvram/<name>_VARS.fd`), so when `-target-domain`
differs from `-source-domain` the replica needs its own file. vmsync repaths it
for the target, in the same place and for the same reason it repaths the disks:

| source | target domain | replica's varstore |
| --- | --- | --- |
| `/var/lib/libvirt/qemu/nvram/web01_VARS.fd` | `web01` | unchanged — already what libvirt would pick |
| `/var/lib/libvirt/qemu/nvram/web01_VARS.fd` | `web01-dr` | `…/web01-dr_VARS.fd` |
| `/srv/uefi/web01.fd` | `web01-dr` | `/srv/uefi/web01-dr.fd` — your directory is kept |
| `/srv/uefi/shared-vars.fd` | `web01-dr` | unchanged — the name is not domain-derived, so there is nothing to correct |

Only the **filename** is rewritten, and only when it actually contains the
source's domain name. A custom directory is your choice and is preserved; a
varstore whose name has nothing to do with the domain is left exactly as you set
it, because guessing there would override a deliberate decision.

> **⚠ The last row is the one case where two domains can still share a
> varstore.** If a target host runs another domain pointing at the same
> non-domain-derived path, each boot overwrites the other's boot entries and
> Secure Boot keys. vmsync warns when it sees that shape. Give the replica its
> own `<nvram>` path if you need both on one host.

# Build

Several components are required to build from source, see the provided
Dockerfiles for example.
