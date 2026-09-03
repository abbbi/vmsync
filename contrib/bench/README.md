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
- Stage 5 (opt-in, not run by default — see below) deliberately makes
  vmsync fail: 5a defines a throwaway domain on the target to force a UUID
  collision, and 5b runs one sync with `-test=failure-define` so the target
  redefine is rejected and the rollback can be checked. Neither touches
  host-level networking, and neither is more destructive to the replica
  than `-reinit` already is — but they are failure injection rather than
  measurement, so they only belong on the same disposable test host
  everything else here already requires.

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
./bench.sh                    # the defaults: Stage 1, then 2, 3, 4, then 9
./bench.sh --only 'compress-zstd-*'   # Stage 1 only, matching scenarios
./bench.sh --stages verify       # 2  just the verify+tamper tests
./bench.sh --stages snapshot     # 4  just the external-snapshot lifecycle test
./bench.sh --stages define       # 5  opt-in: deliberate DefineDomain failures
./bench.sh --stages failover     # 6  opt-in: the DR path — promotion, fencing, the way back
./bench.sh --stages fence-agent  # 7  opt-in: real agents; STOPS THE SOURCE VM, see below
./bench.sh --stages verify-long  # 8  opt-in: a 20-deep chain, then tamper and verify
./bench.sh --stages retention    # 9  just the restore-point tests
./bench.sh --stages restore      # 10 opt-in: putting a restore point back
./bench.sh --stages invert       # 11 opt-in: reversing a pair; STOPS THE SOURCE VM
```

Stages 2, 3, 4, 9, 10 and 11 each run their own baseline `-reinit` full
sync before anything else, so none of them actually depend on Stage 1 (or
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

Stage 1 passes **`-no-checksum`** on every cell. The pre-commit integrity
check is on by default whenever a matching `vmsync-bridge-helper` is on the
target — which, on any host this matrix can run on, is always, since the
compressed cells need that same binary. Left enabled it would add a
`qemu-nbd` start, a local read of every written byte on the target, a hash
and a stop to each of the stage's invocations: enough to inflate a `-reinit`
cell by roughly a disk-sized target read, and to mix the cost of an integrity
check into numbers whose whole purpose is comparing *transport* settings
against each other and against previous runs of this same matrix. The check
gets its own stage instead (see Stage 13).

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
- **`verify-fast` / `verify-full` / `verify-qemu-img`** each tamper, restore
  the original mtime, and assert the compare ran and reported a mismatch.
  That is the "contents diverged with nothing visible at the filesystem
  layer" case — a bad sector, a silent write error, a scrub miscompare.
  **Bit rot does not touch mtime**, so restoring it is not a way around the
  guard; it is the only way to construct the scenario `-verify` exists for.
  Each gets its own tamper at its own random offset, so repeated runs sample
  the disk broadly.
- **`verify-fast-bytes`** and **`verify-full-bytes`** repeat `fast` and `full`
  with `-no-checksum`, because `-verify=fast`/`full` now have **two**
  implementations behind them. When a matching helper is on the target the
  digest exchange runs (vmsync hashes the source, the helper hashes the
  target, only digests cross the wire); with `-no-checksum` the original byte
  comparators `CompareTCP`/`CompareTCPCollect` run instead. Since
  `bench_sync` passes `-compress`, the loop above always reaches the digest
  path — so without these two the byte comparators would be exercised by
  nothing at all. `qemu-img` is deliberately not repeated: it shells out to a
  separate implementation and is unaffected by checksumming either way, so a
  second run would spend a tamper cycle testing the same code twice.
- **`verify-cross`** tampers **once** and asks all three modes about that
  same corrupted replica, asserting they agree. This is the one that tests
  the comparator rather than the modes: `fast` and `full` read through
  vmsync's own code, which skips ranges both sides report as
  `NBD_STATE_ZERO`, while `qemu-img` reads every byte in an implementation
  vmsync did not write. `qemu-img` is therefore an *oracle* for that skip
  logic — but only when all three are asked about identical bytes, which the
  per-mode loop above deliberately does not do. `fast`/`full` reporting clean
  while `qemu-img` reports a mismatch means the skip is dropping real
  differences: a verify that passes over corruption, which is worse than no
  verify at all.
- **`verify-oracle`** is the other half: after the heal, an independent
  full-image `qemu-img` comparison of a freshly resynced replica, asserting
  it is clean. Catches a copy path producing a subtly wrong replica in a
  region vmsync's own verify happens to skip — where both halves of vmsync
  would agree the replica is fine and only an outside reader would notice.

The verify sub-tests key on `vmsync_verification_state`, which vmsync emits
only for a run that actually reached its compare. Its *presence* answers
"did this test test anything" and its *value* answers "what did it find" —
a distinction an exit code cannot make.

All three modes run against an
identically-configured baseline sync, so they are directly comparable
to each other regardless of whatever Stage 1 scenario ran last — see
[Transport](#transport-every-stage-but-the-matrix) for what that
configuration is. There is no
mode-specific caveat here any more. All three modes compare the target
against the same frozen source snapshot the copy read from, so a reported
difference cannot be explained away by guest activity and no mode discards
one. A tamper that goes undetected is a real miss, whichever mode was
running.

(This paragraph used to carry a long warning about `-verify=full`
reconciling mismatches against a dirty bitmap and legitimately reporting
nothing if the guest rewrote the tampered region. That reconciliation is
gone, along with the mode name.)

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

`TAMPER_LENGTH_MIN` defaults to 64KiB. The hard part of that floor is
arithmetic: vmsync reports mismatches at a 4096-byte granularity, so
anything smaller is indistinguishable from a 4KiB tamper and buys no
coverage.

The 64KiB figure itself is now convention rather than necessity. It came
from the dirty-bitmap reconciliation `-verify=full` used to do, which
discarded a reported range that overlapped anything the guest had written,
at qemu's default 64KiB bitmap granularity — so a smaller tamper could be
swallowed whole. No mode reconciles against a bitmap any more. The floor
stays because corruption at bitmap granularity is the more realistic shape
to test, not because a smaller tamper would be missed.

Set `TAMPER_MODE=fixed` to go back to a single `TAMPER_OFFSET`/`TAMPER_LENGTH`.

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
refuses to run, with a clear message, against a source that isn't.

It also refuses to run against a source disk that is not already **flat**,
recorded as the `ext-snapshot/precondition` row. Its cleanup step runs
`blockcommit --active --pivot` with neither `--base` nor `--top`, so `base`
defaults to the bottom of the chain: everything is committed into the base
image and the domain pivots there. Against the single overlay this stage
creates itself that is exactly the intended behaviour; against a chain
somebody else created it would flatten their snapshot structure with no
undo. So the stage aborts, without touching anything, when the source disk
already has a backing chain, when the source is currently running on a
leftover `vmsync-bench-extsnap-*` overlay (an earlier stage 4 that died
between `snapshot-create-as` and `blockcommit`), or when stale bench
snapshot metadata is still registered on the domain. Each case prints the
exact `virsh` command that resolves it.

Leftover overlay **files** are only reported, never removed: once those
checks have established the live disk is flat, nothing in the source
domain's chain references them. They are still named in the warning,
because the overlay filename is derived from the bench PID and a recycled
PID would make `snapshot-create-as` collide with one — and because the
stage's own cleanup deletes only the overlay it created, rather than
guessing at ownership of files on a shared host. Clear them by hand:

```bash
rm -f /path/to/images/*.vmsync-bench-extsnap-*
```

The sequence is: a baseline full sync (`-reinit`); create a real external
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
- **5b** runs one `-reinit` sync with `-test=failure-define`, vmsync's own
  fault-injection flag. That corrupts the document handed to
  `DomainDefineXML`, so libvirt genuinely refuses the redefine and the
  rollback runs against the state a real rejection leaves behind — rather
  than short-circuiting the call, which would only prove the rollback
  closure compiles. The stage then checks that the run failed, that vmsync
  logged restoring the previous definition, and that the target's
  definition (`virsh dumpxml --inactive`) is byte-identical to what it was
  before — **with the `<metadata>` element excluded from that comparison**.

  That last check is the one that matters: a replica defined from a
  half-restored document is one that boots wrong on the day it is promoted.

  **Why metadata is excluded.** vmsync records `replica_written_at` on the
  target after the copy and *before* `DefineDomain` runs, deliberately: that
  field exists so a run which wrote bytes and then failed still says so,
  which is what keeps the next run's out-of-band-write check from wedging.
  So on a `-test=failure-define` run the target's metadata legitimately
  differs from a snapshot taken outside vmsync, and the rollback correctly
  restores the state that includes it. Comparing the raw dumps therefore made
  5b a *guaranteed* false failure from the moment that field was added — the
  rollback was right and the assertion was comparing against a document that
  had already, deliberately, ceased to exist. What 5b asserts now is the
  domain **definition**: disks, devices, uuid, name — the things a promoted
  replica actually boots from. When a difference does survive the strip, both
  documents are written to `logs/define-rollback.xml-{before,after}` and the
  diff is printed, rather than the old advice to "diff the two dumps by hand"
  with nothing on disk to diff.

- **5c** is the other half of that exclusion, and exists so it cannot become
  a blind spot. "Not part of 5b's comparison" must not mean "not tested": the
  point of the rollback is that a failed redefine leaves the replica as
  usable as it was, and a replica whose vmsync metadata was wiped is **not**
  usable — the next sync cannot find its checkpoint, and a promotion has no
  record of what the domain is or what it was replicating.

  It runs inside 5b's window, between the failed redefine and 5b's healing
  resync (which would otherwise rewrite the metadata before anything could
  look at it), and asserts four things in the order their failures would
  matter:

  1. a vmsync metadata block still exists at all;
  2. every unconditionally-set field is still present
     (`last_checkpoint`, `last_sync_timestamp`, `failure_count`,
     `replica_source`, `checkpoint_at`);
  3. those fields are **unchanged** — the sharp one, since they are written
     *through* `DefineDomain`, so a committed change would mean the injected
     failure was not atomic;
  4. `replica_written_at` is **present**, and is allowed to differ. A new
     value is the correct outcome; its absence is the bug.

  The required-field list is a literal in `bench.sh`, not derived from
  vmsync's output — deriving both sides from one source would prove nothing,
  and a field dropped from vmsync without being dropped here is exactly what
  should fail. Conditional fields (`replication_role`,
  `source_stopped_at_sync`) are deliberately not required, since each is set
  or removed depending on the run.

  If the baseline sync left no metadata to begin with, 5c reports `SKIP`
  rather than passing vacuously against two empty snapshots.

  **Why fault injection rather than breaking something externally.** 5b
  used to watch the log for the undefine and then cut the target's SSH with
  an `iptables` rule. It could never work. The window between the undefine
  and the redefine holds no I/O at all — it is a few milliseconds of
  in-memory XML editing — while the harness needed a poll interval plus a
  fresh SSH handshake to get its rule in place. And landing it would have
  been worse, because the rollback restores over the *same* libvirt
  connection the disruption severs, so a perfect hit would have destroyed
  the recovery it was testing. It could also report the opposite of the
  truth: a rule landing slightly early killed the *undefine* instead, which
  returns before the rollback closure is even built, leaving a non-zero
  exit and an unchanged definition — indistinguishable from a successful
  rollback, and scored as PASS.

  `DEFINE_ROLLBACK_WAIT_SECONDS` is no longer used and can be removed from
  `bench.conf`.

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

Disk space is the other budget. `REPLACED_DISK_ACTION` defaults to `delete`
here, unlike vmsync itself, which defaults to `rename`: vmsync is right to
keep the old copy, because a `-reinit` target may be a former primary whose
disks still hold everything written since the last successful sync. That
does not apply to the disposable VM this harness requires, and the aside
copies are never reaped — so on a multi-GB disk, reinitting once per verify
sub-test and four more times in Stage 8 would fill `TARGET_DISK_PATH`
partway through a run and surface as a confusing mid-stage sync failure.
Set it back to `rename` if you would rather keep every replaced copy.

### Stage 9 (`retention`), default

Covers `-retention` end to end against a real target. It skips cleanly, rather
than failing, when the target filesystem cannot reflink — the same refusal
vmsync itself gives. That clean skip is why it can be a default stage: a target
on ext4 records SKIP and the run continues, so nobody has to know in advance
whether their storage supports the feature.

It runs last of the defaults because it `-reinit`s the target several times
over.

Most of it is counting directories, but two checks earn their place:

**It measures the space a restore point actually consumed.** A restore point
that was a full byte-for-byte copy would pass every other check here — the
directory would exist, the disk would be there, it would boot. It would simply
cost a whole replica per copy instead of sharing storage with it, and nothing
would say so. So the stage reads free space (`df`, never `du` — see above)
either side of a sync and fails if taking a restore point cost more than a
tenth of the replica.

**It checks that inspecting a restore point changes nothing.** `last_checkpoint`
is read before `-list-restore-points` and `-clone-restore-point` and again
after, and must be identical. If inspecting a copy moved it, the next
incremental sync would write its delta onto the wrong baseline — which looks
like a clean sync right up until a promotion. That is the phase 1 thesis stated
as an assertion.

The rest: the interval is honoured, pruning keeps the configured count and
drops the oldest first, the sidecar records a verify state, the clone is a
readable qcow2, and an operator `-reinit` sweeps the set.

Uses `-retention=N,0` — a zero interval means every sync — so the stage does
not have to sleep for hours to prove retention works.

### Stage 10 (`restore`), opt-in

Stage 9 proves restore points get taken. Stage 10 proves one can be put back,
with `-restore-restore-point`.

It takes two restore points with real guest writes between them, so the two
genuinely differ, and then compares files byte for byte (`cmp` on the target,
never `du`-style inference) to establish that a rollback actually happened
rather than that a command exited 0.

**The check that earns the stage is the last one.** A restore rolls the
replica's contents backwards while the source's checkpoint chain marches on,
and nothing in vmsync's incremental path looks at disk content — it compares a
checkpoint *name*. A restore that failed to invalidate the target's
replication metadata would leave the next scheduled sync applying the source's
newest delta onto rolled-back data, exiting 0, with green metrics, looking
exactly like a healthy run. So the stage runs a real sync afterwards and
asserts both that it **refused** and that the restored bytes are **still on
disk** when it did.

The rest: the assessment (no `-force-restore`) changes neither the replica nor
its metadata; a restore is refused on a domain marked `promoted`; the metadata
afterwards names the restored point's checkpoint and `checkpoint_at`, has
`source_stopped_at_sync` cleared and `replication_role=paused`; the three
fields `pkg/failover` reads as promotion evidence are all still satisfied
(clearing them would make the feature defeat its own purpose); the displaced
contents are kept and match what the replica held; and restoring consumes
neither restore point, so a first choice that turns out to be wrong can be
followed by a second.

Why it is opt-in: it is new, and it deliberately leaves the target paused
mid-stage. It clears that and heals with a `-reinit` on the way out — via
`clear_target_role`, which falls back to `virsh` if `vmsync -update-role`
itself is broken, because every later stage syncs into that target and all of
them are refused while it stays paused.

### Stage 11 (`invert`), opt-in

Nothing benched inversion before this, which is why it earns a stage: it is the
one operation that rewrites **both** ends' metadata, and each of its failure
modes leaves a pair that looks configured and cannot sync.

**The check it was written for is the disk-path one.** `-target-disk-path` names
where *this direction's* replicas go, so after an inversion the same value
points at where the new **source** keeps its disks, not the new target. Reuse it
on the reversed sync and the copy lands somewhere the new target does not keep
its disks; the domain is then redefined to match and its original disks are
orphaned — still on disk, still costing space, referenced by nothing. vmsync's
answer is to warn and name the value to use, so the stage asserts that warning:
that it fires when the two ends differ, that it names the right directory, and —
just as important — that it stays **silent** when both ends use the same
directory. A warning that always fires teaches operators to ignore it.

The rest: both roles flip; each end records the other as its new peer; the
promotion record is cleared; the old source's checkpoint chain is discarded (it
describes a chain running the other way, and a fail-back would chain onto it);
a sync in the **old** direction is refused afterwards, which is the interlock
that stops a cron job overwriting the new source with its own replica; and
re-running `-invert` on an already-inverted pair succeeds and says it had
nothing to do — that being the documented recovery from a half-applied
inversion, where one end's metadata write landed and the other's did not.

**What it deliberately does not do is run the reversed sync.** That writes into
the *source* domain's disks, which no other stage does, and it would prove
nothing the assertions above do not already establish. Everything here is
metadata and log text; the source's disks are never written.

Peer references are compared against the strings **vmsync itself wrote** during
the baseline, never rebuilt from `SOURCE_HOST`/`TARGET_HOST` — same reason Stage
6 gives: a reconstructed reference only asserts that two strings this harness
built match each other, and comparing the real ones also catches the regression
that once wrote `replica_source` as `127.0.0.1:<vm>`.

**This stage STOPS THE SOURCE VM**, like Stage 7 and for a reason that is part
of what it tests: `-invert` refuses while the old source is running, because the
inversion makes it a replication target and a running target is one scheduled
sync away from being overwritten under a live workload. vmsync will not stop a
production domain as a side effect of a metadata command, so the harness has to.
That refusal is asserted first — including that it tells the operator what to do
— and only then is the source shut down.

It shuts down **gracefully and never destroys**. If the guest does not stop
within `SOURCE_SHUTDOWN_WAIT_SECONDS` (default 90) the stage skips the rest
rather than pulling its power: Stage 6 will hard-stop a disposable replica, but
this is the one domain in the pair standing in for production, and destroying it
to run a test is the wrong trade. The EXIT trap starts it again whatever
happened — that restart runs before anything else in the cleanup, and is not
conditional on the inversion having got anywhere.

Why it is opt-in: besides stopping the source, an inversion leaves it marked as
a replication **target**, which is worse to leave behind than a promoted target
— every later stage and every scheduled run in the estate syncs *from* that
domain. The trap clears both ends' metadata and heals with a full resync, but
the risk is real enough to be asked for rather than assumed.

### Stage 13 (`checksum`), opt-in

The pre-commit integrity check hashes every chunk the copy reads, has
`vmsync-bridge-helper` hash the same ranges back off the target, and refuses
to commit an incremental sync's overlay if they disagree. It is **on by
default** whenever a matching helper is present.

That default is what makes a negative test essential rather than nice to
have: **a check that silently never fires is indistinguishable from a check
that works**, and every other stage in this harness would keep reporting
`PASS` either way.

Corrupting the data itself is not an option. The check reads the target back
between the copy and the commit, inside one vmsync process, so there is no
moment an outside tamper could land in. What *can* be substituted is the
helper — vmsync invokes it by path — so this stage installs a wrapper script
on the target that runs the real binary and then edits its reply. That
exercises the whole comparison (request, response, header check, digest
comparison, failure handling) without needing a fault injected into vmsync.

Four sub-tests:

- **`clean`** — an ordinary incremental must report that the check *ran and
  matched*. The load-bearing one: if the check is silently off (no helper, a
  version mismatch), the three below would pass for the wrong reason. It
  reports `SKIP` rather than `PASS` when there was no drift to hash, so the
  pass keeps meaning something.
- **`mismatch`** — a helper reporting one falsified digest must fail the run,
  the overlay must be **gone**, and the base image must be unchanged. One
  wrong block rather than wholesale corruption, because a single block is
  what a real bad write looks like and is the case a weak check would miss.
- **`stale`** — a helper too old to emit the format header must be reported
  as **version skew, not corruption**. The most consequential distinction in
  the feature: both failures stop the run, so an exit-code check would pass
  either way. What matters is the conclusion an operator draws — told "skew"
  they redeploy a binary; told "your replica does not match" they may reinit
  a perfectly good 50 GiB VM on evidence that was never about the data.
- **`off`** — `-no-checksum` must genuinely skip the check, so the escape
  hatch is real rather than a no-op.

Not in the default stage list: `mismatch` deliberately fails a sync, and all
four temporarily install helper shims under `/tmp` on the target host (removed
on the way out, whichever way the stage ends).

Preflight now also reports the helper's path and version, and warns when it is
missing or version-skewed — because in that state the check is skipped on
every run in the report, and only Stage 13a would otherwise notice.

### Stage 14 (`verify-failure`), opt-in

Stage 2 proves `-verify` **notices** a corrupted replica. This stage proves the
notice **outlives the process that made it**, which is a different property and
the one that actually protects an estate.

The gap it closes: a successful sync rebuilds the target's definition from the
*source's* XML, so every target-only field it does not explicitly write is
lost. A mismatch found at 02:00 was therefore erased by the ordinary
incremental at 03:00, and the promotion at 09:00 saw a replica with a clean
record. Nothing was untrue — the finding was in a log — but nothing that
*decides* anything could see it.

Four assertions, in the order the state moves:

- **`mismatch`** — the finding must be written to the target domain as
  `verify_state=failed` plus a `verify_failed_at` unix timestamp. The
  load-bearing sub-test: with nothing recorded, the two refusals below would
  "pass" by refusing nothing, and the stage would certify an interlock that
  does not exist.
- **`refuse-sync`** — an ordinary sync must be **refused** while the record
  stands, the refusal must not consume the record, and it must **not** count
  toward `-reinit-after-failures`. The exemption matters for a reason easy to
  miss: a non-zero failure count is itself reported by a promotion assessment,
  so counting this would stack a *may be stale* verdict on top of the *may be
  wrong* one that is the real finding.
- **`refuse-reinit`** — a plain `-reinit` must be refused **too**. The
  non-obvious half: a reinit does recopy every byte, so it looks like a repair,
  but it never verifies the result — permitting it would take the domain from
  *known bad* to *assumed good, unverified* while deleting the record that said
  otherwise.
- **`repair`** — `-verify-failure-reinit` must recopy, re-verify, and clear the
  record only on a **passing** verify. This one drives the whole ladder
  unaided: the run it starts is an *incremental*, which copies only what the
  source's dirty bitmap says changed, and the source never wrote to the
  tampered offset — so this run's own verify fails, and the recopy-and-
  re-verify fires for real.

Not in the default stage list: three of the four sub-tests deliberately fail a
sync. Its baseline is `-force-clean` rather than `-reinit`, because a re-run
after a previous attempt left a record behind would otherwise be refused by the
very interlock under test.

**Not covered**, deliberately: the branch where the repair's own verify *also*
fails, so vmsync gives up and leaves the replica faulty. The tamper this stage
uses is healed by the very recopy the repair performs, so the second verify
passes — that is the `repair` sub-test. Reaching the give-up branch would need
a `-test` fault able to force a corruption verdict, and no verify fault exists
today (only `failure-define`).

One consequence worth knowing: `heal_target`, which undoes every tamper in this
harness, now uses `-force-clean` rather than `-reinit`, because every tamper
sub-test leaves a `verify_state` record behind and a plain `-reinit` is refused.
That means the heal path no longer exercises the refusal — which is exactly why
Stage 14 asserts it rather than assuming it.

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
