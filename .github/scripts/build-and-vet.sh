#!/usr/bin/env bash
# Builds and vets every public module and the native media worker; no services.
set -euo pipefail

# The modules (the last is the `pnpm demo` harness) are independent of
# each other, so they build and vet concurrently instead of one after
# another. Each still runs its own compile with -p 1, left alone --
# untangling that pairs with the merge-queue work, not this change.
declare -A module_pids
mkdir -p "$RUNNER_TEMP/module-build-logs"
for module in . adapters/authkit adapters/openrails sdk/ui/e2e/server; do
  log="$RUNNER_TEMP/module-build-logs/$(echo "$module" | tr '/.' '__').log"
  (cd "$module" && go build -p 1 ./... && go vet -p 1 ./...) > "$log" 2>&1 &
  module_pids["$module"]=$!
done
status=0
for module in "${!module_pids[@]}"; do
  log="$RUNNER_TEMP/module-build-logs/$(echo "$module" | tr '/.' '__').log"
  if wait "${module_pids[$module]}"; then
    echo "::group::build/vet $module (ok)"
  else
    echo "::group::build/vet $module (FAILED)"
    status=1
  fi
  cat "$log"
  echo "::endgroup::"
done
[[ $status -eq 0 ]] || exit 1
CGO_ENABLED=1 go build -p 1 -o tests/critical/.bin/media-worker ./cmd/media-worker
# Hosts, server/ and adapters/* reach only the public surface; the image
# encoder's geometry, bounds and metadata stripping run against libvips.
go test -count=1 -p 1 ./internal/boundaries ./internal/media/image
# The published server module has no local replacements. Qualify this
# checkout before its paired root/adapter tags exist, just as Docker does.
server_mod="$RUNNER_TEMP/contentkit-server.mod"
cp server/go.mod "$server_mod"
cp server/go.sum "${server_mod%.mod}.sum"
(
  cd server
  go mod edit -modfile="$server_mod" \
    -replace=github.com/open-rails/contentkit="$GITHUB_WORKSPACE" \
    -replace=github.com/open-rails/contentkit/adapters/openrails="$GITHUB_WORKSPACE/adapters/openrails" \
    -replace=github.com/open-rails/contentkit/adapters/authkit="$GITHUB_WORKSPACE/adapters/authkit"
  go build -mod=readonly -modfile="$server_mod" -p 1 ./...
  go vet -mod=readonly -modfile="$server_mod" -p 1 ./...
)
test -z "$(gofmt -l tests/critical)"
(cd tests/critical && go mod tidy -diff && go vet -p 1 ./...)
