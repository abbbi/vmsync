# vmsync

vmsync: incrementally replicate libvirt based virtual machines to remote hosts using dirty bitmaps.

This utility can be used to sync or "replicate" virtual machines to other libvirt hosts. On the first execution, a complete (full) replication will be executed and an dirty bitmap is created. For any following command calls, it will only synchronize incremental changes since the last checkpoint.

See this link for a full explanation/releases:

https://grinser.de/~abi/vmsync/
