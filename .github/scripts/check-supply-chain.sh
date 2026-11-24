#!/usr/bin/env bash
# Supply-chain guards: every action is pinned to a commit.
set -euo pipefail
cd "$(dirname "$0")/../.."
status=0
refuse() { printf '%s\n' "$*" >&2; status=1; }

# A non-local uses: names a full commit SHA, with the release it is as a comment.
workflows=(.github/workflows/*.y*ml)
shopt -s nullglob
workflows+=(.github/actions/*/action.y*ml)
shopt -u nullglob
while IFS=: read -r file line text; do
  ref=$(sed -E 's/^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*//; s/[[:space:]]+#.*$//; s/^["'\'']|["'\'']$//g' <<<"$text")
  case $ref in
    ./*) ;;
    docker://*) [[ $ref == *@sha256:* ]] || refuse "$file:$line: $ref is not pinned by digest" ;;
    *) [[ $ref =~ ^[^@[:space:]]+@[0-9a-f]{40}$ && $text =~ \#[[:space:]]*[^[:space:]] ]] ||
         refuse "$file:$line: $ref is not pinned to a commit SHA with a version comment" ;;
  esac
done < <(grep -nH -E '^[[:space:]]*(-[[:space:]]+)?uses:' "${workflows[@]}")

exit "$status"
