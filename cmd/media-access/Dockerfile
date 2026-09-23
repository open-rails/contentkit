# Build from the repository root: docker build -f cmd/media-access/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26.6 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/media-access ./cmd/media-access

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/media-access /media-access
EXPOSE 8080
ENTRYPOINT ["/media-access"]
