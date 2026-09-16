# Example agent configuration

Copy one of these to `/etc/vmsync/agent.json`, edit the handful of values that
are site-specific, and start the service. They are deliberately minimal: every
key not present has a default, and the defaults are the right answer on most
hosts.

There are three files because there are three modes, and each needs a
*different shape* of file — not just a different flag. The agent refuses to
start if the mode and the file disagree, by name, so a mismatch is caught at
startup rather than becoming a host that runs and replicates nothing.

| file | mode | why it differs |
| --- | --- | --- |
| [`agent-controlled.json.example`](agent-controlled.json.example) | `--controlled` | Needs `control_plane` (where to enrol) **and** `ssh` (it runs syncs). |
| [`agent-monitor.json.example`](agent-monitor.json.example) | `--monitor` | Needs only `control_plane`. **No `ssh` block at all** — a monitor agent never executes vmsync, so it needs no credentials for the target hypervisors. That is worth keeping: it means a monitored host holds no SSH key on vmsync's account. |
| [`agent-standalone.json.example`](agent-standalone.json.example) | `--standalone` | Needs `schedule_file` and `ssh`. Must **not** have `control_plane`; the two are mutually exclusive. |
| [`schedule.json.example`](schedule.json.example) | `--standalone` | The schedule itself, referenced by `schedule_file` above. Not used in the other two modes — there the control plane sends it. |

These files have **no comments**, because JSON has none and the agent decodes
with `DisallowUnknownFields`: an unrecognised key is a startup error, not a
line it ignores. So anything you add here must be a real key. Every key, its
default and what it does is in
[the `agent.json` reference](../../cmd/vmsync-agent/README.md#agentjson-reference).

## Deploying

```bash
mkdir -p /etc/vmsync
install -m 0640 contrib/agent/agent-controlled.json.example /etc/vmsync/agent.json
$EDITOR /etc/vmsync/agent.json
```

Then set the mode, which lives on the command line rather than in this file —
see [`contrib/systemd/`](../systemd/):

```bash
$EDITOR /etc/sysconfig/vmsync-agent     # /etc/default/ on Debian
# VMSYNC_AGENT_MODE=--controlled
```

For `--controlled` and `--monitor`, enrol once with a token from the console
before enabling the service. For `--standalone`, also install the schedule:

```bash
install -m 0640 contrib/agent/schedule.json.example /etc/vmsync/schedule.json
```

Own `/etc/vmsync` root and keep it out of group- and world-write. The agent
warns on startup and on every accepted reload if `agent.json` is writable by
anyone else, because that file decides which binary this agent runs as root.

## What to change

Everything below is site-specific; nothing else in the examples usually is.

- **`ssh.key`** — a key that reaches the target hypervisors as `ssh.user`.
  vmsync dials SSH to the target itself; there is no agent socket under
  systemd to fall back on.
- **`control_plane.url`** — must be `https://`. `control_plane.ca_file` is only
  needed when the console uses a private CA; drop the line for a publicly
  trusted certificate.
- **`schedule_file` entries** — `vm` is a domain on *this* host. Its targets
  come from that domain's own libvirt metadata, never from the file;
  `target_host` is needed only when a VM replicates to more than one.
- **`limits.max_concurrent_syncs`** — how much concurrent I/O this machine can
  absorb. It can only *lower* what the schedule asks for, never raise it.

## Two things that are not in the examples on purpose

**`vmsync_path` and `bridge_helper_path`.** The defaults (`/usr/local/bin/...`)
match the hand-install the READMEs describe. The `.rpm` and `.deb` install to
`/usr/bin` instead, so the packaged `/etc/vmsync/agent.json.example` sets both
explicitly — see [`contrib/packaging/`](../packaging/). If you installed from a
package and are copying a file from *this* directory, set them.

**`target_runtime_dir`.** Leave it unset. The default (`/run/vmsync`) is
correct almost everywhere, and the one thing you must not do is point it at a
directory the *target* polyinstantiates per SSH session — SELinux with
`pam_namespace` does that to `/tmp` and `/var/tmp`, and an export started by
one command is then invisible to the next. See
[the HOWTO](../../docs/HOWTO.md#where-the-targets-exports-keep-their-sockets-and-pidfiles).
