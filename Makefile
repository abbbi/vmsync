all: rocky_93 rocky_92 rocky_91 rocky_10 rocky_101 debian_trixie debian_unstable

rocky_93:
	podman build --layers --target artifact --output . --build-arg ROCKY_VERSION=9.3 .

rocky_92:
	podman build --layers --target artifact --output . --build-arg ROCKY_VERSION=9.2 .

rocky_91:
	podman build --layers --target artifact --output . --build-arg ROCKY_VERSION=9.1 .

rocky_10:
	podman build --layers --target artifact --output . --build-arg ROCKY_VERSION=10.0 .

rocky_101:
	podman build --layers --target artifact --output . --build-arg ROCKY_VERSION=10.1 .

debian_trixie:
	podman build --layers --target artifact --output . --build-arg DEBIAN_VERSION=trixie . -f Dockerfile.debian

debian_unstable:
	podman build --layers --target artifact --output . --build-arg DEBIAN_VERSION=unstable . -f Dockerfile.debian

# Packages every vmsync_*/ directory the targets above produced: .rpm for the
# rocky ones, .deb for the debian ones, into dist/. Needs nfpm on PATH.
#
# Separate from the build targets rather than folded into them: the binaries
# have to be built inside a container matching the target distro (that is what
# makes them link against the right glibc), while packaging is a pure
# repackaging step that runs anywhere. Keeping them apart means `make
# debian_trixie packages` works, and so does packaging artifacts that were
# built somewhere else.
packages:
	contrib/packaging/build.sh

# What the nightly CI asserts, runnable by hand: that a nightly version sorts
# BELOW the release it precedes, according to dpkg and rpm themselves.
check-package-ordering:
	contrib/packaging/check-ordering.sh

clean:
	rm -f vmsync
	rm -rf vmsync_*
	rm -rf dist

.PHONY: all clean packages check-package-ordering \
	rocky_93 rocky_92 rocky_91 rocky_10 rocky_101 debian_trixie debian_unstable
