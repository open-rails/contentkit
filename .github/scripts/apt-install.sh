#!/usr/bin/env bash
# Installs packages on a GitHub-hosted Ubuntu runner. Its Azure mirror can
# trickle packages for many minutes without ever stalling, which apt's own
# timeout does not catch (actions/runner-images#14823), so an attempt that
# outlasts APT_ATTEMPT_SECONDS is stopped and retried once without it: the
# runner lists it first in /etc/apt/apt-mirrors.txt, other images in sources.
set -euo pipefail

bound=${APT_ATTEMPT_SECONDS:-180}
sudo rm -f /var/lib/man-db/auto-update

attempt() {
	sudo timeout "$bound" sh -c 'apt-get update -qq && apt-get install -y -qq --no-install-recommends "$@"' sh "$@"
}

status=0
attempt "$@" || status=$?
if [ "$status" -ne 0 ]; then
	if [ "$status" -eq 124 ]; then
		echo "::warning::apt took over ${bound}s; retrying without azure.archive.ubuntu.com"
	else
		echo "::warning::apt failed ($status); retrying without azure.archive.ubuntu.com"
	fi
	sudo dpkg --configure -a
	[ -f /etc/apt/apt-mirrors.txt ] && sudo sed -i '/azure\.archive\.ubuntu\.com/d' /etc/apt/apt-mirrors.txt
	sudo sed -i 's#azure\.archive\.ubuntu\.com#archive.ubuntu.com#g' /etc/apt/sources.list.d/*.sources /etc/apt/sources.list 2>/dev/null || true
	attempt "$@"
fi
