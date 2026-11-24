# Build from the repository root: docker build -f cmd/media-worker/Dockerfile .
# libvips is linked (CGO), so each platform builds natively (emulated for the other).
# Debian packages come from snapshot.debian.org at DEBIAN_SNAPSHOT, so a rebuild
# installs the same versions; move it forward to take Debian's fixes.
ARG DEBIAN_SNAPSHOT=20261123T000000Z

FROM golang:1.26.11-trixie@sha256:cff05dd6b81c901f2e8249eb6f91c69a60451d0f12c26bfef08e476a85b32d4d AS build
ARG DEBIAN_SNAPSHOT
RUN sed -i -E "s#^URIs: http://deb\.debian\.org/(debian|debian-security)\$#URIs: http://snapshot.debian.org/archive/\1/${DEBIAN_SNAPSHOT}#" /etc/apt/sources.list.d/debian.sources \
    && ! grep -q deb.debian.org /etc/apt/sources.list.d/debian.sources \
    && apt-get -o Acquire::Check-Valid-Until=false -o Acquire::Retries=5 update \
    && apt-get install -y --no-install-recommends libvips-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/media-worker ./cmd/media-worker

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
ARG DEBIAN_SNAPSHOT
# ffmpeg 7.1: libx264 and libsvtav1 (forced keyframes need ffmpeg ≥ 7 with SVT-AV1 ≥ 2).
# upgrade brings the base image's own packages to the same snapshot.
RUN sed -i -E "s#^URIs: http://deb\.debian\.org/(debian|debian-security)\$#URIs: http://snapshot.debian.org/archive/\1/${DEBIAN_SNAPSHOT}#" /etc/apt/sources.list.d/debian.sources \
    && ! grep -q deb.debian.org /etc/apt/sources.list.d/debian.sources \
    && apt-get -o Acquire::Check-Valid-Until=false -o Acquire::Retries=5 update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends ffmpeg libvips42t64 libheif-plugin-dav1d ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/media-worker /media-worker
USER 65532:65532
ENTRYPOINT ["/media-worker"]
