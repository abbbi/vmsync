#!/bin/sh
#
# dpkg prerm. $1 is a WORD, not a count -- the same distinction rpm makes with
# a number, spelled differently:
#
#   remove | purge   a real removal  -> stop and disable
#   upgrade | ...    an upgrade      -> leave the service alone
#
# Testing this the rpm way ([ "$1" = 0 ]) would never match, so the service
# would survive a genuine removal and leave a masked-looking unit behind.
set -e

case "$1" in
remove | purge)
	if command -v systemctl >/dev/null 2>&1; then
		systemctl --no-reload disable --now vmsync-agent.service >/dev/null 2>&1 || :
	fi
	;;
esac

exit 0
