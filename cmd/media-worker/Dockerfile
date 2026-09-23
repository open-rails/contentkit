# Build from the repository root: docker build -f cmd/media-worker/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26.6 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/media-worker ./cmd/media-worker

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/media-worker /media-worker
USER 65532:65532
ENTRYPOINT ["/media-worker"]
