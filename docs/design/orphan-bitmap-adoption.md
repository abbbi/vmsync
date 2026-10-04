# Clearing an orphaned dirty bitmap by adopting it

*Every claim about current behaviour cites a file and line that was read. Unqualified paths are `cmd/vmsync/`; the libvirt wrapper is `pkg/libvirtsync/`. Unlike the other documents here, this one describes code that is **implemented and measured**: the mechanism was proven against libvirt and qemu on hardware on 2026-10-04 (`contrib/bench` stage 22), and the engine's use of it end to end on the same day (stage 23). Where a claim rests on reading libvirt's source rather than on a measurement, it says so.*

---

## The problem

A libvirt checkpoint **is** a persistent dirty bitmap in the qcow2. The record and the bitmap are two halves of one thing, and losing the record while keeping the bitmap is the one failure state that is both permanent and invisible:

1. Something drops libvirt's record of checkpoint `vmsync-cpt-00000N` without removing its bitmap. The documented causes are a `--metadata`-only delete (`pkg/libvirtsync/libvirt.go:3774`, `DeleteCheckpointIfExists`, whose doc comment exists to keep anyone from reaching that flag), `DeleteAllManagedCheckpointsMetadataOnly` used without its bitmap half, or a run that dies between qemu's transaction and libvirt's metadata commit.
2. `virsh checkpoint-list` now shows nothing. Nothing in libvirt's view explains anything.
3. The next sync asks `NextCheckpointName` for a name, gets `vmsync-cpt-00000N` again, and qemu refuses the create: **`Bitmap already exists: vmsync-cpt-00000N`**.
4. Every subsequent sync for that pair fails identically, for ever, until somebody removes the bitmap by hand.

Before this change the only remedy was to shut the source down and run `qemu-img bitmap --remove`, or to `-reinit` an offline source — a full sync's downtime to fix a metadata bookkeeping error.

## What does not work, and why

**`qemu-img bitmap --remove` on a live image.** qemu holds the image open and takes a write lock; `qemu-img` is refused. Measured every run of stage 22: `Failed to get "write" lock`. This is the premise the whole design is built on, and the stage checks it rather than assuming it, because if it ever *succeeded* the simple fix would have been available all along.

**Raw QMP `block-dirty-bitmap-remove` via `qemu-monitor-command`.** It would work, and it is rejected on different grounds: it goes behind libvirt's back on a running guest's block layer, and libvirt marks a domain tainted once it has been used. vmsync's refusal message used to claim this was *the only* live route (it no longer does — see "Claims retracted" below).

**Dropping the metadata and waiting.** This is the orphan factory, not a fix.

## The route

Two steps, both through libvirt:

1. **Adopt.** `virDomainCheckpointCreateXML` with `VIR_DOMAIN_CHECKPOINT_CREATE_REDEFINE` and metadata naming the bitmap that already exists (`pkg/libvirtsync/libvirt.go:3664`). This gives libvirt back a record for the orphan.
2. **Delete.** `virDomainCheckpointDelete` with **flags 0** — an ordinary delete, never `METADATA_ONLY`. This is what has qemu remove the bitmap, through its own path.

`ClearOrphanBitmap` (`pkg/libvirtsync/libvirt.go:3699`) is the pair; `buildAdoptionXML` (`:3627`) is the description libvirt accepts.

### What libvirt requires of the metadata

Established by trying four shapes against a real libvirt, cheapest first, and reporting which one it took (stage 22's P3b, `contrib/bench/bench.sh:8562`):

| shape | result |
| --- | --- |
| `<name>` + the disk with `bitmap=NAME` | **refused** — `missing creationTime from existing checkpoint` |
| the above **+ `<creationTime>`** | **accepted** |
| + the dumped `<domain>` definition | not needed |
| + a `<parent>` | not needed |

So the minimum is name, `creationTime`, and one `<disk>` per domain disk. Three details that are not obvious and are load-bearing:

- **`creationTime` is required and its value is validated against nothing** — not against other checkpoints, not against the image. `ClearOrphanBitmap` passes the clock.
- **Disks that do not carry the bitmap must be named with `checkpoint="no"`, not omitted.** libvirt's own checkpoint dumps do this. Claiming a bitmap on a disk that has none makes the *delete* fail looking for it, and that delete is the entire point.
- **No `<parent>`.** This adopts into a chain that has just been dropped, and naming a parent that no longer exists is refused. It also keeps the adopted record childless and parentless, which matters below.

`buildCheckpointXML` (`:3595`) is deliberately a separate function: it builds the *creation* description, which must not carry `creationTime` and must not mark disks `no`.

## Why this cannot corrupt an image

Four facts, each from libvirt's or qemu's own source or documentation rather than from reasoning about what ought to happen. This section exists because the question *"are we sure this cannot corrupt the qcow2?"* is the right question to ask of it and the answer is not self-evident.

1. **The adopt step writes nothing to any image.** A REDEFINE issues no QMP command at all; libvirt writes one file, `<checkpointDir>/<domain>/<name>.xml`. Corroborated by the existence of a separate `VIR_DOMAIN_CHECKPOINT_CREATE_REDEFINE_VALIDATE` flag, whose whole purpose is to make libvirt go and inspect the images — `ClearOrphanBitmap` does not pass it. So step 1's worst case is "the orphan is unchanged".

2. **The delete step emits only bitmap removals, and no merge.** libvirt's `qemuCheckpointDiscardBitmaps` collects one `block-dirty-bitmap-remove` per disk into a single `actions` array and executes it with one `qemuMonitorTransaction` (libvirt v10.10.0, `src/qemu/qemu_checkpoint.c`). There is no `block-dirty-bitmap-merge` anywhere in the delete path, for a checkpoint with children or without. A merge is the only operation that writes into *another* bitmap, so nothing else's tracking is touched.

   Being one transaction also means the multi-disk delete is **atomic**: it cannot remove the bitmap from the first disk and fail on the second.

3. **Removing a bitmap affects nothing else.** qemu's own documentation: deleting a persistent bitmap removes it from the qcow2 and *"does not impact any other bitmaps attached to the same node, nor does it affect any backups already created from this bitmap or node."* What it rewrites is the bitmaps header extension, the directory entry, the bitmap table and its data clusters, plus the refcount blocks for the clusters freed. Bitmap clusters are host clusters no L1/L2 entry points at; guest data clusters are not read or written.

4. **The destructive half is the path already in routine production use.** `cp.Delete(0)` is the same call `DeleteAllManagedCheckpoints` and `PruneCheckpointsOlderThan` make on every ordinary prune. Adopt-then-delete introduces no new kind of write to the image; the only novel step is the one that touches no image at all.

Also worth knowing, because it inverts an intuition: **libvirt refuses a non-metadata-only checkpoint delete on an *inactive* domain** (`qemuCheckpointDiscard`: `cannot remove checkpoint from inactive domain`). Removing a bitmap this way is not merely tolerated on a running domain — running is the only state in which it is possible at all.

## The premise the safety argument rests on

**Every checkpoint's bitmap records independently and continuously. Creating a checkpoint does not stop its predecessor.**

libvirt's `qemuCheckpointAddActions` emits one `block-dirty-bitmap-add` per disk, passing `disabled=false`, and issues no `block-dirty-bitmap-disable` against the parent's bitmap (v10.10.0, same file). So the bitmap of the last checkpoint the target accepted still covers everything the guest has written since, including everything an orphan also recorded. **An orphan's dirty bits are a subset of bits that survive it**, and discarding them cannot shorten a later incremental.

This is written down because the opposite would have been a silent-data-loss bug, and it is exactly the shape a reviewer should look for:

> Suppose creating a checkpoint *froze* its predecessor's bitmap. Then an orphan named `tip+1` would hold the only record of every block written while it existed — hours or days. Adopting and deleting it would discard that record, the next incremental from `tip` would copy too little, and the replica would diverge permanently with a green exit code and healthy metrics. The orphan's name makes this reachable rather than theoretical: an orphan left by a failed advance is named `tip+1`, which is precisely the name the next run asks for, which is precisely why the self-heal fires.

Three independent reviews of this change each constructed that scenario and each flagged the premise as the thing that decided it. It was settled by reading libvirt's checkpoint-creation path, not by argument. If a future libvirt or qemu changes it, **this feature becomes a data-loss bug** — which is why the premise is restated in `ClearOrphanBitmap`'s doc comment (`pkg/libvirtsync/libvirt.go:3699`) rather than left to this document.

## Failure modes, and the direction each fails in

| what fails | result | loud? |
| --- | --- | --- |
| the adopt is refused | the orphan is untouched; the caller reports it | yes, with libvirt's own message |
| the delete fails | rolled back with `METADATA_ONLY`, restoring exactly the prior state — an orphan with no record | yes |
| the delete *and* the rollback fail | libvirt lists a checkpoint whose bitmap state is unknown; the error names it and says to check `virsh checkpoint-list` and `qemu-img info` | yes |
| the adopted metadata claims a bitmap a disk does not have | the delete fails looking for it, then rolls back | yes |
| killed between the adopt and the delete | a parentless record whose bitmap exists. A later `-reinit` clears it properly; a later ordinary run may delete it as a stale tip | loses change-tracking only, and the premise above means nothing needed it |
| process death mid-removal | leaked clusters (`qemu-img check -r leaks`) or a bitmap left `+inconsistent` at next open, whose only valid operation is removal | loss of tracking, loud at next use, not guest data |

The `METADATA_ONLY` rollback deserves its own note: that flag is poison in general — it is what *creates* orphans, and `DeleteCheckpointIfExists` refuses to go near it. It is correct in exactly one place, which is the rollback, because the record being dropped was created moments earlier by the same function, so dropping it restores the state the function was called in rather than inventing a new orphan.

## Where it is used

Two callers, for two different moments:

**`-reinit`, after the chain drop** (`cmd/vmsync/main.go:4468`). The chain has just been removed the proper way, so a vmsync-named bitmap still in the images is an orphan by definition. `clearOrphanBitmapsOnline` (`cmd/vmsync/bitmaps.go:210`) clears each one, the images are re-read, and the refusal survives only for what could not be cleared. `adoptionPlan` (`:275`) groups the work **per bitmap name rather than per disk**, because one vmsync checkpoint puts the same name on every disk it covers and adopting per disk would ask libvirt to redefine the same checkpoint twice. A bitmap in a file the domain does not list is reported as a failure, not skipped — there is no target device to name it with.

A shut-down source needs none of this: `dropCheckpointsOffline` (`cmd/vmsync/failover.go:1109`) removes the bitmaps with `qemu-img` directly, orphaned or not, because it can. The route is chosen from the source's own state (`dropCheckpointChain`, `:1080`), never from which flag was passed.

The pre-drop check (`cmd/vmsync/main.go:4373`) used to **refuse** a running source carrying orphans, because nothing could clear them and stopping before anything was displaced was the kindest answer available. It now says what it found and proceeds.

**`CreateCheckpoint`, on collision** (`pkg/libvirtsync/libvirt.go:3467`). When qemu refuses a checkpoint for `Bitmap already exists`, the bitmap is cleared and the create is retried **once, never in a loop**. Three guards: the name must parse out of the error (`collidingBitmap`, `:3542`), must be the name that was asked for, and must satisfy `IsManagedCheckpointName`.

This second caller is not redundant, and on at least one real host it is the *only* one that fires. `-reinit`'s sweep enumerates orphans by reading the qcow2 bitmap directory, which does not carry a bitmap created during the current qemu process's life — so a fresh orphan is invisible to it. qemu refusing the create is the one moment the collision is certain. Measured on hardware 2026-10-04: stage 23 reported `cleared by the self-healing retry in CreateCheckpoint`, not by the sweep.

`collidingBitmap` matches **qemu's** words, not libvirt's. libvirtd formats its own messages server-side in the daemon's locale, so on a French host the wrapper arrives translated — `erreur interne : Impossible d'executer la commande QEMU 'transaction' : Bitmap already exists: vmsync-cpt-000001` — while the qemu half comes through untranslated because libvirt appends it verbatim. The test keeps that exact string as a case (`pkg/libvirtsync/libvirt_test.go:1505`).

## How it is tested

**Unit, decisions only:** `buildAdoptionXML`'s three load-bearing details (`pkg/libvirtsync/libvirt_test.go:1551`), `collidingBitmap` including the translated form (`:1505`), and the planning and error assembly (`cmd/vmsync/bitmaps_test.go:166-262`). `clearOrphanBitmapsOnline` takes the removal as a function so every decision in it is testable without a libvirt connection.

**Hardware, mechanism:** stage 22 `redefine-probe` (`contrib/bench/bench.sh:8562`) asks libvirt and qemu directly whether the route exists, makes its own clean starting point, and reports which XML shape was accepted. It grades libvirt rather than vmsync, so the two "which host behaviour is this" measurements are recorded rather than failed.

**Hardware, end to end:** stage 23 `reinit-orphan` (`:9154`) has vmsync build a real orphan — a seeding `-reinit`, then drop libvirt's record of the checkpoint it made, so the bitmap is on every disk vmsync manages as a real one is — and then runs a plain `-reinit` over it. The planted name is `vmsync-cpt-000001`, the name the rebuild's own chain takes, so **a clean exit is itself the proof the orphan was cleared**: a surviving one is exactly what makes qemu refuse that checkpoint.

The sync it runs afterwards carries `-verify=fast`, and that is the only check in either stage that speaks about **data**. Everything else establishes that the bitmap went and the metadata is consistent; a replication tool can get both right and still have copied the wrong bytes. Measured 2026-10-04: incremental, 5 MB transferred, verify clean.

Both stages share their bitmap and checkpoint helpers (`:8184`).

## Limits

- The libvirt facts in "Why this cannot corrupt an image" and "The premise" come from reading **libvirt v10.10.0**'s source. They are version-specific by nature. The empirical check on a given build: create checkpoint A, write in the guest, create B, and compare the dirty extents reported for A against B — if A's set includes the post-B writes, A kept recording and the premise holds.
- The write ordering that makes a crash mid-removal produce leaked clusters or an `+inconsistent` bitmap, rather than a damaged image, was **not** verified against qemu's `qcow2-bitmap.c`. Both outcomes are loss of change-tracking, loud at next use; the conclusion is believed, not established.
- `-verify` proves the replica matched the source for **that** interval. It cannot prove the tracking will be right for every future one. Over-copying would also pass, which is the safe direction.
- `-reinit`'s sweep cannot enumerate an orphan that is not yet in the image. That gap is covered by the `CreateCheckpoint` retry, not closed.

## Claims retracted while this was being established

Recorded because each was believed, written down, and wrong — and because the wrong versions are the ones a reader is likely to re-derive.

- **"`qemu-img` cannot see a running domain's bitmaps."** It can. It reports what was in the file when qemu opened it. What it cannot see is a bitmap created *since*, which reaches the image when something flushes it — a restart will. Both readings were observed on the same host two days apart.
- **"Stopping the domain settles whether a bitmap is invisible or absent."** It does not: a clean shutdown is itself when qemu writes a persistent bitmap out, so a positive reading afterwards may be the shutdown's own doing. The live oracle — asking qemu to create the name and seeing whether it refuses — answers it without perturbing it, and is what stage 22 uses.
- **"`qemu-img check` is the integrity check for this."** It is not, and it was removed from stage 23. It needs the domain down, so running it means shutting down the very thing whose live behaviour is under test; and it validates refcounts and cluster allocation, so an image whose bitmap has quietly lost dirty bits passes it and then under-copies. A check that cannot fail for the reason you care about is worse than none, because it reads as reassurance.
- **"Deleting a checkpoint merges its bitmap into the next one."** Stated in eight comments across the engine. libvirt's own API documentation says the merge goes into the *parent*; its modern qemu driver performs no merge at all. The operational conclusion those comments drew — that only a live qemu can delete a checkpoint properly — is correct for a different reason, which is the inactive-domain refusal above.
- **"The only live route is raw QMP behind libvirt's back."** The refusal message in `leftoverBitmapRefusal` (`cmd/vmsync/bitmaps.go:122`) said so. It now carries the adopt-then-delete recipe an operator can run without stopping the guest.
