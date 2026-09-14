#!/usr/bin/env bash
#
# pkgversion.sh -- print the package version for the tree it is run in.
#
# Package versions are not the same string as the binary's own version, and
# conflating them is how a nightly ends up permanently outranking a release.
# `vmsync -v` prints pkg/version.Version verbatim ("0.50-2026091401-beta");
# this prints something dpkg and rpm can both order correctly.
#
# Usage:
#   pkgversion.sh              # nightly (the default)
#   pkgversion.sh release      # release, from pkg/version.Version as-is
#
set -euo pipefail

repo_root() {
	# The script's own location rather than $PWD, so it works from anywhere
	# and from a Makefile invoked in a subdirectory.
	cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd
}

ROOT="$(repo_root)"
VERSION_GO="${ROOT}/pkg/version/version.go"

[ -f "$VERSION_GO" ] || {
	echo "pkgversion.sh: cannot find ${VERSION_GO}" >&2
	exit 1
}

# The authoritative in-tree version, e.g. 0.50-2026091401-beta.
FULL="$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$VERSION_GO")"
[ -n "$FULL" ] || {
	echo "pkgversion.sh: could not parse a version out of ${VERSION_GO}" >&2
	exit 1
}

# The numeric part, up to the first dash: 0.50-2026091401-beta -> 0.50.
#
# Everything after it is a build stamp and a channel word, neither of which
# belongs in a package version: rpm forbids '-' in a version outright, and deb
# would read the last dash as the start of its own revision field. The full
# string is not lost -- it is what the binary prints, and it goes in the
# package description.
BASE="${FULL%%-*}"

case "${1:-nightly}" in
release)
	# A release is the base version and nothing else, so it sorts ABOVE every
	# nightly built on the way to it (see below).
	printf '%s\n' "$BASE"
	;;

nightly)
	# The COMMIT's date, not the wall clock. Same commit -> same package
	# version, which is what makes a re-run of the nightly (or a manual
	# dispatch of one) produce an identical package rather than a new version
	# of identical content. It is also monotonic, because commit dates are.
	STAMP="$(TZ=UTC git -C "$ROOT" show -s --format=%cd --date=format-local:%Y%m%d.%H%M HEAD)"
	SHA="$(git -C "$ROOT" rev-parse --short=8 HEAD)"

	# '~' is the whole trick, and it is the one character both packagers agree
	# on: in dpkg and in rpm >= 4.10 it sorts BEFORE everything, including the
	# empty string. So
	#
	#     0.50~nightly.20260915.0317.gabc12345   <   0.50
	#
	# and every nightly built while 0.50 is in development is superseded the
	# moment 0.50 is actually released. Without it -- with '+' or a bare
	# suffix -- the nightly sorts ABOVE 0.50, an upgrade prefers it forever,
	# and the release nobody can install is the one you just cut.
	printf '%s~nightly.%s.g%s\n' "$BASE" "$STAMP" "$SHA"
	;;

*)
	echo "pkgversion.sh: unknown channel '${1}' (want 'nightly' or 'release')" >&2
	exit 2
	;;
esac
