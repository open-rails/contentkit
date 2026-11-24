# Build from the repository root: docker build -f cmd/media-worker/Dockerfile .
# libvips is linked (CGO), so each platform builds natively (emulated for the other).
# Debian packages come from snapshot.debian.org at DEBIAN_SNAPSHOT, so a rebuild
# installs the same versions; move it forward to take Debian's fixes. CI's
# native jobs install from it too (debian-snapshot.sh).
ARG DEBIAN_SNAPSHOT=20261123T000000Z

FROM golang:1.26.11-trixie@sha256:cff05dd6b81c901f2e8249eb6f91c69a60451d0f12c26bfef08e476a85b32d4d AS build
ARG DEBIAN_SNAPSHOT
RUN --mount=type=bind,source=cmd/media-worker/debian-snapshot.sh,target=/debian-snapshot.sh \
    sh /debian-snapshot.sh libvips-dev
# The context carries no go.work; builds resolve each go.mod alone.
ENV GOWORK=off
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/media-worker ./cmd/media-worker

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
ARG DEBIAN_SNAPSHOT
# ffmpeg 7.1: libx264 and libsvtav1 (forced keyframes need ffmpeg ≥ 7 with SVT-AV1 ≥ 2).
# --upgrade brings the base image's own packages to the same snapshot.
RUN --mount=type=bind,source=cmd/media-worker/debian-snapshot.sh,target=/debian-snapshot.sh \
    sh /debian-snapshot.sh --upgrade ffmpeg libvips42t64 libheif-plugin-dav1d ca-certificates
COPY --from=build /out/media-worker /media-worker
USER 65532:65532
ENTRYPOINT ["/media-worker"]
