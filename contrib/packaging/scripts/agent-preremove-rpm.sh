#!/bin/sh
#
# rpm preremove. $1 is the number of instances of this package that will REMAIN
# once this transaction finishes:
#
#   $1 == 0  this is a real removal    -> stop and disable
#   $1 == 1  this is an upgrade        -> leave the service alone
#
# Getting this backwards is the classic packaging bug: every upgrade stops the
# agent, the new one never starts, and replication quietly stops on a host that
# looks like it was merely updated.
set -e

if [ "$1" = 0 ] && command -v systemctl >/dev/null 2>&1; then
	systemctl --no-reload disable --now vmsync-agent.service >/dev/null 2>&1 || :
fi

exit 0
