#!/usr/bin/env bash
# Installs Ubuntu packages on a GitHub-hosted runner from the .debs in APT_DEBS
# (the apt-install action caches that directory per runner image), with no
# network. When it holds none, or they no longer install, they are downloaded.
# The runner's Azure mirror can trickle packages for many minutes without ever
# stalling, which apt's own timeout does not catch (actions/runner-images#14823),
# so a download that outlasts APT_ATTEMPT_SECONDS is stopped and retried once
# without it: the runner lists it first in /etc/apt/apt-mirrors.txt, other
# images in sources.
set -euo pipefail

bound=${APT_ATTEMPT_SECONDS:-180}
debs=${APT_DEBS:-$(mktemp -d)}
mkdir -p "$debs"
sudo rm -f /var/lib/man-db/auto-update

have_debs() { compgen -G "$debs/*.deb" >/dev/null; }
# No sources: only the given .debs and what is installed can satisfy a dependency.
install_debs() {
	sudo apt-get install -y -qq --no-install-recommends -o Dir::Etc::SourceList=/dev/null \
		-o Dir::Etc::SourceParts="$(mktemp -d)" "$debs"/*.deb
}

if have_debs; then
	install_debs && exit 0
	echo "::warning::cached packages no longer install on this image; downloading them"
	sudo dpkg --configure -a
	sudo rm -f "$debs"/*.deb
fi

download() {
	sudo timeout "$bound" sh -c 'mkdir -p "$0/partial" && apt-get update -qq &&
		apt-get install -y -qq --no-install-recommends --download-only -o Dir::Cache::Archives="$0" "$@"' "$debs" "$@"
}

status=0
download "$@" || status=$?
if [ "$status" -ne 0 ]; then
	if [ "$status" -eq 124 ]; then
		echo "::warning::apt took over ${bound}s; retrying without azure.archive.ubuntu.com"
	else
		echo "::warning::apt failed ($status); retrying without azure.archive.ubuntu.com"
	fi
	[ -f /etc/apt/apt-mirrors.txt ] && sudo sed -i '/azure\.archive\.ubuntu\.com/d' /etc/apt/apt-mirrors.txt
	sudo sed -i 's#azure\.archive\.ubuntu\.com#archive.ubuntu.com#g' /etc/apt/sources.list.d/*.sources /etc/apt/sources.list 2>/dev/null || true
	download "$@"
fi
# The cache keeps only the .debs, readable by the runner user.
sudo rm -rf "$debs/partial" "$debs/lock"
sudo chown -R "$(id -u):$(id -g)" "$debs"
# No .deb means every package was already installed.
if have_debs; then install_debs; else dpkg -s "$@" >/dev/null; fi
