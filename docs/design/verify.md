# -verify: what it is, and what is left to do

Working notes for the verification path. Written during the 2026-09-01/02
audit that followed a production report of `-verify=online` failing
systematically on multi-disk VMs.

The F-numbers are the audit's own, kept stable so they can be referred to in
commits and conversation. Numbering is not priority order.

**Status: the F-list is closed.** F1–F13 and PORT-2 are all done, and the
title's "what is left to do" now means the two measurements listed under
"Open" rather than any outstanding work.

The document is kept because most entries record *why* a fix took the shape it
did, and because in several places this document was itself wrong — which is
the part worth having when somebody proposes the same thing again:

- **PORT-2** prescribed a reservation plus a host-wide mutual-exclusion point.
  Replaced by binding the port and retrying on failure, which needs no lock at
  all. The prescription was nearly built.
- **F9** prescribed a remote `ss` probe. Replaced by keeping the dial and
  pointing it at the address actually in use, because only a dial proves
  reachability.
- **F13** was expected to be the largest remaining item on two counts, and
  both were wrong: the helper needed no libnbd, and two-level hashing was never
  necessary.
- A **`qemu-img` confirming pass** after a failed verify was proposed here and
  declined, because it suspends the source.

## Where it stands

`-verify` compares each replica disk against the **same frozen source
snapshot the copy read from** — the primary backup job's export, still open.
Both sides are therefore the same point in time and must be byte-identical.
There is no drift to excuse and no dirty-bitmap reconciliation anywhere in
the path.

Three modes, differing in cost and independence, not in what they assert:

| mode | comparator | suspends source | use |
|---|---|---|---|
| `fast` | digests both sides in parallel, or `nbdsync.CompareTCP` under `-no-checksum` | no | scheduled runs |
| `full` | same, reporting every differing block rather than a summary | no | when you need to know *how* broken |
| `qemu-img` | `qemu-img compare`, an independent implementation | **yes** | tie-breaker |

`fast` and `full` each have **two** comparators behind them. With a matching
`vmsync-bridge-helper` on the target (the default) vmsync hashes the source
while the helper hashes the same ranges locally, and only digests cross the
wire. With `-no-checksum` the original byte comparators
(`CompareTCP`/`CompareTCPCollect`) run instead. Both are exercised by bench —
see `verify-fast`/`verify-full` and `verify-fast-bytes`/`verify-full-bytes`.

`qemu-img` is deliberately excluded from the digest path: it is the
independent oracle, and re-expressing it through vmsync's own digest code
would make it agree with vmsync by construction.

### There is a second integrity check now, on the sync path

Separate from `-verify` and on by default: every chunk the copy reads is
hashed as it passes, the helper hashes the same ranges back off the target
before the commit, and an incremental sync's overlay is **removed instead of
committed** if they disagree. `-no-checksum` disables it. See the F12 entry under Done, and
`pkg/blockdigest`.

The two answer different questions and deserve different cadences: *did this
run land correctly* versus *is this replica still intact*.

### Done

- **F1** — `-verify=online` (now `full`) compared source@T2 against
  target@T0 and tried to excuse the difference with a bitmap that measured
  neither interval. Near-100% false positives on a busy guest. Replaced by
  comparing against the primary export; the second backup job, the ephemeral
  checkpoint, the barrier, the two-phase path and `overlapsAnyExtent` are all
  gone.
- **F2** — a run that copied and then failed left the disks freshly written
  and `last_sync_timestamp` untouched, so the next run's out-of-band check
  refused, forever. `replica_written_at` records the write independently of
  run success, per disk, on the target's own clock.
- **F3** — `cfg.Verify != ""` exempted *every* failure on a `-verify` run
  from `failure_count` and `-reinit-after-failures`, so an operator running
  verify on scheduled syncs had auto-reinit silently doing nothing. Now only
  a verification that RAN and found a difference is exempt
  (`isVerifyMismatch`); a compare that could not be performed counts like any
  other broken sync.
- **F10** — bridge pidfiles were keyed by port alone, host-wide, so two runs
  colliding on a port could have the loser's readiness check match the
  winner's helper and relay into another VM's disk. Now keyed by identity.
  Target NBD exports are additionally **named** (`<domain>-<dev>`) and every
  connection asks for the name, so mixing two VMs is refused by the NBD
  handshake rather than prevented by port hygiene.
- The suspend that `compare`/`fast` both used to do was **vestigial**: git
  history shows `-verify` originally read the source as a local FILE
  (`disk.CompareImages(d.RootSource, ...)`), where a running guest genuinely
  would have corrupted the comparison. A later change repointed the source at
  the frozen NBD export and left the suspend behind. Only `qemu-img` still
  suspends, and only to keep the source snapshot's scratch space empty across
  a full-image read.
- **F11 — allocation-aware compare.** `compareTCP` chunked the entire
  *virtual* size with no block-status query, so a sparse 50 GiB image with
  12 GiB used was read in full from **both** sides. It now skips ranges
  reported as reading zeros on both, keyed on `NBD_STATE_ZERO` rather than
  `HOLE` (a hole with a backing file underneath does not read as zeros).
  Measured on a real pair: a nearly-empty 10 GiB disk went from 10 GiB per
  side to 4 MB, compared in 14 ms; a nearly-full one gained ~11%. The win
  scales with sparseness, not size.
- **F12 — the pre-commit integrity check** (see "a second integrity check"
  above). Built on the digest exchange rather than a byte comparison,
  because `CopyExtentsTCP` **streams**: a byte compare before the commit
  would have to re-read the source *as well as* the target, 2× the delta,
  while hashing the bytes already in the copy's buffers costs no extra I/O
  at all. Cost is proportional to the delta, so it is near-free on a small
  incremental.
- **F13 — digests instead of transferring the replica**, and it turned out
  much cheaper than this document originally estimated. See below.
- **F4, F7, F8 and PORT-2** — all four closed by one change, and three of them
  without being worked on directly: ports are no longer reserved as a block
  but bound one at a time, retrying on failure.
- **F9** — the readiness probe now dials the address the copy or compare will
  really use, in every mode, and a failure is exempt from `failure_count`.

See "Closed after this list was written" below for both, including why the
reservation-and-lock design and the `ss`-probe design were each the wrong
answer.

#### What F13 actually cost, versus what was predicted

The original F13 note claimed two things that both proved wrong, and they
are recorded here because they were what made it look like the biggest,
last-to-do item.

**"Adding libnbd to the helper and therefore a new dependency on every
target host."** The helper needs only `NBD_CMD_READ` (plus block status)
against a *localhost* qemu-nbd. `pkg/nbdclient` is a few hundred lines of
pure Go doing exactly that — fixed newstyle handshake, `NBD_OPT_GO`, simple
replies, nothing optional negotiated. So the helper stays a single static
binary and the change is a redeploy of the same file, not a new
compiled dependency. cgo was the property that mattered, not the dependency
count; that is also why `xxhash` **is** a dependency while the NBD client is
hand-written.

**"Two-level hash comparison."** Never needed. A whole-image digest first
and per-block only on mismatch exists to avoid sending many digests — but
the digests are tiny (≈41 KB for a 10 GiB disk), so the two-level scheme
optimised something that was never expensive. Per-block, always, is simpler
and localises damage for free.

What the note did get right: this is where the network win is. Verify stopped
transferring the replica and now exchanges a few bytes per megabyte.

**And one thing neither the note nor the first implementation got right:**
the first cut hashed the source, *then* asked the target — serialising two
reads that the byte comparator had always overlapped in one AIO pipeline. It
traded parallelism for network, which on a link fast enough that disk is the
wall is a straight loss. Splitting the plan (metadata only) from the hashing
lets both sides run at once, so a verify costs `max(source, target)` rather
than their sum: 45s → 27s on a real pair.

#### Two bugs the checksum work surfaced

- **`-verify=qemu-img` had been broken since F10** and was scoring false
  passes. F10 named the verify export, but the URL handed to `qemu-img`
  still asked for the *default, unnamed* export — so the handshake refused
  it and `qemu-img compare` exited 2 without comparing a byte, in ~3s
  against 9.5 GB. Every other comparator takes the export name as a
  parameter; this is the only one that formats its own URL.
  What hid it: `vmsync_verification_state` mirrored the run state, so bench
  read "the run failed" as "a mismatch was found" and a comparator that
  never compared anything passed three tamper tests. Only the sub-test
  expecting a *clean* result could catch it, and did.
- **A second wedge, independent of F2's.** The source's chain and the
  target's record of it advance by two different libvirt calls, and only the
  second can fail alone: `CreateCheckpoint` adds the checkpoint, but only
  `UpdateSyncMetadata` → `DefineDomain` records it on the target. A run that
  copied successfully and then failed that define left the source at
  `cpt-000002` with the target still saying `cpt-000001` — and every later
  incremental refused until someone ran `-reinit`. Fixed by
  `pending_checkpoint`, a write-ahead record on the target written *before*
  the source advances; the next run deletes the unaccepted tip and recopies
  from the last accepted checkpoint. Recopying from the older baseline is a
  superset of whatever the failed run wrote, which matters because the copy
  is per-disk and concurrent — adopting the newer checkpoint could declare a
  baseline the target only partly holds.

#### The metrics now say which kind of failure it was

`vmsync_verification_state` no longer mirrors `vmsync_sync_state`. It is
`0` = ran and matched, `1` = ran and found a difference, `2` = could not be
performed. `vmsync_checksum_state` uses the same encoding, and `2` there
also covers *skipped* — the state that is otherwise completely silent, since
a missing or version-skewed helper turns a default-on check off while every
sync still reports success.

Deliberately not split by cause: a failed SSH, a missing helper, version
skew and an export that would not start are one conclusion, and enumerating
them would mean classifying every error site while changing nothing an
operator does. Transience is a duration, not a state — use Prometheus's own
`for:` clause.

## Decided, deliberately not built

**A `qemu-img` confirming pass after a failed verify.** Proposed so that
"failed twice" would mean two independent implementations agree. **Rejected
by the operator:** `qemu-img` mode suspends the source VM, and pausing a
production guest to confirm a finding is too high a price for the
confirmation. The position taken instead is that once the verify path has
been exercised on real hardware, its own comparator is trusted enough to act
on. Revisit only if the comparator is ever itself suspected.

## Settled on hardware (2026-09-04)

Bench stages 13 (including 13e, real corruption) and 14 (including 14e, the
give-up branch) have run against a real pair and pass. Everything below has
therefore been exercised on hardware, not just built.

### F2.5 — persist and act on a verify verdict

**Built.** A verify failure used to be *nearly* ephemeral: an exit code, a log
line, and a `vmsync_verification_state` of 1 in the Prometheus textfile. That
was enough to **alert** on, and to tell a mismatch apart from a comparison that
could not run — but not enough to *decide* anything with. The metric lived in a
file the next run overwrote, and nothing on the domain recorded that this
replica had failed verification, so the next scheduled run had no idea it
happened and `evidenceProblems` handed out a clean bill of health at the worst
possible moment.

What exists now:

- **The record.** `verify_state=failed` plus `verify_failed_at` (unix seconds)
  on the target domain, written by `SetDomainMetadataFields` — the narrow merge
  — because a mismatch means the run failed and the full-redefine path is never
  reached. Written *before* the worker-error drain in `run()`, since that drain
  is what carries the mismatch out.
- **Only the verdict, never the evidence.** Which blocks differed is in the
  run's log, in far more detail than a metadata field could hold. Duplicating
  it would create a second copy to keep honest across reinits and restores.
- **Both refusals.** `TargetVerifyStateAllowsSync` refuses an ordinary sync
  *and* a plain `-reinit`. The second is the non-obvious half: a reinit
  recopies every byte, so it looks like a repair, but it never verifies the
  result — permitting it would move the domain from *known bad* to *assumed
  good, unverified* while erasing the record that said otherwise. Refusing is
  also what gives the record a lifetime at all, since a successful sync
  rebuilds the definition from the source's XML.
- **Two ways past, both deliberate.** `-verify-failure-reinit` recopies and
  re-verifies, clearing the record only by passing; `-force-clean` discards it
  and logs an ERROR saying so.
- **The ladder.** One full recopy and one more verify, then stop — driven from
  `main()` by a second `run(cfg)` call rather than by making `run()`
  re-enterable. It sets `ReinitAutomatic`, which keeps the restore points:
  those copies predate a replica now known to be wrong, so they are the only
  candidates for a clean one. Never a third attempt — each pass destroys the
  previous one's evidence, and "failed, then failed again after a full recopy"
  already separates a transient write error from a real one.
- **The promotion gate.** `evidenceProblems` reports a recorded failure, with a
  message deliberately distinguishable from the `failure_count` one: that says
  the replica may be **stale**, this says it may be **wrong**.
- **The exemptions.** The refusal is exempt from `failure_count` alongside
  `isVerifyMismatch` and `ErrRoleRefusesSync`. Counting it would only climb —
  the reinit it would force is refused by the same gate — and a non-zero count
  is itself reported by a promotion assessment, so it would stack a *may be
  stale* verdict on top of the real *may be wrong* one.
- **Surfaced** as `-verify-failure-reinit`, agent
  `verify_failure_reinit`, and a UI control; refused without `-verify` rather
  than ignored, in all three places.

Resolved along the way:

- The `role=paused`-plus-field question is settled in favour of a **field
  only**. `role` answers *what this domain is*; a verify failure is *what last
  happened to it*. A separate field keeps the target identity and keeps "clear
  the finding" and "resume replication" as distinct acts.
- Losing a finding to a reinit that then verifies clean is **acceptable** — the
  intermediate metrics and the logs both survive, and a passing whole-image
  compare is a real proof, not an assumption. This is also why an ordinary
  incremental is allowed past the record when `-verify-failure-reinit` is set:
  `-verify` compares the whole image, not just the extents that run wrote, so
  tonight's ordinary run can clear yesterday's finding on its own and the
  expensive recopy is spent only when a verify genuinely fails again.

Covered by bench stage 14 (`verify-failure`), five sub-tests: the record, both
refusals, the repair ladder, and the give-up branch.

The give-up branch needed a new fault to be testable at all, and the reason is
worth keeping. Corruption staged from outside is overwritten by the repair's
full recopy — that is what the recopy is *for* — so the second verify passes
and you get the repair sub-test instead, whatever you tamper with. So
**`-test=corrupt-after-commit`**: it writes over the replica **after** the copy has
been committed and confirmed, on every run, incremental or full, and therefore
fails both rungs of the ladder.

Where it writes was the whole design decision. Three placements were possible,
and they are not interchangeable — **two of them are useful, for different
checks**:

| placement | what it tests |
|---|---|
| the image the copy just wrote, before the pre-commit digest check reads it back | **`-test=corrupt-before-checksum`.** That check is expected to catch it, and this is the only way to test it against genuinely corrupted bytes rather than a falsified reply — see below |
| the overlay, after that check | nothing useful. The overlay exists only on an *incremental*, and the repair is a **full** recopy, so the fault would stop firing exactly where the ladder needs it |
| the committed base, after the restore point is taken | **`-test=corrupt-after-commit`.** Uniform across full and incremental, independent of whether the digest check ran, and it models the one corruption class nothing upstream can see: storage that went bad after a confirmed write |

### The pre-checksum fault, and why it is not the same test

`-test=corrupt-before-checksum` exists because bench's stage 13b tests the
pre-commit integrity check with a **shim that falsifies the helper's reply**.
That proves vmsync refuses a commit when *told* the digests disagree — the
plumbing. It cannot prove the check would notice actual wrong bytes, and the
ways it could fail to are not exotic: vmsync hashing ranges other than the ones
it wrote, the helper hashing the overlay's *backing* file instead of the
overlay, an off-by-one in the range plan. Every one of those passes 13b and
commits corruption in production.

Two details make it a test rather than a no-op:

- **The offset comes from the digest plan, not from a constant.** The check
  hashes only the ranges the run wrote, so a fixed offset would fall outside
  the plan on any small incremental, sail through unhashed, get committed, and
  read as a pass for a check that never looked at it.
- **It is refused unless the check is actually running** (`checksumEnabled`,
  which is only known after the helper is probed — so this cannot be a
  flag-parse-time check). With the check off there is nothing to catch the
  corruption and the fault would simply commit damage.

Its cost is asymmetric and worth stating: on an incremental it costs only the
discarded overlay, because the base is still untouched when the check runs. A
**full** sync has no overlay, so the check reads the base directly and a
mismatch there cannot be undone — the run fails loudly, and bench heals after
itself in that case.

### Details common to both

Two are load-bearing for the post-commit fault. It runs **inside `syncDisk`**, so it
completes before `measureReplicaWrittenAt("post-copy")` stamps
`replica_written_at` — the write bumps the file's mtime, and the other order
would make the *next* run refuse at the out-of-band-modification guard instead
of reaching its compare, a different refusal at a different stage that would be
scored as though the replica had been verified. (`tamper_target` in bench
restores the mtime by hand for exactly this reason; in-process the ordering
does it for free.) And it runs **after** `rp.take`, so the restore points stay
clean — more realistic, and it keeps them usable as the known-good candidates a
faulty replica's operator actually needs.

The post-commit one is refused without `-verify`, because without a comparison
to fail it is not a test, it is just damage.

Both use `qemu-io`, not `dd`, and that is not stylistic. The replica is always
qcow2, so a **file**-offset write lands in the qcow2 header or whatever
metadata happens to be there, and the image stops opening at all — `qemu-nbd`
then fails to export it, no comparison runs, and the outcome is "could not be
performed" rather than the mismatch the fault exists to produce. That is the
opposite of the test. Reaching a given **guest** offset with `dd` would mean
parsing `qemu-img map` for its host offset first, and would still have nowhere
to write if that cluster were unallocated. Both also `-c flush` after the write
and read back with `-t none`, because both read-back exports open
`--cache=none`: a fault sitting in dirty pages would be invisible to the very
check it exists to defeat, intermittently and depending on host memory
pressure. And `qemu-io`'s presence on the target is checked **before** the
copy — otherwise a missing package costs a full copy to discover, and the run
then fails for a reason unrelated to what was being tested.

Not yet proven:

- **Nothing here has run on hardware.**
- **Cost to know before enabling estate-wide:** on a 50 GiB VM a mismatch turns
  a ~30-minute run into a full recopy plus another full verify — hours, holding
  an agent concurrency slot throughout.

## Open

**Nothing from the audit's F-list.** F1–F13 and PORT-2 are all closed; what
remains is verification rather than work, and it is listed under "Built, not
yet run on hardware" and at the end of "Settled on hardware":

- Two `pending_checkpoint` recovery branches bench cannot construct — a
  mid-chain pending record, and the source-shut-down case. Both are covered by
  `TestPlanCheckpointRecovery`; neither has run against libvirt.
- Whether a digest verify's cost is now dominated by the source-side read
  through the fleecing export. The `nbd digest complete` line reports its own
  elapsed time; comparing that against the run's total verify time answers it.

### Historical: how the checksum design was arrived at

**Kept for the reasoning, not as open work** — everything below was decided
and built (F12, F13 above). It is here because two of these conclusions get
re-proposed otherwise, and because one of them turned out to be wrong in a
way worth remembering: the section argues for a *byte* comparison before the
commit, and the shipped F12 uses digests instead. The reason is stated in
F12's entry — the copy streams, so a byte compare would have to re-read the
source as well, doubling the I/O this was meant to make cheap.

The starting idea: add rolling checksums to the sync — checksum the source,
checksum the target, commit only if they agree, otherwise call it a
transport or write error and discard the target's temporary qcow2. Then
reuse that machinery as the hash comparison for verify.

Two conclusions came out of working through it, and they are recorded here
because both are the kind of thing that gets re-proposed otherwise.

#### Rolling checksums buy nothing here. Per-block does.

A rolling checksum solves exactly one problem: finding matching content at
**unknown offsets**. That is rsync's problem — two files where bytes may have
been inserted or removed, so a block on one side corresponds to some
arbitrary byte offset on the other. The rolling window makes sliding a byte
at a time cheap, and the price is a two-tier scheme (weak rolling hash to
find candidates, strong hash to confirm).

vmsync has no alignment problem. The dirty bitmap names exactly which extents
changed and they are written at **identical offsets** on the target; block N
is always block N. The entire mechanism rolling checksums exist for is dead
weight. What is wanted is **per-block digests at fixed offsets**.

#### Location matters, and a two-level scheme gets it cheaply

The 2026-09-01 incident settled this: "260 ranges scattered across 50 GiB"
versus "one 4 KiB range" is what distinguished guest drift from corruption. A
bare "differs" would have told nobody anything, and a healthy replica would
probably have been reinit'd on the strength of it. Location also opens
**targeted repair** — rewrite the differing extents rather than recopy 50 GB,
which is a better answer than F2.5's reinit ladder.

Both are available at once. One digest for the whole image; if it matches,
done at minimum cost, which is the overwhelmingly common case. Descend to
per-block digests only on mismatch. Cheap when healthy, precise when not.

Worth naming the trade: a byte comparison yields exact differing ranges for
free, because it already holds both streams. A digest yields "differs". The
two-level scheme claws most of that back but does not make it free.

#### The reframing: the sync integrity check IS a verify

Hashing "what was sent" against "what the helper received" catches only
*transport* corruption. Catching a bad **write**, or storage that corrupts it
afterwards, means reading the bytes back off the target and comparing them —
which is verify, exactly.

And there is already a clean abort point. In incremental mode the copy writes
to an overlay and only then commits it into the base
(`copyAndCommit`, in `cmd/vmsync/main.go`), so between copy and commit the replica's base is
still untouched.

So the idea is better served by scoping and timing an existing operation than
by adding a new checksum format. That is F12.

#### How much a transport check would actually buy

Honest accounting, because it affects priority. s2 and zstd frames carry
their own checksums, so corrupt compressed data overwhelmingly fails to
*decompress* rather than silently decoding to wrong bytes; `-use-ssh` adds a
MAC on top. The genuinely exposed case is plain TCP with no compression,
relying on TCP's 16-bit checksum. Against **transport**, an estate running
`-compress` is already reasonably covered.

What nothing currently catches is corruption **after** the write. Only a
read-back finds that — which is the argument for F12 rather than for stream
checksums.

## Closed after this list was written

### F9 — `WaitForTCPExport` asymmetry

The two call sites disagreed. `copyAndCommit` guarded the probe behind
`if !bridgeCfg.Enabled()`, so a bridged run got no readiness check at all;
`runVerify` called it unconditionally against `targetNBDHost:verifyPort` — an
address that with `-compress`/`-netbuffer` **plus** `-use-ssh` this host cannot
reach, since the point of `-use-ssh` is that only the SSH connection crosses.
It timed out for ten seconds and failed the verify while the export was
healthy and reachable through the bridge it had not yet built.

This entry proposed replacing the dial with a remote `ss` probe, so the check
would run where the export is and work in every mode. **Rejected by the
operator, on the grounds that a dial proves something `ss` structurally
cannot: reachability.** A firewall between the hosts, or a bind address that
does not cover the route, leaves the port listening and the copy still unable
to start — so an `ss` probe would report ready and the copy would then fail
anyway, having replaced a wrong answer with a confident wrong answer.

What both call sites had in common was probing a FIXED address rather than the
one in use. So the dial is kept, and pointed at
`effectiveTargetHost/Port` — whatever the copy or compare will really connect
to. That is `127.0.0.1:<local bridge port>` when bridging and the target host
otherwise, so the probe always speaks plain NBD (the bridge's remote end
speaks the compressed protocol and would reject a plain handshake) and always
follows the real path. Through a bridge the handshake transits the whole
chain, proving more than probing the export directly ever did: the local
helper, the link, the remote helper, and the export behind it.

Three further points, all from the operator's direction that dial probing
should happen everywhere it can:

- The **source** export now has a probe, which it never had. An unreachable
  source used to surface from inside `ChangedExtentsTCP` as whatever libnbd
  said about a failed connect, reading as "the extent query broke" rather than
  "this host cannot reach the source". It asks for the disk's target device as
  the export name, so it proves *this disk's* export is being served.
- The failure says **not reachable**, and lists what that can mean.
- A probe failure is **exempt from `failure_count`** (`nbdsync.ErrExportUnreachable`),
  joining the verify mismatch and the two role refusals. The reasoning is the
  sharpest of the four: nothing this error can mean is repaired by what the
  counter would eventually force. A firewall, a bind address, a non-relaying
  bridge, an export that bound and then died — a full resync addresses none of
  them, so the count would climb until it triggered a recopy that cannot help,
  blocking promotion the whole time. It exempts the died-after-binding case
  too, which *is* a broken mechanism; that is deliberate, because a dial
  getting no handshake cannot tell it apart and a reinit does not fix it
  either.

### PORT-2 — auto-allocation block overlap, and the port work it pulled in

The diagnosis was right and the prescription was wrong, which is worth
recording because the prescription was nearly built.

The diagnosis: `blockFree` infers ownership from live sockets, but a run binds
its blocks at wildly different times — the verify exports stay unbound for the
whole copy — so a second run probing in that window sees them free. This entry
called for "a real reservation plus one host-wide mutual-exclusion point held
across probe → read → write", and the plan was to claim each block with
`util.AcquireRemoteRunLock` (a remote `flock` held for the run, released when
the SSH connection drops).

What settled it instead: **bind the port and, on failure, take another.** The
bind *is* the reservation, which makes the kernel the arbiter — the only
authority that cannot be stale. It needs no lock, no registry and no
cross-process coordination, and it additionally covers a port held by
something that is not vmsync at all, which no amount of vmsync-side
bookkeeping could. The lock would have been a weaker copy of a fact the kernel
already holds.

Three consequences fell out rather than being aimed at:

- **F4** is implemented, because a retry loop *must* clean up between
  attempts or an attempt that bound its port and then failed leaves a
  `qemu-nbd` holding the replica image open. It runs the stop string directly
  with a fresh `context.Background()` rather than pre-registering it, exactly
  as F4 warned: `pollStopCommands`' index is monotonic, so pre-registering
  burns the one-shot slot before the pidfile exists.
- **F7** is gone rather than guarded. A fixed spec yields `fixed, fixed+1, …`
  and the iterator simply stops at 65535, so there is no `spec.Fixed+need-1`
  to overflow.
- **F8** is gone by deletion. `sourcePortsNeeded` existed to predict whether a
  second source port would be wanted; the bridge now draws its own port and
  binds it, so nothing predicts and the drifted predicate has no job.

Also fixed, and the reason this was urgent: **an agent-managed estate had no
port separation at all.** `source_port_range`/`target_port_range` exist on
`SyncProfile` but nothing sets them, so every VM fell back to the single fixed
default of `20809` — with `max_concurrent_syncs` at 4, two VMs syncing to one
target host shared every port. The default is now a *range* on both sides, and
`auto` is retired as a keyword since it came to mean exactly what passing
nothing means.

The agent deliberately does **not** track which ports its children hold, and
does not partition the range between them. A registry there would be the
stale-prediction problem relocated: it could only ever know its own children,
never a second agent replicating into the same target host, so it would create
confidence it could not honour — and slicing the range would reintroduce the
artificial scarcity that dropping contiguity removed.

Two smaller decisions inside it:

- The starting offset is now **random per run** rather than derived from the
  domain name. Not for collision probability — a collision is handled — but
  because a derived offset makes a vm *permanently* unlucky if it hashes to
  where something else already sits. The offset is logged, so a run stays
  reproducible from its log rather than from its name.
- The **source export** keeps probe-then-bind, deliberately. A probe is only
  as good as the delay before it is trusted: on the target that was the whole
  copy, here it is seconds. And the cost of the alternative is different in
  kind — this port is bound by a libvirt backup job on a *production* domain,
  where libvirt permits one asynchronous job at a time, so retrying means
  beginning and aborting jobs against a live guest to dodge a collision the
  probe already prevents. The source bridge, an ordinary helper process, did
  move over.

`contrib/runner/vmsync-parallel.sh` still advances a base port per VM. That
now solves a problem that no longer exists — a fixed port means "start here" —
so it can drop both flags whenever it is next touched.

## Settled on hardware (2026-09-02)

Every item this section previously listed as unverifiable from the repo has
now run against a real pair, with all bench stages passing:

- A fixed `-verify=full` returns clean on a real pair, and the negative
  control holds: a tampered block is still reported, by both the digest and
  the byte comparator (`verify-fast`/`verify-full` and their `-bytes`
  variants).
- libvirt's pull-backup fleecing view **does** report `base:allocation`
  faithfully — F11's skip works against it, and the numbers above are
  measured through it.
- `qemu-nbd --export-name` works, and so does `--socket` combined with it,
  which is what the pre-commit check's export uses. That combination had
  never been exercised before this run.
- The pre-commit check demonstrably *fires*: bench stage 13 refuses a
  falsified digest, removes the overlay, leaves the base intact, reports
  version skew as skew rather than as corruption, and honours
  `-no-checksum`.
- `pkg/nbdclient` speaks to a real `qemu-nbd`, not only to the fake server
  in its own tests. The fake can prove the client matches the specification;
  only this proves qemu agrees.

### Still unverified

- The `pending_checkpoint` recovery has been proven for the ordinary case
  (bench stage 12). Two branches remain unexercised because bench cannot
  construct them: a **mid-chain** pending record, which must refuse rather
  than delete; and the **source shut down**, where the delete cannot merge
  the dirty bitmap and must refuse with the remedy rather than fall back to
  a metadata-only delete. Both are unit-tested
  (`TestPlanCheckpointRecovery`); neither has run against libvirt.
- Whether a digest verify's cost is now dominated by the source-side read
  through the fleecing export. The `nbd digest complete` line reports its own
  elapsed time; comparing that against the run's total verify time answers
  it, and decides whether anything further is worth optimising.
