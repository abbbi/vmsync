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
NFPM="${NFPM:-nfpm}"
OUT_DIR="${OUT_DIR:-${ROOT}/dist}"
PKG_CHANNEL="${PKG_CHANNEL:-nightly}"

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
	local bin_dir="$1" family arch stage
	family="$(family_of "$bin_dir")"
	arch="$(arch_of "$bin_dir")"

	for b in vmsync vmsync-agent vmsync-bridge-helper; do
		[ -f "${bin_dir}/${b}" ] || {
			echo "build.sh: ${bin_dir} has no ${b} -- was 'make' run for this target?" >&2
			return 1
		}
	done

	stage="$(mktemp -d)"
	# shellcheck disable=SC2064  # $stage must expand now, not at trap time
	trap "rm -rf '$stage'" RETURN

	# The unit, with the package's own paths.
	#
	# contrib/systemd/vmsync-agent.service names /usr/local/bin because that is
	# where the hand-install documented in the READMEs puts the binaries. This
	# package owns /usr/bin instead -- /usr/local is reserved for things the
	# local admin installed, and a package writing there would silently
	# overwrite a hand-built binary somebody is relying on. Substituting here
	# keeps one unit file as the source of truth for both.
	sed 's#/usr/local/bin/#/usr/bin/#g' \
		"${ROOT}/contrib/systemd/vmsync-agent.service" >"${stage}/vmsync-agent.service"
	grep -q '/usr/bin/vmsync-agent' "${stage}/vmsync-agent.service" || {
		echo "build.sh: the unit substitution produced no /usr/bin ExecStart -- has the unit moved?" >&2
		return 1
	}

	# The example config, with the same paths. Written here rather than kept as
	# a checked-in file for exactly one reason: if it were checked in, it would
	# name /usr/local/bin like everything else aimed at hand-installers, and
	# the one config a packaged agent copies would be the one pointing at
	# binaries the package did not install.
	cat >"${stage}/agent.json.example" <<'JSON'
{
  "config_version": 1,

  "vmsync_path": "/usr/bin/vmsync",
  "bridge_helper_path": "/usr/bin/vmsync-bridge-helper",

  "ssh": { "user": "root", "key": "/etc/vmsync/id_ed25519" },

  "control_plane": {
    "url": "https://vmsync-ui.dr.example.org",
    "ca_file": "/etc/vmsync/ui-ca.pem"
  }
}
JSON

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
			STAGE_DIR="$stage" \
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
		dirs=("$@")
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
