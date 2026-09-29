#!/bin/sh
# EZDR client installer.
#
#   curl -fsSL https://github.com/jlbyh2o/ezdr/releases/download/v<version>/install.sh | sh -s -- <token> [enroll options]
#
# Installs the ezdr package from this release and then runs `ezdr enroll`
# with the given arguments. The release's checksums must carry a valid
# signature from EZDR's release key (below), and the package must match
# them. Enrollment still shows the planned changes and asks for
# confirmation.
#
# Set EZDR_DOWNLOAD_URL to install from a mirror instead of GitHub; its
# files are verified the same way.
set -eu

VERSION="__VERSION__"
BASE_URL="${EZDR_DOWNLOAD_URL:-https://github.com/jlbyh2o/ezdr/releases/download/v${VERSION}}"
PACKAGE="ezdr_linux_amd64.deb"

# EZDR's release signing key (also in packaging/release-signing-key.asc).
SIGNING_KEY_FPR="1EEE299995900231E3F02BF0F2483414DBDDAB2B"
SIGNING_KEY='-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEarva/hYJKwYBBAHaRw8BAQdAUTlOzJ9JPpaW7rohdv+lcSe6G9qP0ABxUQ4q
/YY0lyS0P0VaRFIgcmVsZWFzZSBzaWduaW5nIDw2MzcyOTY0K2psYnloMm9AdXNl
cnMubm9yZXBseS5naXRodWIuY29tPoiTBBMWCgA7FiEEHu4pmZWQAjHj8Cvw8kg0
FNvdqysFAmq72v4CGwMFCwkIBwICIgIGFQoJCAsCBBYCAwECHgcCF4AACgkQ8kg0
FNvdqyvrvAEAtIIV5VAHRDz+bOekfUWkNYzMG+8/6JRObPFD2m/BmtkA/3djg5rE
6CiuQ5IC5m9LR/+nypxPhO+9qxRkMfAFKHUN
=Ka23
-----END PGP PUBLIC KEY BLOCK-----'

fail() {
    echo "ezdr install: $*" >&2
    exit 1
}

download() {
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -o "$1" "$2" || fail "could not download $2"
}

# verify checks checksums.txt's signature in a keyring holding only the
# release key.
verify() {
    GNUPGHOME="$tmp/gnupg"
    export GNUPGHOME
    mkdir -m 700 "$GNUPGHOME"
    printf '%s\n' "$SIGNING_KEY" | gpg --batch --quiet --import 2>/dev/null ||
        fail "could not read the release signing key"
    gpg --batch --status-fd 1 --verify "$tmp/checksums.txt.sig" "$tmp/checksums.txt" 2>/dev/null |
        grep -q "^\[GNUPG:\] VALIDSIG $SIGNING_KEY_FPR " ||
        fail "the release's checksums aren't signed by EZDR's release key"
}

main() {
    [ "$(id -u)" -eq 0 ] || fail "run this as root"
    command -v pveversion >/dev/null 2>&1 || fail "this does not look like a Proxmox VE host (pveversion not found)"
    command -v gpg >/dev/null 2>&1 || fail "gpg is needed to verify the release"
    [ "$(dpkg --print-architecture)" = "amd64" ] || fail "only amd64 hosts are supported"
    [ "$#" -ge 1 ] || fail "usage: sh -s -- <enrollment token> [enroll options]"

    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT

    echo "Downloading EZDR ${VERSION}..."
    download "$tmp/checksums.txt" "$BASE_URL/checksums.txt"
    download "$tmp/checksums.txt.sig" "$BASE_URL/checksums.txt.sig"
    verify
    download "$tmp/$PACKAGE" "$BASE_URL/$PACKAGE"
    (cd "$tmp" && grep " ${PACKAGE}\$" checksums.txt | sha256sum -c --quiet -) ||
        fail "checksum verification failed for $PACKAGE"

    echo "Installing the ezdr package..."
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$tmp/$PACKAGE" >/dev/null

    echo
    ezdr enroll "$@"
}

# Nothing runs until the whole script has arrived.
main "$@"
