# Build from the repository root: docker build -f cmd/media-worker/Dockerfile .
# libvips is linked (CGO), so each platform builds natively (emulated for the other).
FROM golang:1.26.6-trixie AS build
RUN apt-get update && apt-get install -y --no-install-recommends libvips-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/media-worker ./cmd/media-worker

FROM debian:trixie-slim
# ffmpeg 7.1: libx264, libx265 and libsvtav1 (forced keyframes need ffmpeg ≥ 7 with SVT-AV1 ≥ 2).
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg libvips42t64 libheif-plugin-dav1d ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/media-worker /media-worker
USER 65532:65532
ENTRYPOINT ["/media-worker"]
