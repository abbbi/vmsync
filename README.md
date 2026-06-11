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

# Limitations

 * Both source and target libvirt host should run on the same libvirt/qemu
   version/distribution
 * The utility at its current state does not identify custom specified
   kernel/nvram/tpm devices
 * Special devices like iso files attached to the cdrom are not copied

# Build

Several components are required to build from source, see the provided
Dockerfiles for example.
