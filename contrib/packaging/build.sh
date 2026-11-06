#!/usr/bin/env bash
#
# build.sh -- turn a built artifact directory into .deb or .rpm packages.
#
# The binaries are built inside a container matching the target distro (see the
# Dockerfiles and the Makefile), because that is what makes them link against
# that distro's own glibc. Packaging does NOT need to happen there: nfpm is a
# static Go binary that writes the archive formats directly, so it runs on the
# CI runner over whatever the containers produced.
#
# Usage:
#   build.sh                         package every vmsync_*/ dir in the repo root
#   build.sh vmsync_rocky_9.3_x86_64 package just that one
#
# Environment:
#   PKG_VERSION   override the computed version (default: pkgversion.sh nightly)
#   PKG_CHANNEL   nightly | release   (default: nightly)
#   OUT_DIR       where packages land (default: <repo>/dist)
#   NFPM          path to nfpm (default: nfpm on PATH)
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INVOCATION_DIR="$PWD"
NFPM="${NFPM:-nfpm}"
OUT_DIR="${OUT_DIR:-${ROOT}/dist}"
PKG_CHANNEL="${PKG_CHANNEL:-nightly}"

# nfpm resolves `scripts:` relative to CWD, not to the config file, while
# vmsync-agent.yaml deliberately uses repo-relative script paths -- nfpm never
# expands ${VAR} in the scripts fields (see nfpm.go expandEnvVars: Scripts is
# simply absent there), so an absolute path cannot be expressed with a
# variable. Run from the repo root so those resolve no matter where build.sh
# was invoked from. OUT_DIR is made absolute first so a relative OUT_DIR still
# lands where the caller meant.
case "$OUT_DIR" in
/*) ;;
*) OUT_DIR="${INVOCATION_DIR}/${OUT_DIR}" ;;
esac
cd "$ROOT"

command -v "$NFPM" >/dev/null 2>&1 || {
	echo "build.sh: nfpm not found. Install it (https://github.com/goreleaser/nfpm) or set NFPM=" >&2
	exit 1
}

PKG_VERSION="${PKG_VERSION:-$("${ROOT}/contrib/packaging/pkgversion.sh" "$PKG_CHANNEL")}"
VMSYNC_FULL_VERSION="$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "${ROOT}/pkg/version/version.go")"

# Package mtimes come from the commit, not from the clock. Same commit in ->
# same bytes out, which is the property the build containers already go to some
# trouble to preserve (see the go.mod note in the Dockerfiles) and which a
# packaging step stamping "now" into every file would throw away.
SOURCE_DATE_EPOCH="$(git -C "$ROOT" show -s --format=%ct HEAD 2>/dev/null || echo 0)"
export SOURCE_DATE_EPOCH

# family_of DIR -> rpm | deb
#
# From the directory name the Dockerfiles produce: vmsync_rocky_9.3_x86_64 or
# vmsync_debian_trixie_x86_64. Refuses anything else rather than guessing --
# building a .deb for a Rocky binary would install cleanly and then fail at the
# first dynamic link, which is a much worse way to find out.
family_of() {
	case "$(basename "$1")" in
	vmsync_rocky_*) echo rpm ;;
	vmsync_debian_*) echo deb ;;
	*)
		echo "build.sh: cannot tell which packager '$1' is for (expected vmsync_rocky_* or vmsync_debian_*)" >&2
		return 1
		;;
	esac
}

# arch_of DIR -> the nfpm arch name.
#
# The Dockerfiles name their output directories with the RPM spelling
# (x86_64); nfpm wants Go's (amd64) and maps it back per packager itself.
arch_of() {
	case "$(basename "$1")" in
	*_x86_64) echo amd64 ;;
	*_aarch64 | *_arm64) echo arm64 ;;
	*)
		echo "build.sh: cannot tell the architecture of '$1'" >&2
		return 1
		;;
	esac
}

package_one() {
	local bin_dir="$1" family arch
	family="$(family_of "$bin_dir")"
	arch="$(arch_of "$bin_dir")"

	for b in vmsync vmsync-agent vmsync-bridge-helper; do
		[ -f "${bin_dir}/${b}" ] || {
			echo "build.sh: ${bin_dir} has no ${b} -- was 'make' run for this target?" >&2
			return 1
		}
	done

	# The unit and the example config ship as they are checked in, because
	# every path in them is already the path this package installs to:
	# /usr/bin is both the engine's default (cmd/vmsync/main.go's
	# -bridge-helper-path, cmd/vmsync-agent/agentfile.go's vmsync_path) and
	# this package's destination, so there is nothing left to substitute.
	#
	# Asserted rather than assumed, in both directions. A unit whose ExecStart
	# drifted away from /usr/bin would install a service that cannot start, and
	# an example config that names a path explicitly would pin one where the
	# default already answers -- and would then have to be moved in lockstep
	# with both this package's dst and the engine's defaults.
	#
	# The alternation accepts the binary with arguments or alone, matching what
	# cmd/vmsync-agent/installpath_test.go accepts: two guards for one property
	# that disagree about the property send whoever hits the stricter one
	# looking for a mismatch the laxer one says is not there. It still refuses
	# ExecStart=/usr/bin/vmsync-agent-wrapper, since neither a space nor the end
	# of the line follows the name.
	grep -qE '^ExecStart=/usr/bin/vmsync-agent( |$)' "${ROOT}/contrib/systemd/vmsync-agent.service" || {
		echo "build.sh: contrib/systemd/vmsync-agent.service does not exec /usr/bin/vmsync-agent, which is where this package installs it" >&2
		return 1
	}
	if grep -qE '"(vmsync_path|bridge_helper_path)"' "${ROOT}/contrib/agent/agent-controlled.json.example"; then
		echo "build.sh: contrib/agent/agent-controlled.json.example now names vmsync_path or bridge_helper_path. Either the binaries moved -- in which case this package's dst and the engine's defaults have to move with them -- or the key is redundant and should come out. cmd/vmsync-agent/installpath_test.go asserts the same thing on every go test." >&2
		return 1
	fi
	# Validity of the file as shipped, as a build-time backstop: the same file
	# is parsed by cmd/vmsync-agent/installpath_test.go, which does not depend
	# on python3 being on the packaging host. A /etc/vmsync/agent.json.example
	# that does not parse is one an operator copies and then has to debug.
	if command -v python3 >/dev/null 2>&1; then
		python3 -c 'import json,sys; json.load(open(sys.argv[1]))' \
			"${ROOT}/contrib/agent/agent-controlled.json.example" || {
			echo "build.sh: contrib/agent/agent-controlled.json.example is not valid JSON" >&2
			return 1
		}
	fi

	local sysconfig_dst
	if [ "$family" = rpm ]; then
		sysconfig_dst=/etc/sysconfig/vmsync-agent
	else
		sysconfig_dst=/etc/default/vmsync-agent
	fi

	mkdir -p "$OUT_DIR"
	echo "--> $(basename "$bin_dir"): building ${family} packages, version ${PKG_VERSION}"

	local cfg
	for cfg in vmsync-bridge-helper vmsync vmsync-agent; do
		ROOT="$ROOT" \
			BIN_DIR="$bin_dir" \
			PKG_ARCH="$arch" \
			PKG_VERSION="$PKG_VERSION" \
			SYSCONFIG_DST="$sysconfig_dst" \
			VMSYNC_FULL_VERSION="$VMSYNC_FULL_VERSION" \
			"$NFPM" package \
			--config "${ROOT}/contrib/packaging/nfpm/${cfg}.yaml" \
			--packager "$family" \
			--target "$OUT_DIR"
	done
}

main() {
	local -a dirs=()
	if [ "$#" -gt 0 ]; then
		# Resolved against the caller's directory, not $ROOT: the script
		# cd's to $ROOT above, so a relative argument must be anchored
		# before that.
		local a
		for a in "$@"; do
			case "$a" in
			/*) dirs+=("${a%/}") ;;
			*) dirs+=("${INVOCATION_DIR}/${a%/}") ;;
			esac
		done
	else
		# A nullglob-free way to notice there is nothing to do, so the script
		# says so instead of running nfpm against a literal "vmsync_*".
		local d
		for d in "${ROOT}"/vmsync_*/; do
			[ -d "$d" ] || continue
			dirs+=("${d%/}")
		done
	fi

	[ "${#dirs[@]}" -gt 0 ] || {
		echo "build.sh: no vmsync_*/ artifact directories found in ${ROOT}. Run 'make rocky_93' (or another target) first." >&2
		exit 1
	}

	local d
	for d in "${dirs[@]}"; do
		package_one "$d"
	done

	echo
	echo "Packages in ${OUT_DIR}:"
	ls -1 "$OUT_DIR"
}

main "$@"
