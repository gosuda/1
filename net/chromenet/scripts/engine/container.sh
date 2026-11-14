#!/bin/sh
# Runs inside the gvx sandbox (scripts/engine/build.sh starts it). Installs the few host tools the
# Chromium build scripts call from the pinned Debian snapshot, then fetches, prepares and builds.
#
# Mounts: /chromenet (this module, read-only), /work (ext4lite volume: source, toolchains, out),
# /hqq (HQQ C runtime, read-only), /dist (outputs).
# Usage: container.sh SNAPSHOT TARGET...
set -eu

snapshot=$1
shift

cat >/etc/apt/sources.list.d/debian.sources <<EOF
Types: deb
URIs: http://snapshot.debian.org/archive/debian/$snapshot/
Suites: trixie trixie-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
EOF
apt-get -o Acquire::Check-Valid-Until=false -o Acquire::Retries=5 update
DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 install -y --no-install-recommends \
	ca-certificates python3 xz-utils pkg-config patch

engine="python3 /chromenet/scripts/engine/engine.py"
targets=$(printf '%s,' "$@")
$engine fetch --work /work
$engine toolchain --work /work --targets "${targets%,}"
for t in "$@"; do
	goos=${t%-*}
	goarch=${t#*-}
	ext=
	[ "$goos" = windows ] && ext=.exe
	$engine build --work /work --target "$t" --hqq /hqq \
		--out "/dist/chrome_net_server_${goos}_${goarch}${ext}" --notice "/dist/NOTICE_${goos}_${goarch}.txt"
done
