#!/bin/sh
#
# Runs after the agent package is installed or upgraded, on both rpm and deb.
#
# It reloads systemd and does NOTHING ELSE. In particular it does not enable
# and does not start the service, which is deliberate and is the whole posture
# of this package:
#
#   * The agent has no default mode. Exactly one of --standalone, --monitor or
#     --controlled must be given, and the only one a package could plausibly
#     default to is --controlled -- the mode in which a network service can
#     stop this hypervisor's VMs. A package that enabled that on install would
#     be making, on its own, the decision the flags exist to force a human to
#     make.
#   * It has no configuration yet either. /etc/vmsync/agent.json is not shipped
#     (see the .example), and the agent refuses to start without it.
#
# So a freshly installed agent that does nothing until somebody configures it
# is the correct outcome, not an omission. `systemctl enable --now
# vmsync-agent` is the operator's call, after they have set VMSYNC_AGENT_MODE.
set -e

if command -v systemctl >/dev/null 2>&1; then
	# Fails inside a container or chroot with no running systemd, which is
	# exactly where the nightly workflow install-tests these packages. Not a
	# reason to fail the install.
	systemctl daemon-reload >/dev/null 2>&1 || :
fi

exit 0
