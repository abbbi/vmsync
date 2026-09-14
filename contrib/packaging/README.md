# Packaging

`.deb` and `.rpm` for vmsync, built nightly from `main` by
[`.github/workflows/nightly.yml`](../../.github/workflows/nightly.yml) and
buildable by hand with `make packages`.

## Three packages, not one

| package | contents | depends on | install it on |
| --- | --- | --- | --- |
| `vmsync` | the engine, docs, the parallel launcher as an example | libvirt, libnbd | every **source** hypervisor |
| `vmsync-bridge-helper` | one static binary, nothing else | *nothing* | every **target** hypervisor |
| `vmsync-agent` | the agent, its systemd unit, its sysconfig | `vmsync` | hosts you want scheduled or monitored |

The split is not tidiness. `vmsync-bridge-helper` is built with
`CGO_ENABLED=0`, so it needs neither libvirt nor libnbd — a host that only
*receives* replicas can install it alone. That host had no reason to carry the
engine, and until this existed the helper was deployed by hand to every target
with its version required to match the vmsync driving it (`pkg/nbdbridge`'s
`CheckRemote` refuses a sync otherwise). "By hand, everywhere, and it must
match" is the problem package managers exist to solve.

`vmsync` *suggests* rather than *depends on* the helper: it is needed at the
far end of a sync, on a different machine, so a hard dependency would install
it where it does nothing and still not put it where it is required.

## `/usr/bin`, not `/usr/local/bin`

The packages install to `/usr/bin`. The READMEs describe a hand-install to
`/usr/local/bin`, and both are right: `/usr/local` is reserved for what the
local administrator installed, so a package writing there would silently
overwrite a hand-built binary somebody is relying on.

Two consequences, both handled at build time by
[`build.sh`](build.sh):

- The packaged systemd unit is `contrib/systemd/vmsync-agent.service` with
  `/usr/local/bin/` → `/usr/bin/` substituted, so one unit file stays the
  source of truth for both install styles.
- `/etc/vmsync/agent.json.example` names `/usr/bin/vmsync` and
  `/usr/bin/vmsync-bridge-helper`, because the agent's compiled defaults point
  at `/usr/local/bin` and a packaged agent would otherwise try to exec a binary
  its own package did not install.

## What installing the agent does, and deliberately does not

It installs the unit and runs `systemctl daemon-reload`. **It does not enable
or start anything.**

That is the posture, not an omission. The agent has no default mode — exactly
one of `--standalone`, `--monitor` or `--controlled` must be given — and the
only mode a package could plausibly default to is `--controlled`, which is the
one where a network service can stop this hypervisor's VMs. A package that
enabled that on install would be making, by itself, the decision the flags
exist to force a human to make. It also has no configuration: `agent.json` is
not shipped, only an example, and the agent refuses to start without it.

So:

```bash
dnf install ./vmsync-agent-*.rpm          # or apt install ./vmsync-agent_*.deb
$EDITOR /etc/sysconfig/vmsync-agent       # /etc/default/ on Debian — set VMSYNC_AGENT_MODE
cp /etc/vmsync/agent.json.example /etc/vmsync/agent.json
$EDITOR /etc/vmsync/agent.json
systemctl enable --now vmsync-agent
```

The sysconfig is a conffile (`%config(noreplace)` on rpm, a dpkg conffile on
deb), so an upgrade never silently changes what a host is.

Removal stops and disables the service; **upgrade does not**, which is the one
thing the scriptlets in [`scripts/`](scripts/) exist to get right. The two
packagers signal "real removal" completely differently — rpm passes a count,
dpkg passes a word — so there is one preremove per packager rather than one
clever script that tries to read both. Getting it backwards is the classic
packaging bug: every upgrade stops the agent, the new one never starts, and
replication quietly stops on a host that looks like it was merely updated.

Nothing removes `/var/lib/vmsync-agent`. It holds the enrolment credential, the
operation ledger and the **fence ledger** — and that last one is what makes a
fence single-use, so deleting it on package removal would let a reinstalled
agent perform a second unattended shutdown of a VM it had already fenced.

## Versions

`pkgversion.sh` prints the package version, which is **not** the string
`vmsync -v` prints:

| | |
| --- | --- |
| binary (`pkg/version.Version`) | `0.50-2026091401-beta` |
| release package | `0.50` |
| nightly package | `0.50~nightly.20260914.2224.geb03e205` |

rpm forbids `-` in a version outright and dpkg would read the last one as the
start of its own revision field, so the package version is the numeric part
plus a suffix both packagers can order. The full string is not lost — it is
what the binary prints, and it is in each package's description.

The `~` is the whole point. In dpkg and in rpm ≥ 4.10 it sorts *before*
everything, including the empty string, so every nightly built on the way to
`0.50` is superseded the moment `0.50` is released. With `+`, or with a bare
suffix, the nightly would sort *above* the release, an upgrade would prefer it
forever, and the release nobody can install is the one you just cut.

The timestamp comes from the **commit**, not the clock, so re-running the
nightly on an unchanged tree produces an identical package rather than a new
version of identical content. `SOURCE_DATE_EPOCH` is set from the same commit,
so file mtimes inside the package are stable too.

[`check-ordering.sh`](check-ordering.sh) asserts the ordering using dpkg and
rpm themselves. It works out which `rpmdev-vercmp` exit code means "older" by
first asking it about `1.0` vs `2.0`, rather than hardcoding a constant that is
easy to remember backwards — an inverted check would pass cheerfully while the
ordering was wrong, which is the one outcome it exists to prevent. If neither
packager is available it **fails** rather than reporting success.

## What CI actually proves

Building a package proves very little, so the nightly workflow installs each
one into a clean container of the distro it was built for and runs
`vmsync -v`, `vmsync-agent -v` and `vmsync-bridge-helper -v`.

That is the assertion that matters. nfpm does not derive dependencies from a
binary's `DT_NEEDED` the way `rpmbuild` does — the runtime library names in
[`nfpm/`](nfpm/) are asserted by hand — so the install is what resolves them
against the real repositories, and running the binaries is what proves the
dynamic linker is genuinely satisfied, which a successful install alone does
not. It also checks that `agent.json` is *not* present, that the example and
the sysconfig are, and that the unit is not enabled.

## Building by hand

```bash
make debian_trixie          # build binaries in a matching container
make packages               # package every vmsync_*/ into dist/
```

Needs [nfpm](https://github.com/goreleaser/nfpm) on `PATH`, pinned in CI so a
change in how it lays out an archive can never arrive unattributed to a commit.
`contrib/packaging/build.sh vmsync_rocky_9.3_x86_64` packages one directory
instead of all of them.
