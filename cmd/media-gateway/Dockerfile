# Build from the repository root: docker build -f cmd/media-gateway/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26.10-trixie@sha256:909771fef53e3dcb0950255f0c9a8b4728c3b841e61509c7b29b098de7ee02d4 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/media-gateway ./cmd/media-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/media-gateway /media-gateway
EXPOSE 8080
ENTRYPOINT ["/media-gateway"]
