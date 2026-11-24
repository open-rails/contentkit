#!/usr/bin/env bash
# Writes a modfile (and its go.sum) for server/ that builds against this
# checkout's root and adapters, and prints its path. The published server
# module pins released versions; CI qualifies the checkout before its paired
# tags exist, as Docker does.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
mod="${RUNNER_TEMP:-$(mktemp -d)}/contentkit-server.mod"
cp "$root/server/go.mod" "$mod"
cp "$root/server/go.sum" "${mod%.mod}.sum"
(
  cd "$root/server"
  go mod edit -modfile="$mod" \
    -replace=github.com/open-rails/contentkit="$root" \
    -replace=github.com/open-rails/contentkit/adapters/openrails="$root/adapters/openrails" \
    -replace=github.com/open-rails/contentkit/adapters/authkit="$root/adapters/authkit"
)
echo "$mod"
