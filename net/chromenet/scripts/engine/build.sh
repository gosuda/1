#!/bin/sh
# Builds chrome_net_server from the pinned Chromium release into DIST (default: dist/).
#
#   Linux x86-64 host: linux-amd64 linux-arm64 windows-amd64 windows-arm64. The build runs in a
#   gvx sandbox (rungv, pinned in BUILDENV) on the pinned Debian image, cross-compiling with the
#   clang that the Chromium release pins; the Windows SDK comes from Microsoft through xwin.
#   macOS arm64 host: darwin-arm64 darwin-amd64, natively (Chromium's macOS build needs Xcode).
#
# Usage: scripts/engine/build.sh [-w WORKDIR] [-d DIST] TARGET...
#   WORKDIR keeps downloads, the unpacked source and build outputs between runs
#   (default: ${TMPDIR:-/tmp}/chromenet-engine). Needs Go (to locate the pinned HQQ module)
#   and python3.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=${TMPDIR:-/tmp}/chromenet-engine
dist=$root/dist
while getopts "w:d:" opt; do
	case $opt in
	w) work=$OPTARG ;;
	d) dist=$OPTARG ;;
	*) exit 2 ;;
	esac
done
shift $((OPTIND - 1))
[ $# -gt 0 ] || { echo "usage: $0 [-w WORKDIR] [-d DIST] TARGET..." >&2; exit 2; }
mkdir -p "$work" "$dist"
work=$(cd "$work" && pwd)
dist=$(cd "$dist" && pwd)

env_value() { sed -n "s/^$1=//p" "$here/BUILDENV"; }

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# The HQQ C runtime of the gosuda.org/hqq version in go.mod (verified by go.sum).
hqq_dir=$(cd "$root" && GOWORK=off GOFLAGS=-mod=mod go mod download -json gosuda.org/hqq |
	python3 -c 'import json,sys; print(json.load(sys.stdin)["Dir"])')
rm -rf "$work/hqq"
mkdir -p "$work/hqq"
cp -R "$hqq_dir/c/include" "$hqq_dir/c/src" "$hqq_dir/LICENSE" "$work/hqq/"
chmod -R u+w "$work/hqq"

case $(uname -s)/$(uname -m) in
Darwin/arm64)
	for t in "$@"; do
		case $t in darwin-*) ;; *) echo "build.sh: $t does not build on macOS" >&2; exit 2 ;; esac
	done
	py="python3 $here/engine.py"
	targets=$(printf '%s,' "$@")
	$py fetch --work "$work"
	$py toolchain --work "$work" --targets "${targets%,}"
	for t in "$@"; do
		$py build --work "$work" --target "$t" --hqq "$work/hqq" \
			--out "$dist/chrome_net_server_darwin_${t#darwin-}" --notice "$dist/NOTICE_darwin_${t#darwin-}.txt"
	done
	;;
Linux/x86_64)
	for t in "$@"; do
		case $t in linux-* | windows-*) ;; *) echo "build.sh: $t does not build on Linux" >&2; exit 2 ;; esac
	done
	version=$(env_value rungv_version)
	want=$(env_value rungv_sha256_linux_amd64)
	archive=$work/rungv-$version-linux-amd64.tar.gz
	if [ ! -f "$archive" ] || [ "$(sha256_of "$archive")" != "$want" ]; then
		rm -f "$archive"
		if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
			# The API asset route keeps working while github.com/<repo>/releases/download answers 504.
			gh release download "$version" --repo gfire-sigs/gvx -p "$(basename "$archive")" -D "$work"
		else
			curl -fsSL --retry 30 --retry-delay 30 --retry-max-time 1200 --retry-all-errors -o "$archive" \
				"https://github.com/gfire-sigs/gvx/releases/download/$version/rungv-$version-linux-amd64.tar.gz"
		fi
	fi
	[ "$(sha256_of "$archive")" = "$want" ] || { echo "build.sh: rungv archive does not match BUILDENV" >&2; exit 1; }
	rm -rf "$work/rungv"
	mkdir -p "$work/rungv"
	tar -xzf "$archive" -C "$work/rungv" --strip-components=1
	# The source tree (~12 GB, ~500k files) lives on an ext4lite volume: one host file instead of
	# per-file host calls. Build outputs (~2 GB per target, written and memory-mapped by rustc and
	# lld) go to a tmpfs.
	"$work/rungv/rungv" run --rm \
		--mount "type=volume,src=$work/build.ext4,dst=/work" \
		--tmpfs /work/src/out:size=12g \
		-v "$root:/chromenet:ro" \
		-v "$work/hqq:/hqq:ro" \
		-v "$dist:/dist" \
		"$(env_value image)" \
		sh /chromenet/scripts/engine/container.sh "$(env_value debian_snapshot)" "$@"
	;;
*)
	echo "build.sh: unsupported host $(uname -s)/$(uname -m)" >&2
	exit 2
	;;
esac
ls -l "$dist"
