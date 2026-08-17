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
- It deliberately removes the target domain's own `<vmsync:vmsync>`
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

**Stage 2 (verify + tamper)** always tests all three modes against the
same plain, no-bridge baseline sync, so the three are directly comparable
to each other regardless of whatever Stage 1 scenario ran last. One
caveat specific to `-verify=online`: unlike `compare`/`fast`, it never
suspends the source, and cross-references any mismatch it finds against
what the guest wrote to the *source* during the compare window, discarding
mismatches inside touched regions as inconclusive rather than failing on
them. Since this harness tampers the *target* independently of source
guest activity, that reconciliation should not swallow it — but if the
running guest happens to legitimately rewrite the exact same region during
that window, `-verify=online` could correctly and legitimately report no
mismatch this round. `bench.conf`'s `TAMPER_OFFSET` should point somewhere
the guest is unlikely to be actively rewriting, to keep this rare; the
harness logs a specific warning distinguishing this from an actual
detection bug when it happens.

**Stage 3 (`-reinit-after-failures`)** first runs its own baseline full
sync, then removes the target domain's own `<vmsync:vmsync>` metadata
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
things — the sync/verify itself still succeeds, the log shows the
expected "checkpoint creation blocked by an existing external snapshot"
tolerance path (added specifically for this scenario — see
`IsCheckpointBlockedBySnapshot` in `pkg/libvirtsync`), and the target
disk's resolved path did not drift while the snapshot was present; then
remove the snapshot for real (`blockcommit --active --pivot --wait`,
followed by `snapshot-delete --metadata` and removing the now-orphaned
overlay file); then sync+verify once more, checking that the checkpoint
chain actually resumes advancing rather than staying stuck in the
tolerant-but-not-advancing state. A `blockcommit` failure here is treated
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
