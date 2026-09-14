#!/usr/bin/env bash
#
# check-ordering.sh -- prove a nightly version sorts BELOW the release it
# precedes, according to dpkg and rpm themselves.
#
# This is the assertion the whole nightly version scheme rests on, and its
# failure mode is both silent and very annoying: if a nightly sorts ABOVE the
# release, every host that ever installed one refuses the actual release as a
# downgrade, and nobody notices until release day.
#
# Usage:
#   check-ordering.sh                       # compare today's nightly to the release
#   check-ordering.sh NIGHTLY RELEASE       # compare two explicit versions
#
# Needs dpkg for the deb half. The rpm half runs in a container, so it needs
# podman; without one it is skipped rather than silently passing.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPM_IMAGE="${RPM_IMAGE:-rockylinux:9.3}"

NIGHTLY="${1:-$("${ROOT}/contrib/packaging/pkgversion.sh" nightly)}"
RELEASE="${2:-$("${ROOT}/contrib/packaging/pkgversion.sh" release)}"

echo "nightly = ${NIGHTLY}"
echo "release = ${RELEASE}"

fail() {
	echo "check-ordering.sh: $*" >&2
	exit 1
}

# How many of the two halves actually ran. A script that skipped both and then
# printed "ordering OK" would be the exact shape of failure it exists to catch:
# a check that verified nothing, reading as a pass.
checked=0

# --- deb -------------------------------------------------------------------
#
# Unambiguous: 'lt' is either true or it is not.
if command -v dpkg >/dev/null 2>&1; then
	if dpkg --compare-versions "$NIGHTLY" lt "$RELEASE"; then
		echo "dpkg: OK  (${NIGHTLY} < ${RELEASE})"
		checked=$((checked + 1))
	else
		fail "dpkg says ${NIGHTLY} is NOT older than ${RELEASE}"
	fi
else
	echo "dpkg: SKIP (not installed)"
fi

# --- rpm -------------------------------------------------------------------
#
# Asserted by CALIBRATION rather than by hardcoding an exit code.
#
# rpmdev-vercmp reports its answer through its exit status, and which number
# means "the first one is older" is exactly the kind of detail that gets
# remembered backwards. An inverted hardcoded check would pass cheerfully while
# the ordering was wrong -- which is the one outcome this script exists to
# prevent, so it must not depend on getting that constant right.
#
# Instead: ask about a pair whose answer nobody can dispute (1.0 vs 2.0), learn
# from that which code means "older", and require the real comparison to return
# the same one. If a future rpmdevtools renumbers its exit codes, this keeps
# working; if it stops distinguishing the three cases at all, the sanity check
# below catches that too.
rpm_probe() {
	cat <<'SH'
set -u
dnf -y install rpmdevtools >/dev/null 2>&1 || exit 90

rpmdev-vercmp 1.0 2.0 >/dev/null 2>&1; older=$?
rpmdev-vercmp 2.0 1.0 >/dev/null 2>&1; newer=$?
rpmdev-vercmp 1.0 1.0 >/dev/null 2>&1; same=$?

# Three distinct answers, or the tool is not telling us what we think it is.
if [ "$older" = "$newer" ] || [ "$older" = "$same" ] || [ "$newer" = "$same" ]; then
	exit 91
fi

rpmdev-vercmp "$1" "$2" >/dev/null 2>&1; got=$?
echo "  rpmdev-vercmp codes: older=$older newer=$newer same=$same, got=$got"
[ "$got" = "$older" ]
SH
}

if command -v podman >/dev/null 2>&1; then
	out="$(rpm_probe | podman run --rm -i "$RPM_IMAGE" sh -s "$NIGHTLY" "$RELEASE" 2>&1)"
	rc=$?
	[ -n "$out" ] && echo "$out"
	case "$rc" in
	0)
		echo "rpm:  OK  (${NIGHTLY} < ${RELEASE})"
		checked=$((checked + 1))
		;;
	90) fail "could not install rpmdevtools in ${RPM_IMAGE}" ;;
	91) fail "rpmdev-vercmp no longer distinguishes older/newer/equal by exit code" ;;
	*) fail "rpm says ${NIGHTLY} does NOT sort below ${RELEASE} -- every host that installed a nightly would refuse the release as a downgrade" ;;
	esac
else
	echo "rpm:  SKIP (podman not installed)"
fi

if [ "$checked" -eq 0 ]; then
	fail "neither dpkg nor podman was available, so NOTHING was verified. This script must not report success without having compared anything -- install dpkg, or podman for the rpm half."
fi

echo "ordering OK (${checked}/2 packagers checked)"
