#!/usr/bin/env bash
# Build a release archive for the current OS/architecture, with libusb linked
# statically so users don't need to install it.
#
#   scripts/build-release.sh <version> [outdir]
#
# Needs Go, pkg-config and libusb's static library (Homebrew's libusb on macOS;
# libusb-1.0-0-dev and libudev-dev on Debian/Ubuntu). On Linux the binary still
# links libudev and glibc dynamically, so build on the oldest distro you want
# to support.
set -euo pipefail

version=${1:?usage: $0 <version> [outdir]}
out=${2:-dist}
name=xbelite2-configurator
goos=$(go env GOOS)
goarch=$(go env GOARCH)

libdir=$(pkg-config --variable=libdir libusb-1.0)
if [[ ! -f "$libdir/libusb-1.0.a" ]]; then
  echo "error: no static libusb at $libdir/libusb-1.0.a" >&2
  exit 1
fi
case "$goos" in
  darwin) private="-lobjc -Wl,-framework,IOKit -Wl,-framework,CoreFoundation -Wl,-framework,Security" ;;
  linux) private="-ludev -pthread" ;;
  *) echo "error: unsupported GOOS $goos" >&2; exit 1 ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# gousb finds libusb through pkg-config; this .pc is found first and points
# straight at the static archive instead of -lusb-1.0.
mkdir -p "$work/pkgconfig"
cat >"$work/pkgconfig/libusb-1.0.pc" <<EOF
Name: libusb-1.0
Description: libusb, linked statically
Version: $(pkg-config --modversion libusb-1.0)
Libs: $libdir/libusb-1.0.a $private
Cflags: $(pkg-config --cflags libusb-1.0)
EOF

stage="$work/$name-$version-$goos-$goarch"
mkdir -p "$stage" "$out"
PKG_CONFIG_PATH="$work/pkgconfig" CGO_ENABLED=1 \
  go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/$name" .
cp README.md PROTOCOL.md "$stage/"

# Fail if libusb ended up dynamically linked after all.
if [[ "$goos" == darwin ]]; then deps=$(otool -L "$stage/$name"); else deps=$(ldd "$stage/$name"); fi
echo "$deps"
if grep -q libusb <<<"$deps"; then
  echo "error: libusb is dynamically linked" >&2
  exit 1
fi
"$stage/$name" -version

archive="$name-$version-$goos-$goarch.tar.gz"
tar -C "$work" -czf "$out/$archive" "$(basename "$stage")"
(cd "$out" && shasum -a 256 "$archive" >"$archive.sha256")
echo "built $out/$archive"
