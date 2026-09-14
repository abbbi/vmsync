#!/bin/sh
#
# Runs after removal or after the teardown half of an upgrade, on both
# packagers. Reloading is right in both cases -- either the unit file is gone,
# or it has just been replaced -- so unlike preremove this needs no argument
# inspection.
#
# What it deliberately does not touch: /var/lib/vmsync-agent. It holds the
# enrolment credential, the operation ledger and the FENCE ledger, and that
# last one is what makes a fence single-use. Deleting it on package removal
# would mean a reinstalled agent could perform a second unattended shutdown of
# a production VM it had already fenced. Removing that directory is a
# deliberate act, not a side effect of `dnf remove`.
set -e

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || :
fi

exit 0
