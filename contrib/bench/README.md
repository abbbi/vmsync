# vmsync benchmark / integration harness

A Bash harness that drives a real `vmsync` binary against a real
source/target VM pair on a preprod server, across the full range of
transport settings, and reports sync times (and bytes transferred, where
available) for each. It also exercises `-reinit`, `-reinit-after-failures`,
all three `-verify` modes with deliberate target-side disk tampering to
confirm mismatch detection actually works, and the external-snapshot
lifecycle (syncing while a source-side external snapshot exists, then
again after it's removed) -- not just that the flags are accepted.

## Before you run this anywhere

**This is a genuinely destructive tool.** Read this whole section first.

- It repeatedly runs `-reinit` against the target replica: undefines the
  target domain, deletes its disk file(s), and does a fresh full sync.
  Every time Stage 1 runs a new transport scenario, and every time Stage 2
  heals after a tamper test, this happens again.
- It deliberately corrupts a few KiB of the target replica's disk file
  (via `qemu-io`) to test `-verify`'s mismatch detection, then heals it
  with a full `-reinit` resync immediately after. A script bug, a killed
  SSH session, or an interrupted run partway through Stage 2 could leave
  the target replica genuinely corrupted until the next successful sync.
- It deliberately removes the target domain's own vmsync
  metadata block (`virsh metadata ... --remove --config`) to induce
  genuine, repeatable checkpoint-chain-inconsistency failures for
  `-reinit-after-failures` testing. This is real target-side state churn,
  not a no-op -- the stage's own final successful trigger run overwrites
  it with fresh, consistent metadata, but an interrupted run partway
  through could leave the target's metadata missing until the next
  successful sync.
- Stage 4 creates a real, live external disk-only snapshot on the
  **source** domain (`virsh snapshot-create-as ... --disk-only --atomic`),
  then removes it with a live `blockcommit --active --pivot`. This is the
  one stage that touches the source rather than just the target — see its
  own section below before running it against anything you care about.
- **The target domain must be shut off for the entire run, and stays that
  way throughout.** The harness never starts it. This is also what makes
  tampering it safe — nothing else has the file open at the time.
- Stage 5b (opt-in, not run by default — see below) temporarily inserts an
  `iptables` rule on the **target host itself** to force a real
  `DomainDefineXML` failure. It's self-removing and this script also tries
  an explicit cleanup afterward as a backstop, but it's the one stage that
  touches host-level networking rather than just the VM/replica — only
  enable it against the same disposable test host everything else here
  already requires.

**Point this at a disposable, dedicated test VM.** Never at a real,
in-use replication pair. `bench.sh` refuses to run for real (as opposed to
`--dry-run`) until `bench.conf`'s `I_UNDERSTAND_THIS_IS_DESTRUCTIVE` is set
to `yes` — that gate exists specifically so this can't be run by accident.

## Setup

```bash
cd contrib/bench
chmod +x bench.sh
cp bench.conf.example bench.conf
$EDITOR bench.conf
```

Fill in `bench.conf`: the `vmsync` binary path, source/target libvirt URIs
and hostnames, the test VM's domain name, SSH credentials, and the disk
device/offset to use for tamper testing. Every field is commented in
`bench.conf.example`.

Prerequisites on the machine running `bench.sh` (not necessarily the
source/target hosts themselves): `virsh`, `xmllint` (from `libxml2-utils`
or `libxml2`), `ssh`, `awk`. On the **target** host specifically:
`qemu-io` (part of `qemu-utils`, already required alongside `qemu-nbd` for
vmsync itself to work at all).

## Running it

```bash
./bench.sh --dry-run          # print every vmsync command line, touch nothing
./bench.sh                    # the full run: Stage 1, then 2, 3, then 4
./bench.sh --only 'compress-zstd-*'   # Stage 1 only, matching scenarios
./bench.sh --stages verify    # just the verify+tamper tests
./bench.sh --stages snapshot  # just the external-snapshot lifecycle test
./bench.sh --stages matrix,verify,reinit,snapshot,define  # + Stage 5, opt-in
./bench.sh --stages failover  # just the DR path: promotion, fencing, the way back
./bench.sh --stages fence-agent  # + real agents; STOPS THE SOURCE VM, see below
```

Stage 2, Stage 3, and Stage 4 each run their own baseline `-reinit` full
sync before anything else, so none of them actually depend on Stage 1(or
any prior sync) having run first — each is safe to run standalone via
`--stages`. This matters for more than just the "full vs incremental"
distinction in their own pass/fail checks: Stage 3 specifically needs a
target domain that already exists, since `-reinit-after-failures`'s
counter lives in the target's own domain metadata and vmsync treats
recording a failure against a target that doesn't exist yet as a no-op
(nothing to record against) — without its own baseline, every induced
failure in `--stages=reinit` run in isolation would silently fail to
persist, and the counter would never reach its threshold. Stage 5 is the
same way (its own baseline first) but is never included by the default
`--stages` value — see its own section below and pass it explicitly.

Results land in `results/<run-id>/`: `results.csv` (machine-readable),
`report.md` (the human-readable summary, also printed at the end),
`logs/*.log` (full stdout+stderr per run), `prom/*.prom` (the raw
`-prometheus-textfile` output per run).

### Did it work?

Each stage prints its own verdict as a banner the moment it finishes, and
the run ends with an overall one — after the report, which is long:

```
[…] ============================================================
[…]   BENCH RESULT: FAIL
[…] ============================================================
    matrix       PASS     1170 vmsync runs, all exited 0
    failover     FAIL     4 of 21 checks failed
    fence-agent  SKIPPED  binaries unset
[…] ============================================================
```

The same table heads `report.md`, so a skim reaches the outcome rather than
inferring it from the tables below.

**The exit status means something**, so this can be driven by something that
acts on the answer:

| exit | meaning |
| --- | --- |
| `0` | every stage that ran passed |
| `1` | a stage failed — or, in a real run, *nothing was verified*: stages were asked for and every one of them skipped, which is usually a config problem |
| `1` | a `die`: a fatal precondition, before any stage could run |

`--dry-run` verifies nothing by design and exits `0`.

A stage's verdict comes from `results.csv` rather than from each stage
remembering to report one: every stage but the matrix records `PASS`/`FAIL`/
`SKIP` per assertion in the notes column, and the matrix — which has no
assertions, only timings — fails if any `vmsync` exited non-zero.

## Transport: every stage but the matrix

Every stage **except Stage 1** adds `-compress` to its syncs, via
`BENCH_SYNC_ARGS` in `bench.conf`.

Stage 1 is the one stage measuring transport, so it chooses its own from
`scenarios.conf` and is never given these. Every other stage copies a disk
only so there is something real to tamper with, reinit, snapshot, promote or
fence — the transport is incidental, and the fastest way across the wire is
simply the least waiting. On a link that is the bottleneck (a saturated 1GbE
reads as roughly 110 MB/s in `results.csv`) that is most of their runtime,
and Stage 6 alone does three full copies.

It does not change what those stages test. vmsync's port allocator handles
*"whichever combination of `-compress`/`-netbuffer`/`-verify` is active"* by
design, reserving 4N target ports when both are on, so the verify modes
compose with the bridge rather than working around it. Stage 3 already
passed `-compress` unconditionally before this was a setting.

`-compress` needs `vmsync-bridge-helper` on the **target** host. Without it
every affected stage fails at its baseline — and says so, naming this
setting. Set `BENCH_SYNC_ARGS=""` to turn it off, or to any vmsync transport
flags (`"-compress=zstd -compress-level 3"`, `"-compress -netbuffer 128k,1G"`).

## What this does and does not control

**Stage 1 (transport matrix)** times a full sync (`-reinit`) followed
immediately by one incremental sync, for the full cross-product of every
value in `scenarios.conf`'s four `[compress]`/`[netbuffer]`/`[use_ssh]`/
`[iodepth]` sections, so you get both numbers under directly comparable
conditions for every combination. `scenarios.conf` is not a flat list of
rows — with the shipped defaults (s2 x3 levels + zstd at every odd level
1-19 + "off", 7 netbuffer settings, 2 `-use-ssh` values, 3 `-io-depth`
values) that cross product is 588 cells, minus the handful the harness
itself skips as meaningless (`-use-ssh` alone with no bridge active is a
documented no-op) — **585 combinations, 1170 full `vmsync` invocations**.
`bench.sh` prints the exact count (and `N/total` progress) before Stage 1
starts. Trim any of the four sections down if that's more than you
actually want to wait for — each row of the product is a full, real disk
copy. The exact format is documented at the top of `scenarios.conf`.

The incremental sync's timing reflects **whatever real drift accumulated
on the source VM** since the preceding full sync — the harness
deliberately does not fabricate synthetic writes on the source to force a
reproducible incremental size. This means incremental numbers will vary
run to run depending on real guest activity, which is realistic but not
perfectly controlled; if you need a fixed, reproducible incremental
workload, write a known amount of data from inside the guest yourself
between runs.

**Stage 2 (verify + tamper)** runs *four* sub-tests, not three, because two
different protections are involved and conflating them is how this stage
spent a long time reporting `PASS` without ever exercising `-verify` at all.

`vmsync` refuses an incremental sync when the target file's mtime is newer
than the last recorded sync (`"Target file on system is newer"` in
`cmd/vmsync/main.go`), and that check fires *early* — before the checkpoint
is created, long before any compare. `qemu-io` tampering bumps the mtime, so
the tamper sync used to die at that guard, and the harness scored its
non-zero exit as "`-verify` detected the mismatch". It had not; it had never
run.

The two checks cover disjoint threats, so both are now tested by name:

- **`verify-guard`** tampers and leaves the mtime alone, asserting the guard
  refuses the sync *and* that no verification metric was emitted. That is
  the "somebody wrote to the replica through the filesystem" case.
- **`verify-compare` / `verify-fast` / `verify-online`** tamper, restore the
  original mtime, and assert the compare ran and reported a mismatch. That
  is the "contents diverged with nothing visible at the filesystem layer"
  case — a bad sector, a silent write error, a scrub miscompare. **Bit rot
  does not touch mtime**, so restoring it is not a way around the guard; it
  is the only way to construct the scenario `-verify` exists for.

The verify sub-tests key on `vmsync_verification_state`, which vmsync emits
only for a run that actually reached its compare. Its *presence* answers
"did this test test anything" and its *value* answers "what did it find" —
a distinction an exit code cannot make.

All three modes run against an
identically-configured baseline sync, so they are directly comparable
to each other regardless of whatever Stage 1 scenario ran last — see
[Transport](#transport-every-stage-but-the-matrix) for what that
configuration is. One
caveat specific to `-verify=online`: unlike `compare`/`fast`, it never
suspends the source, and cross-references any mismatch it finds against
what the guest wrote to the *source* during the compare window, discarding
mismatches inside touched regions as inconclusive rather than failing on
them. Since this harness tampers the *target* independently of source
guest activity, that reconciliation should not swallow it — but if the
running guest happens to legitimately rewrite the exact same region during
that window, `-verify=online` could correctly and legitimately report no
mismatch this round. `bench.conf`'s tamper band should sit somewhere
the guest is unlikely to be actively rewriting, to keep this rare; the
harness logs a specific warning distinguishing this from an actual
detection bug when it happens.

### Where the corruption goes

By default (`TAMPER_MODE=random`) every tamper picks its own offset and
length, drawn from `TAMPER_BAND_START`/`TAMPER_BAND_END` and
`TAMPER_LENGTH_MIN`/`TAMPER_LENGTH_MAX`. A single hand-picked offset only
ever proves that *that* offset is detectable; varying it finds boundary
cases in the mismatch scanner and offsets whose detection depends on where
they fall.

That is only useful because it is **reproducible**. The draw is seeded from
`TAMPER_SEED`, which defaults to the run id, is logged at startup, and is
repeated in every failure message. Re-run with `TAMPER_SEED=<that value>`
and the whole sequence of corruptions repeats byte for byte. The seed is
hashed with `md5sum` rather than using `$RANDOM` or `awk`'s `srand()`,
neither of which is reproducible across hosts or implementations — and a
failure you cannot replay is one you cannot investigate.

`TAMPER_LENGTH_MIN` defaults to 64KiB, and that floor is arithmetic rather
than caution. vmsync reports mismatches at a 4096-byte granularity, so
anything smaller than that is indistinguishable from a 4KiB tamper. Above
that, `-verify=online` discards a whole reported range when *any* byte of it
overlaps a region the guest wrote during the compare window, and those
regions come from a dirty bitmap at qemu's default 64KiB granularity — so a
tamper smaller than one granule can be swallowed whole by an unrelated guest
write and read as "verify missed it".

Set `TAMPER_MODE=fixed` to go back to a single `TAMPER_OFFSET`/`TAMPER_LENGTH`.

### Stage 8 (`verify-long`), opt-in

Every other stage verifies a replica built moments ago by a single
`-reinit`. Stage 8 builds one the way a real deployment does — twenty
incremental syncs, each carrying real guest writes — and corrupts it **part
way through**, so the copies that follow run over the damage.

Two things about the shape are deliberate.

**Each mode gets its own full chain.** Healing after a tamper has to be a
full `-reinit` — an incremental sync re-copies only what the *source's*
dirty bitmap says changed, and the source never wrote to the corrupted
region — and that destroys the chain. So a single chain followed by several
tamper/verify attempts would test a deep chain exactly once and a one-deep
chain every time after, while reporting all of them as though they had
tested the same thing. Three modes therefore means three chains.

**The corruption lands after a random copy, not after the last one.** Bit
rot does not wait for a sync window to close. Injecting it mid-chain asks a
question the end-of-chain version cannot: does the damage survive the
incremental syncs that follow it, and is it still detectable afterwards?

That opens one legitimate outcome that is neither pass nor fail. If the
guest happened to write to the same region the corruption sat in, a later
incremental copy overwrote it — correctly, by design — and there is nothing
left for `-verify` to find. Scoring that as "verify missed it" would be a
false accusation against the code under test, and it is not a rare corner:
`GUEST_DIRTY` rewrites the same file every round, so its blocks are exactly
the ones most likely to be re-copied. The harness therefore re-reads the
corrupted region immediately before verifying, and records **SKIP —
corruption healed by a later copy** when it is gone.

It needs the guest to write between copies, or the chain is twenty no-ops
against an empty dirty bitmap and the stage proves nothing. There is no
`virsh` verb for running a command inside a guest, so this goes through the
QEMU guest agent's `guest-exec` RPC. **Two separate things must hold**: the
agent must be responding — vmsync's own `FSFreeze` already needs that — *and*
`guest-exec` must not be blocked. RHEL-family packages ship
`/etc/sysconfig/qemu-ga` with `guest-exec` and `guest-file-*` in `BLOCK_RPCS`
by default, so an agent that happily freezes filesystems will still refuse to
run `dd`. The harness probes `guest-info` for this and **skips the stage**
with a message naming the file, rather than spending an hour proving that
vmsync can copy nothing twenty times.

Two details that would otherwise make the chain meaningless: the write uses
`conv=fsync`, because the dirty bitmap tracks writes that reach the virtual
block device and a `dd` whose data is still in the guest's page cache leaves
it empty; and the harness polls `guest-exec-status` until the write has
exited, because `guest-exec` returns a pid immediately and the next
checkpoint would otherwise be taken mid-write. The stage asserts that the
chain actually carried data, and fails if none of the copies transferred a
byte.

Budget for it: roughly `VERIFY_LONG_MODES × (VERIFY_LONG_COPIES + 1)` syncs
plus one final heal — 64 with the defaults, four of them full resyncs. Trim
`VERIFY_LONG_MODES` to a single mode if that is too much for one sitting.
Consider setting `-replaced-disk-action=delete` for the run — the default
renames each discarded target disk aside and those copies are never reaped.

**Stage 3 (`-reinit-after-failures`)** first runs its own baseline full
sync, then removes the target domain's own vmsync metadata
block (`virsh metadata ... --remove --config`) to induce `N` genuine,
repeatable incremental-sync failures -- the same
`unverifiableCheckpointMetadataError` a real target would hit if it were
manually redefined, restored from an old XML, or otherwise lost vmsync's
own bookkeeping (see `pkg/libvirtsync`/`cmd/vmsync/main.go`). The source
domain is never touched and stays real throughout -- this is a deliberate
choice over e.g. pointing at a nonexistent source domain, since a missing
source says nothing about whether the incremental-sync trust mechanism
itself is working. After `N` induced failures, a final run with
`-reinit-after-failures=N` is expected to force a full resync instead of
the incremental one it would otherwise attempt, which also overwrites the
target's metadata with a fresh, consistent value.

**Stage 4 (external snapshot lifecycle)** confirms sync+verify keep
working correctly across the one scenario vmsync itself has special-cased
handling for: an external, disk-only snapshot sitting on top of the
source disk. Unlike every other stage, this one requires the **source**
domain to be running (removing an external snapshot via a live
`blockcommit --active --pivot` is itself a running-domain operation, and
it's also vmsync's own realistic use case of replicating a live VM) — it
refuses to run, with a clear message, against a source that isn't. The
sequence is: a baseline full sync (`-reinit`); create a real external
disk-only snapshot on the source (`virsh snapshot-create-as
--disk-only --atomic`); sync+verify while it exists, checking three
things — the sync/verify itself still succeeds, vmsync actually saw the
snapshot (`vmsync_external_snapshot_count` is non-zero, which is what
proves the snapshot took effect on the disk being synced), and the target
disk's resolved path did not drift while the snapshot was present; then
remove the snapshot for real (`blockcommit --active --pivot --wait`,
followed by `snapshot-delete --metadata` and removing the now-orphaned
overlay file); then sync+verify once more, checking that the checkpoint
chain actually resumes advancing rather than staying stuck in the
tolerant-but-not-advancing state.

Whether libvirt refuses to create a checkpoint while an external snapshot
exists is **version-dependent** — newer libvirt/qemu pairs allow it. So
the during-snapshot run reports which happened rather than requiring one:
if the checkpoint was blocked, it confirms vmsync's tolerance path handled
it (see `IsCheckpointBlockedBySnapshot` in `pkg/libvirtsync`); if libvirt
permitted it, the chain simply advanced normally and the tolerance path
was not needed. Both are a PASS. An earlier version of this stage demanded
the tolerance log line and reported a healthy run on a permissive libvirt
as a failure. A `blockcommit` failure here is treated
as fatal rather than merely healed and retried (unlike Stage 2's tamper
tests) — it can leave the source domain's own disk chain inconsistent,
and the harness stops immediately with instructions to inspect it by hand
via `virsh blockjob` before doing anything else.

**Stage 5 (`DefineDomain` redefine/rollback, opt-in — pass `--stages
...,define` explicitly)** exercises `libvirtsync.DefineDomain`, the sole
place vmsync ever undefines or redefines the target domain (a fix earlier
in this project's own life removed `-reinit`'s own early undefine
specifically so this function's capture-XML/undefine/redefine/
rollback-on-failure sequence would be the only thing relied on) — and,
before this stage existed, nothing exercised it end to end. Two sub-tests:

- **5a** defines a throwaway domain on the target that deliberately reuses
  the source domain's UUID, then runs a real sync — this reliably
  reproduces libvirt's own "already defined with uuid" error and forces
  vmsync's documented stripped-UUID retry to actually run, checked by
  confirming the target's UUID changed (proof the retry executed) and the
  sync still succeeded overall. The throwaway domain is undefined again
  afterward either way.
- **5b** is a best-effort timing race, not a hard pass/fail: it starts a
  real `-reinit` sync in the background, watches its log for the
  "Undefining domain on target system" line `DefineDomain` logs right
  before the real work starts, and the instant it appears, briefly
  disrupts the target's own SSH reachability (an `iptables REJECT` rule,
  self-removing) to try to make the actual redefine call fail. If the
  disruption lands, it checks that the sync failed **and** that the
  target's domain definition (`virsh dumpxml`) is byte-identical to what
  it was before the run — i.e. that rollback genuinely restored it rather
  than leaving the target undefined or partially redefined. Because this
  races a live, variable-duration copy with no way to synchronize on the
  exact right instant from outside the vmsync process, missing the window
  entirely is common and reported as a SKIP, never a FAIL — only a landed
  disruption proves anything either way. `DEFINE_ROLLBACK_WAIT_SECONDS` in
  `bench.conf` caps how long it waits for the marker before giving up on
  that attempt.

**Stage 6 (failover)** covers the DR path — promotion, the fence a
promotion arms, and the role change that undoes both — which had no
real-life coverage at all before it existed. Opt in with `--stages
...,failover`.

It is deliberately **power-neutral and direction-neutral**: it never stops
a domain and never reverses a pair. The sequence is baseline `-reinit` →
promote → inspect → arm a fence → inspect → `-update-role=target` → sync
again, ending exactly where it started. What it asserts:

- a promotion records `role=promoted`, a timestamp, and a `promoted_from`
  matching the `replica_source` the sync itself wrote — which also catches
  a real regression directly, since that field was once written as
  `127.0.0.1:<vm>`, a name that identifies every host and therefore none;
- **a promotion with no `-fence-source` arms nothing at all.** This is the
  single most important safety property in the fencing design: a DR drill
  is a promotion too, and one that authorised stopping production would be
  worse than the split brain it was rehearsing for;
- a promoted target **refuses to be synced into** — the backstop under
  everything else;
- `-read-fence` reports the peer's role and, once armed, the fence's id and
  who it names, read from the other host exactly as a displaced source
  would read it;
- an **unreachable** peer reports `reachable=false` and still exits 0.
  Silence must never read as "no fence is armed": a partition is precisely
  when a promotion is most likely to have happened and least likely to be
  visible;
- a fence can be armed on an **already-promoted** domain without rewriting
  the original promotion record — the recovery path for "promoted, then
  noticed the old source is still serving";
- `-shutdown-domain` **refuses a remote libvirt URI**, which is what keeps
  a failover working when the other site is unreachable;
- `-update-role=target` takes the promotion record *and* the fence with
  it, and replication actually resumes afterwards.

It also checks **who owns the target's disks**, which is the check that
would have caught the bug the ownership handling was written for: vmsync
creates those files by running `qemu-img` over SSH, so they belong to that
SSH user — root — while qemu runs as `qemu` or `libvirt-qemu` and cannot
open a root-owned disk. That surfaces during a failover, on the copy meant
to take over.

The stage works out what the target host *should* use by looking for a
`qemu` or `libvirt-qemu` account itself, rather than reading it back out of
vmsync's log — a test that asks the thing under test what the right answer
is passes just as happily when both are wrong together. Then:

- a **fresh** sync leaves the disk owned by that user (and, stated
  separately because it is the bug's specific signature, *not* by root);
- **`-reinit` preserves** the ownership it replaces. This is the sharper
  half: reinit renames the correctly-owned disk aside and creates a fresh
  root-owned one, silently turning a bootable replica into one qemu cannot
  open;
- an explicit **`-target-disk-owner` overrides** what was preserved.

Each property only happens when a disk file is created from scratch, so
this costs **two extra full copies** on top of the stage's own baseline --
see [Transport](#transport-every-stage-but-the-matrix) for why that is
cheaper than it sounds.
The sentinel it uses is the disk's *group*, set to `root` while leaving the
owning user alone — deliberately harmless, so the disk stays openable
throughout and an interrupted run never leaves an unbootable replica. If
the target host has neither account, the ownership checks skip: there is no
way to say what the right answer would be.

It needs **`TARGET_VMSYNC_BIN`** in `bench.conf`: `-promote` and
`-update-role` refuse a remote URI by design, so they must run on the
target host over SSH rather than being driven from here like every other
stage. Without that setting the stage skips with a message rather than
failing.

Why it is opt-in rather than default: it promotes a real replica, and a
target left `promoted` refuses **every** later sync in the estate — not just
this harness's — until somebody clears it. That is a worse thing to leave
behind than any other stage does, even though nothing here is more
destructive to the target than `-reinit` already is.

Three things keep that from happening. The stage puts the target back itself;
an `EXIT` trap does the same on a `die` or a Ctrl+C; and if the way back does
not take, it says so loudly and prints the exact command:

```bash
vmsync -update-role target -target-uri qemu:///system -target-domain <vm>   # on the target host
```

Both this stage and Stage 7 also **wipe vmsync's metadata off the domains
they are about to use** before starting, so a run left half-finished by a
`kill -9` does not block the next one. Stage 7 does the source too, since it
is the one that fences.

That reset goes through `virsh`, not `vmsync -update-role`, and the
distinction matters: a harness that resets state through the code it is
testing cannot recover when that code is broken. When writing metadata
failed outright, `-update-role` failed with it, every run left the target
`promoted`, and the *next* run then failed at its baseline for reasons that
said nothing about the real fault. `virsh` talks to libvirt directly and has
no such dependency. If the reset does not take, the stage still skips with
the manual command rather than failing three minutes into a copy.

Wiping the whole block rather than just the role is deliberate: these stages
`-reinit` the target immediately so nothing in it is worth keeping, it sweeps
up anything an interrupted run or a hand-run `virsh metadata --set` left
behind, and it makes the stage's own *"the sync recorded a `replica_source`"*
assertion prove the sync wrote it rather than that it was already there.

**Stage 7 (fence-agent)** is the other half: Stage 6 proves the fence
*token* is written and readable, this proves it is **acted on**. A real
`vmsync-agent` runs on the source host, reads the token from the promoted
peer's own libvirt, and shuts its copy down. Opt in with `--stages
...,fence-agent`.

> **This stage stops the source VM.** Every other stage deliberately leaves
> the source's power state alone. This one cannot: a fence only ever acts on
> a *running* domain, so a fence that never fires would prove nothing. It
> restores the source afterwards — power state and both roles — and an
> `EXIT` trap does the same on a crash or a Ctrl+C, but this is a different
> class of intrusion from anything else here.

The agents run in `--standalone` mode: no control plane, no enrolment, no
credential. Their schedule entry is deliberately **disabled**, so no syncs
run and the fence still fires — which is the design property being
demonstrated, since a displaced source is very often one whose replication
was already switched off.

What it asserts:

- the fence actually **shuts the displaced source down**, within
  `FENCE_WAIT_SECONDS` (the agent sweeps once immediately at startup, so
  this mostly covers the guest's own shutdown rather than the 60s tick);
- the fenced source is left **`paused`**, not merely stopped — without that
  the next sync would start replicating it again;
- the agent recorded the fence in its **durable ledger** (`fences.json`)
  with state `done`, which is what makes a fence single-use and would stop
  a second attempt on the next sweep;
- with `TARGET_AGENT_BIN` set, an agent on the target host **does not fence
  the promoted copy**. A fence sweep must skip anything whose role is not
  `source`, and a bug there would stop the copy that just took over.

Needs `SOURCE_AGENT_BIN` (the agent that gets fenced) and
`TARGET_VMSYNC_BIN` (to promote). `TARGET_AGENT_BIN`, `SOURCE_VMSYNC_BIN`,
`AGENT_WORK_DIR` and `FENCE_WAIT_SECONDS` are optional — see
`bench.conf.example`. Without the two required ones the stage skips rather
than failing. When a fence does not fire, the agent's own log is at
`$AGENT_WORK_DIR/agent.log` on the source host.

If `bench.sh` runs directly on the source hypervisor itself (`SOURCE_URI`
pointing at a local `qemu:///system`, say), set `SOURCE_LOCAL=yes` in
`bench.conf` — this makes the one direct shell-out Stage 4 does against
`SOURCE_HOST` (removing the leftover overlay file after `blockcommit`)
run locally instead of over SSH, so no SSH access to the source host is
needed at all in that setup.

## Files

- `bench.sh` — the driver.
- `bench.conf.example` — config template; copy to `bench.conf` (gitignored).
- `scenarios.conf` — the Stage 1 transport matrix; edit freely.
- `lib/common.sh` — shared shell helpers (logging, ssh/virsh wrappers,
  Prometheus textfile parsing).

## A note on how this was built

This harness was written by an AI assistant from a careful reading of
vmsync's own CLI flag parsing and Prometheus metrics code, but it has
**not been run against a real vmsync/libvirt environment** (no such
environment was available while writing it). Review the generated `vmsync`
command lines with `--dry-run` before trusting it on real infrastructure,
and expect to need small fixes for whatever is specific to your actual
environment (exact `qemu-io` behavior/version, SSH configuration quirks,
etc.).
