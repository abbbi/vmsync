# vmsync
 
incrementally replicate libvirt based virtual machines to remote hosts using
dirty bitmaps.

This utility can be used to sync or "replicate" virtual machines to other
libvirt hosts. On the first execution, a complete (full) replication will be
executed and an dirty bitmap is created. For any following command calls, it
will only synchronize incremental changes since the last checkpoint.

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
and `-target-nbd-port` each accept three forms:

```bash
vmsync -target-nbd-port 20809          # fixed base port
vmsync -target-nbd-port 20000-20100    # pick a free block inside this range
vmsync -target-nbd-port auto           # pick inside the default range
```

With a range or `auto`, vmsync asks the host which ports are listening and
takes a free contiguous block. Without it, two syncs running at once both
try the fixed default and one fails to bind — so a range is what makes
concurrent syncs to the same host work.

How many ports a run occupies, for `N` disks:

| side | plain | `-compress`/`-netbuffer` | `-verify` | both |
| --- | --- | --- | --- | --- |
| target | `N` | `2N` | `3N` | `4N` |
| source | 1 | 2 | 1 | 2 |

Size ranges from the largest VM you replicate, not the average — a 4-disk
VM with compression and verification needs 16 consecutive target ports.
vmsync refuses to start, naming the shortfall, rather than binding outside
the range you gave it.

The search starts at an offset derived from the target domain name, so
different VMs replicating into the same host tend to pick different blocks,
while a given VM lands on the same ports run after run as long as the
host's occupancy is unchanged — which keeps firewall logs and packet
captures meaningful.

Passing a fixed port keeps the previous behaviour exactly, including
skipping the probe entirely: use it where firewall rules are pinned to
specific ports.

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
jobs, manual invocations and any external tooling are all bound by it.

A role this build does not recognize is also refused, on the assumption it
was written by a newer version — failing closed costs an error message,
failing open would cost data.

# Limitations

 * Both source and target libvirt host should run on the same libvirt/qemu
   version/distribution
 * The utility at its current state does not copy custom specified
   kernel/nvram/tpm devices
 * Special devices like iso files attached to the cdrom are not copied

# Build

Several components are required to build from source, see the provided
Dockerfiles for example.
