# nbdbridge

## Topology

vmsync itself runs wherever it's invoked from -- commonly the source host (`-source-uri
qemu:///system`, a local libvirt connection), but it can equally
run on a third, separate host, reaching the source over SSH instead (`-source-uri
qemu+ssh://src/system`). The target is always reached over SSH (`-target-uri
qemu+ssh://target/system` is required -- vmsync refuses to start otherwise, since it needs to
create/manage files and processes there remotely).

This matters for the port tables below: every source-side connection/port described here
only exists at all when the source is itself remote (`-source-uri` uses `qemu+ssh://`). When
vmsync runs directly on the source host, its NBD reads never leave that host in the first
place, so there's nothing there to bridge or secure.

### vmsync standalone

vmsync can run in standalone mode, in which case it will open an SSH connection to the target host,
and sync the VMs over a separate plain TCP connection.

### vmsync-bridge-helper

When using compression or buffering, vmsync will need a helper binary, which needs to be put in
`/usr/local/bin/vmsync-bridge-helper` on the target side.
This will allow two topologies:

- SSH connection and plain TCP connection
- Single SSH connection with port forwarding with `-use-ssh`, in which case data connections are encrypted by SSH 

## Ports to open in firewall, by run mode

Ports below use this repo's own flag defaults; `-source-nbd-port`/`-target-nbd-port`/
`-ssh-port` change the actual numbers but not the shape. `N` is the number of disks in the
domain being synced; disk index `i` runs `0..N-1`. Rows marked "always" apply regardless of
mode; each mode below that adds its own rows on top.

| Run mode | Host | What's listening | Bind (default) | Port(s) (default) |
|---|---|---|---|---|
| always | Target | SSH, control plane (`qemu-img`, `qemu-nbd`, checkpoint/backup calls) | -- | `-ssh-port` (`22`) |
| always, only if `-source-uri` is `qemu+ssh://` (source is remote -- see Topology above) | Source | SSH, control plane | -- | `-ssh-port` (`22`) |
| always | Source | libvirt backup NBD export (one, shared across all disks, differentiated by export name) | `-source-nbd-bind` (`0.0.0.0`) | `-source-nbd-port` (`10809`) |
| always | Target | `qemu-nbd`, one TCP export **per disk** | `-target-nbd-bind` (`0.0.0.0`) | `-target-nbd-port + i` (`20809`, `20810`, ...) |
| `-compress`/`-netbuffer`, direct (default, no `-use-ssh`) | Target | vmsync-bridge-helper, one **per disk** | `0.0.0.0` | `-target-nbd-port + N + i` -- the block right after all `N` plain export ports above |
| `-compress`/`-netbuffer`, direct, only if source is remote (see Topology above) | Source | vmsync-bridge-helper, one shared | `0.0.0.0` | `-source-nbd-port + 1` |
| `-compress`/`-netbuffer` + `-use-ssh` | -- | nothing additional -- bulk data tunnels through the SSH port(s) already listed above | -- | -- |
| `-verify` | Target | `qemu-nbd`, **read-only**, one per disk | `-target-nbd-bind` | `-target-nbd-port + 2*N + i` -- a third block, after the plain and bridge ranges, so it never collides with either regardless of whether bridging is active |

Notes:

- All of the above are **plain, unencrypted, unauthenticated TCP**, except SSH itself and
  anything tunneled through it with `-use-ssh`. Route the rest through a VPN or
  otherwise-trusted network if they cross anything untrusted.
- vmsync's own local relay (`StartLocal`, this package) also opens one listener per bridge,
  but always on `127.0.0.1` with an OS-assigned ephemeral port -- never reachable from
  outside the host vmsync runs on, so it's not a firewall concern.
- `-verify`'s read-only export is always dialed directly over plain TCP from wherever vmsync
  itself runs, and does **not** tunnel through `-use-ssh` even if that flag is set for
  `-compress`/`-netbuffer` -- see `-verify`'s own flag help for why (it needs the source's
  own local file, not something vmsync's existing SSH tunneling -- built only for the source
  dialing out to the target -- can route for this one-off comparison).

## vmsync-bridge-helper

Compression and/or buffering features require to setup a helper binary on target system.
Usually the helper binary needs to be deployed in `/usr/local/bin/vmsync-bridge-helper`

Once the helper binary is setup, you can use `-compress` or `-netbuffer` or `-use-ssh` parameters.

### vmsync-bridge-helper internals

When using compression / netbuffer, the bridged NBD traffic goes directly between hosts
over plain TCP by default -- not through SSH. Set -use-ssh to route it through the existing
SSH connection instead, as an encrypted tunnel; see -use-ssh below for the tradeoff. Either
way, nbd being a bidirectional protocol, both directions (incoming/outgoing traffic) are
compressed/buffered independently, for both source and target.

In order to achieve bidirectional communications, we launch vmsync-bridge-helper on the
target, listening on 0.0.0.0:bridgeport by default (127.0.0.1:bridgeport with -use-ssh;
bridgeport = targetport + N, where N is the domain's total disk count -- the bridge ports
occupy the contiguous block right after all N real export ports). For each accepted
connection it dials the real, plaintext NBD
export directly (no external tool -- no socat, no shell -- in between) and relays both
directions natively (pkg/zstdrelay), instead of shelling out to external
compression/buffering CLI tools -- those buffer on EOF only, which is fundamentally
incompatible with NBD's long-lived, synchronous, small-message protocol.

Example with vmsync --compress --netbuffer=64k,1G (default: direct, no -use-ssh)

 vmsync host (local)                                       Target host (remote, reached via SSH for orchestration only)
 ════════════════════                                      ═══════════════════════════════════════════════════════════
                                                           For EACH disk i independently (N = total disk count):
                                                           qemu-nbd real port  = TargetNBDPort + i
                                                           bridge port         = TargetNBDPort + N + i  (= qemu-nbd port + N)
 ┌── disk 0 ──────────────────────────┐                    ┌── disk 0 ─────────────────────────────────────────────┐
 │ nbdsync.CopyExtentsTCP             │                    │  qemu-nbd --fork --persistent --format=qcow2          │
 │      │ dials 127.0.0.1:localPort0  │                    │           --bind 0.0.0.0 --port <targetPort>          │
 │      ▼                             │                    │           --pid-file /tmp/vmsync-qemu-nbd-...pid      │
 │ ┌─────────────────────────────┐    │                    │  (this is the REAL, uncompressed NBD server,          │
 │ │ Go relay (StartLocal)       │    │                    │   listening on 127.0.0.1:<targetPort> only)           │
 │ │ 127.0.0.1:<localPort0>      │    │                    │              ▲                                        │
 │ │  (net.Listen "tcp", :0 --   │    │                    │              │ TCP (plaintext)                        │
 │ │   ephemeral, one per disk)  │    │                    │              │                                        │
 │ │                             │    │                    │  ┌───────────┴──────────────────────────────────┐     │
 │ │ outbound (to target):       │    │                    │  │ vmsync-bridge-helper                          │     │
 │ │  conn → [compress+flush]    │    │  plain TCP, direct  │  │   -listen 0.0.0.0:<bridgePort0>              │     │
 │ │  → [netbuffer]              │    │  (default; add      │  │   -connect 127.0.0.1:<targetPort>            │     │
 │ │  → net.Dial() ──────────────┼────┼── -use-ssh to ──────┼──┤    -compress -level N -netbuffer <b>,<s>     │     │
 │ │                             │    │  tunnel this over   │  │  (single persistent process; each accepted   │     │
 │ │ inbound (from target):      │    │  SSH instead)        │  │   connection gets its own goroutine, dials   │     │
 │ │  Dial() → [netbuffer]       │    │                    │  │   <targetPort> itself, and relays both       │     │
 │ │  → [decompress] → conn      │    │                    │  │   directions -- no socat, no shell, no       │     │
 │ └─────────────────────────────┘    │                    │  │   per-connection process fork)               │     │
 │                                    │                    │  └────────────────────────────────────────────────┘  │
 │                                    │                    │           ▲                                          │
 │                                    │                    │           │ (backgrounded once, not per connection)  │
 │                                    │                    │  ┌────────┴────────────────────────────────────────┐ │
 │                                    │                    │  │ setsid sh -c 'vmsync-bridge-helper ...' &       │  │
 │                                    │                    │  │ pid  → /run/vmsync-bridge/vmsync-bridge-...pid  │  │
 │                                    │                    │  │ log  → /run/vmsync-bridge/vmsync-bridge-...log  │  │
 │                                    │                    │  └─────────────────────────────────────────────────┘  │
 └────────────────────────────────────┘                    └───────────────────────────────────────────────────────┘
 ┌── disk 1 ───────────────────────────┐                    ┌── disk 1 ────────────────────────────────────────────┐
 │ same shape, independent:            │                    │ same shape, independent:                             │
 │ localPort1 ≠ localPort0             │                    │ targetPort1  = TargetNBDPort + 1                     │
 │                                     │                    │ bridgePort1  = TargetNBDPort + N + 1                 │
 └─────────────────────────────────────┘                    └──────────────────────────────────────────────────────┘
                                                              ...one such block per disk, all torn down independently
 Source side (for contrast): only ONE shared bridge for the whole sync, since every disk
 reads through the same libvirt backup NBD export (differentiated by export name, not by
 port) — no per-disk repetition there. Only exists at all when the source itself is reached
 over SSH (-source-uri uses qemu+ssh://; see Topology above) -- when it exists, it listens
 on 0.0.0.0:<SourceNBDPort+1> (127.0.0.1:<SourceNBDPort+1> with -use-ssh).

Note: the SSH connection is *always* used to start/stop vmsync-bridge-helper and poll its
readiness (plain command execution -- the "control plane") regardless of -use-ssh --
-use-ssh only changes whether the bulk NBD data itself (the "data plane") also goes through
SSH, or directly.

vmsync-bridge-helper must already be deployed at -bridge-helper-path (default
/usr/local/bin/vmsync-bridge-helper) on any host --compress/--netbuffer will run
against -- vmsync does not upload it itself. No other tool (socat included) needs to be
installed on the remote host for bridging to work.

Since the helper is deployed manually, its version can drift from the vmsync binary
driving it. Before any sync work starts, vmsync runs `<helper-path> -version` over SSH
and refuses to proceed if it doesn't exactly match this vmsync's own version -- rebuild
and redeploy vmsync-bridge-helper any time you upgrade vmsync itself.

## Flags

- `-compress` -- compress bridged NBD traffic, native (`pkg/zstdrelay`), with an explicit
  `Flush()` after every chunk so nothing sits buffered indefinitely.
- `-compress-algo zstd|s2` (default `zstd`) -- compression format. `zstd` gives the better
  ratio; `s2` (Snappy-derived) trades ratio for substantially higher throughput -- the better
  fit once compression speed itself, not network bandwidth, is the bottleneck (e.g. a fast
  local/LAN link). Only meaningful with `-compress`.
- `-compress-level` -- meaning depends on `-compress-algo`: for `zstd`, a traditional numeric
  level `1`-`19` (default `3`); `s2` has no numeric levels at all, only three discrete modes --
  `default` (fastest, s2's own default), `better`, or `best` (slowest, closest to zstd's own
  ratio). If left unset, `s2` automatically uses `default` rather than zstd's `3`. Only
  meaningful with `-compress`.
- `-netbuffer <blocksize>,<buffersize>` (e.g. `64k,512M`) -- smooths throughput through a
  bounded in-memory buffer, native on both ends. Independent of `-compress` -- usable alone
  or combined with it.
- `-bridge-helper-path` -- remote path to vmsync-bridge-helper (default
  `/usr/local/bin/vmsync-bridge-helper`). Must already be deployed there by you (scp,
  config management, ...); vmsync never uploads it.
- `-use-ssh` -- route the bridged NBD traffic through the existing SSH connection as an
  encrypted tunnel, instead of the default: vmsync-bridge-helper listening on all interfaces
  and the local relay connecting to it directly over plain TCP. **The default (false) has no
  encryption or authentication of its own for that traffic** -- only appropriate when the
  network path between the hosts is already secured some other way (a VPN/WireGuard tunnel,
  a private/trusted network segment). When false (the default), the bridge port range must
  actually be reachable directly between the two hosts (firewall/routing) -- vmsync doesn't
  verify this itself, so a misconfigured firewall shows up as a plain connection failure the
  first time the local relay tries to dial the remote helper. No effect without
  `-compress`/`-netbuffer`. `-use-ssh` exists for links where the network path *isn't*
  already secured some other way, or where SSH's channel-level flow control isn't itself a
  bottleneck -- but neither mode is automatically the faster one: in one real test (an
  already-encrypted WireGuard-backed link), tunneled and direct measured at essentially the
  same speed, so measure before assuming either helps in your environment.

## Teardown

Both sides of vmsync-bridge-helper's per-connection relay explicitly half-close
(`CloseWrite`) the connection they're forwarding *into* once their read side hits EOF:
the local relay (`pkg/nbdbridge/local.go`) does this once its outbound direction drains
(over the SSH channel with `-use-ssh`, or the direct connection otherwise), and the helper
(`cmd/vmsync-bridge-helper`) does it on both the real NBD connection (inbound direction) and
the client-facing connection (outbound direction).

This matters because nothing else will ever signal "done" on these connections: they're
long-lived (an SSH direct-tcpip channel or a direct TCP connection either way, or a
persistent daemon's per-connection socket), not a pipe that closes for free when a one-shot
process exits. Without an explicit `CloseWrite`, the peer on the other end blocks forever
waiting for an EOF that can now never arrive -- this exact bug (on the local relay's side)
was root-caused via a `SIGQUIT` goroutine dump on a stuck sync: the decoder on the inbound
leg was blocked waiting on data that could never arrive, because the remote helper's own
relay goroutine, in turn, was waiting on an EOF nothing had sent it either.

## Testing / tuning

Each disk sync logs elapsed time and throughput, to make it easy to compare
`--compress`/`--netbuffer` settings against each other and against an uncompressed baseline:

- `nbd copy complete ... elapsed=... avg_mib_per_sec=...` -- the raw data-copy stage itself,
  the part these settings affect most directly.
- `disk sync complete disk=... elapsed=...` -- the whole per-disk sequence (export setup,
  copy, commit).
- `vmsync run finished elapsed=... success=...` -- total wall-clock for the whole
  invocation, logged on every exit path (success or failure).
