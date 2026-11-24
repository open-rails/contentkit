# Build from the repository root: docker build -f cmd/media-gateway/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26.11-trixie@sha256:cff05dd6b81c901f2e8249eb6f91c69a60451d0f12c26bfef08e476a85b32d4d AS build
ARG TARGETOS TARGETARCH
# The context carries no go.work; builds resolve each go.mod alone.
ENV GOWORK=off
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/media-gateway ./cmd/media-gateway

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/media-gateway /media-gateway
EXPOSE 8080
ENTRYPOINT ["/media-gateway"]
