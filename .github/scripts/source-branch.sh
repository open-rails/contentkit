#!/usr/bin/env bash
# master takes pull requests only from rc/* and release/* branches of this repository.
set -euo pipefail
repo="$1" head_repo="$2" head_ref="$3"
if [[ "$head_repo" == "$repo" && "$head_ref" =~ ^(rc|release)/. ]]; then
  echo "$head_ref may merge into master."
  exit 0
fi
echo "::error::master takes pull requests only from rc/* and release/* branches of this repository; $head_repo:$head_ref is neither. Retarget this pull request to the open release candidate named in OPEN.md."
exit 1
