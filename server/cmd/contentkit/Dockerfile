# Build from the repository root: docker build -f server/cmd/contentkit/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26.11-trixie@sha256:cff05dd6b81c901f2e8249eb6f91c69a60451d0f12c26bfef08e476a85b32d4d AS build
ARG TARGETOS TARGETARCH
# The context carries no go.work; builds resolve each go.mod alone.
ENV GOWORK=off
WORKDIR /src
COPY go.mod go.sum ./
COPY adapters/openrails/go.mod adapters/openrails/go.sum adapters/openrails/
COPY adapters/authkit/go.mod adapters/authkit/go.sum adapters/authkit/
COPY server/go.mod server/go.sum server/
RUN cd server && go mod edit \
    -replace=github.com/open-rails/contentkit=../ \
    -replace=github.com/open-rails/contentkit/adapters/openrails=../adapters/openrails \
    -replace=github.com/open-rails/contentkit/adapters/authkit=../adapters/authkit \
    && go mod download
COPY . .
# Apply checkout replacements after COPY, which includes the replacement-free published go.mod;
# -mod=mod lets the checkout's requirements raise the published pins in this copy.
RUN cd server && go mod edit \
    -replace=github.com/open-rails/contentkit=../ \
    -replace=github.com/open-rails/contentkit/adapters/openrails=../adapters/openrails \
    -replace=github.com/open-rails/contentkit/adapters/authkit=../adapters/authkit \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=mod -trimpath -ldflags="-s -w" -o /out/contentkit ./cmd/contentkit

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/contentkit /contentkit
EXPOSE 8080
ENTRYPOINT ["/contentkit"]
