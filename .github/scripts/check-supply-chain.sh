#!/usr/bin/env bash
# Supply-chain guards: every action is pinned to a commit and every image by digest.
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

# Every base image and job image is pinned by digest (an earlier stage or scratch excepted).
while IFS= read -r dockerfile; do
  stages=" scratch "
  while IFS=: read -r line text; do
    read -r -a words <<<"${text#*[Ff][Rr][Oo][Mm]}"
    image=""
    for word in "${words[@]}"; do [[ $word == --* ]] || { image=$word; break; }; done
    [[ $stages == *" ${image,,} "* || $image =~ @sha256:[0-9a-f]{64}$ ]] ||
      refuse "$dockerfile:$line: $image is not pinned by digest"
    [[ ${#words[@]} -ge 2 && ${words[-2],,} == as ]] && stages+="${words[-1],,} "
  done < <(grep -n -i -E '^[[:space:]]*FROM[[:space:]]' "$dockerfile")
done < <(git ls-files '*Dockerfile*')
while IFS=: read -r file line text; do
  [[ $text =~ @sha256:[0-9a-f]{64}([[:space:]]|$) ]] || refuse "$file:$line: image is not pinned by digest"
done < <(grep -nH -E '^[[:space:]]*(-[[:space:]]+)?image:' "${workflows[@]}")

exit "$status"
