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

clean:
	rm -f vmsync
	rm -rf vmsync_*
