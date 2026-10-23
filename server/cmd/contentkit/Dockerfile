# Build from the repository root: docker build -f server/cmd/contentkit/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26.10 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY adapters/openrails/go.mod adapters/openrails/go.sum adapters/openrails/
COPY adapters/authkit/go.mod adapters/authkit/go.sum adapters/authkit/
COPY server/go.mod server/go.sum server/
# Repository images use this checkout; the published Go module uses tagged dependencies.
RUN cd server && go mod edit \
    -replace=github.com/open-rails/contentkit=../ \
    -replace=github.com/open-rails/contentkit/adapters/openrails=../adapters/openrails \
    -replace=github.com/open-rails/contentkit/adapters/authkit=../adapters/authkit \
    && go mod download
COPY . .
RUN cd server && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/contentkit ./cmd/contentkit

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/contentkit /contentkit
EXPOSE 8080
ENTRYPOINT ["/contentkit"]
