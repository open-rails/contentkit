# Build from the repository root: docker build -f cmd/media-gateway/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26.9-trixie@sha256:f06b7122f888680048afb247c4e5f49c0069eb6c4c1ccae0f3f4834527a8d22c AS build
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
