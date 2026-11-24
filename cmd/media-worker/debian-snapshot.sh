#!/bin/sh
# Installs Debian packages from snapshot.debian.org at DEBIAN_SNAPSHOT, by
# default the ARG of the Dockerfile beside this script: the worker image and
# CI's native jobs install the same versions. --upgrade first moves the
# installed packages to the snapshot. Runs as root on Debian trixie.
set -eu

: "${DEBIAN_SNAPSHOT:=$(sed -n 's/^ARG DEBIAN_SNAPSHOT=//p' "$(dirname "$0")/Dockerfile")}"
case $DEBIAN_SNAPSHOT in
  [0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z) ;;
  *) echo "debian-snapshot: DEBIAN_SNAPSHOT '$DEBIAN_SNAPSHOT' is not YYYYMMDDTHHMMSSZ" >&2; exit 64 ;;
esac
upgrade=false
if [ "${1:-}" = --upgrade ]; then upgrade=true; shift; fi

sources=/etc/apt/sources.list.d/debian.sources
sed -i -E "s#^URIs: http://deb\.debian\.org/(debian|debian-security)\$#URIs: http://snapshot.debian.org/archive/\1/$DEBIAN_SNAPSHOT#" "$sources"
if grep -q deb.debian.org "$sources"; then echo "debian-snapshot: $sources still names deb.debian.org" >&2; exit 1; fi
# A snapshot outlives its Release files' Valid-Until. Later apt runs read this too.
printf 'Acquire::Check-Valid-Until "false";\nAcquire::Retries "5";\n' > /etc/apt/apt.conf.d/80snapshot

export DEBIAN_FRONTEND=noninteractive
apt-get update
if $upgrade; then apt-get upgrade -y; fi
apt-get install -y --no-install-recommends "$@"
rm -rf /var/lib/apt/lists/*
