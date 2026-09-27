#!/bin/sh
# EZDR client installer.
#
#   curl -fsSL https://github.com/jlbyh2o/ezdr/releases/latest/download/install.sh | sh -s -- <token> [enroll options]
#
# Installs the ezdr package from this release (verifying its SHA-256
# checksum) and then runs `ezdr enroll` with the given arguments. Enrollment
# still shows the planned changes and asks for confirmation.
#
# Set EZDR_DOWNLOAD_URL to install from a mirror instead of GitHub.
set -eu

VERSION="__VERSION__"
BASE_URL="${EZDR_DOWNLOAD_URL:-https://github.com/jlbyh2o/ezdr/releases/download/v${VERSION}}"
PACKAGE="ezdr_linux_amd64.deb"

fail() {
    echo "ezdr install: $*" >&2
    exit 1
}

[ "$(id -u)" -eq 0 ] || fail "run this as root"
command -v pveversion >/dev/null 2>&1 || fail "this does not look like a Proxmox VE host (pveversion not found)"
[ "$(dpkg --print-architecture)" = "amd64" ] || fail "only amd64 hosts are supported"
[ "$#" -ge 1 ] || fail "usage: sh -s -- <enrollment token> [enroll options]"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading EZDR ${VERSION}..."
curl -fsSL -o "$tmp/$PACKAGE" "$BASE_URL/$PACKAGE"
curl -fsSL -o "$tmp/checksums.txt" "$BASE_URL/checksums.txt"

(cd "$tmp" && grep " ${PACKAGE}\$" checksums.txt | sha256sum -c --quiet -) ||
    fail "checksum verification failed for $PACKAGE"

echo "Installing the ezdr package..."
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$tmp/$PACKAGE" >/dev/null

echo
exec ezdr enroll "$@"
