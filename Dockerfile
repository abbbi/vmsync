ARG ROCKY_VERSION=9
ARG GO_VERSION=1.25.0

FROM golang:${GO_VERSION} AS go-toolchain

FROM rockylinux:${ROCKY_VERSION} AS build
ARG TARGETARCH=amd64

RUN dnf -y install dnf-plugins-core && \
    dnf config-manager --set-enabled crb && \
    dnf -y install \
      ca-certificates \
      gcc \
      git \
      libnbd-devel \
      libvirt-devel \
      make \
      pkgconf-pkg-config && \
      mkdir -p /tmp/go/src/ && \
    dnf clean all

COPY --from=go-toolchain /usr/local/go /usr/local/go
COPY --exclude=vmsync_* --exclude=.git --exclude=go.sum --exclude=go.mod . /tmp/go/src
WORKDIR /tmp/go/src/

ENV PATH="/usr/local/go/bin:${PATH}"
RUN GOOS=linux GOARCH=${TARGETARCH} go mod init vmsync && go mod tidy && \
    go build -o /out/vmsync ./cmd/vmsync/ && \
    CGO_ENABLED=0 go build -o /out/vmsync-bridge-helper ./cmd/vmsync-bridge-helper/

FROM build AS package
ARG ROCKY_VERSION
ARG TARGETARCH
RUN arch="${TARGETARCH}" && \
    if [ "${arch}" = "amd64" ]; then arch="x86_64"; fi && \
    out="/export/vmsync_rocky_${ROCKY_VERSION}_${arch}" && \
    mkdir -p "${out}" && \
    cp /out/vmsync "${out}/vmsync" && \
    cp /out/vmsync-bridge-helper "${out}/vmsync-bridge-helper"

FROM scratch AS artifact
COPY --from=package /export/ /
