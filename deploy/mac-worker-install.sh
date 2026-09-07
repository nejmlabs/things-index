#!/usr/bin/env bash
# ThingsIndex - one-command Mac worker install:
#
#   bash -c "$(curl -fsSL https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/mac-worker-install.sh)"
#
# Downloads and verifies the signed release, then lets its Go installer preserve
# the old binary, check signing continuity, and run interactive worker setup.
# Uses macOS system tools; no Python or developer tools are required.
# --no-setup installs the binary but leaves the worker stopped and disabled:
#
#   bash -c "$(curl -fsSL .../mac-worker-install.sh)" install --no-setup

set -euo pipefail
umask 077

fail() { printf '✗ %s\n' "$1" >&2; exit 1; }

if [ "$#" -gt 1 ] || { [ "$#" -eq 1 ] && [ "$1" != "--no-setup" ]; }; then
    fail 'Usage: mac-worker-install.sh [--no-setup]'
fi

REPO="nejmlabs/things-index"
ASSET="things-index-darwin-universal"
# Public release-certificate fingerprint, deliberately pinned before executing
# downloaded code. Changing this trust anchor requires an explicit source edit.
MACOS_RELEASE_CERTIFICATE_SHA1="411458A567FC772FF286B076E379703960D71231"

if [ "$(uname -s)" != "Darwin" ]; then
    fail 'This installer is for macOS (the Mac that runs Things 3).'
fi

BIN_DIR="${HOME}/.local/bin"
BINARY="${BIN_DIR}/things-index"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ⬇️  ThingsIndex Mac Worker Installer"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

mkdir -p "${BIN_DIR}"
# Fresh private staging avoids inheriting metadata from an earlier download.
# Keep it until the Go installer returns; it reads and verifies this source.
DOWNLOAD_DIR="$(mktemp -d "${BIN_DIR}/.things-index-download.XXXXXX")"
DOWNLOAD_PATH="${DOWNLOAD_DIR}/things-index"
cleanup() {
    result=$?
    trap - EXIT
    rm -rf -- "${DOWNLOAD_DIR}"
    exit "${result}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

echo "• Downloading the latest things-index release..."
curl -fL --progress-bar -o "${DOWNLOAD_PATH}" \
    "https://github.com/${REPO}/releases/latest/download/${ASSET}"

if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    echo "• Verifying build provenance attestation..."
    gh attestation verify "${DOWNLOAD_PATH}" --repo "${REPO}" >/dev/null
    echo "  ✓ Provenance verified: built by GitHub Actions from ${REPO}"
else
    echo "  • Skipping provenance verification (no authenticated gh CLI)."
    echo "    To verify by hand later: gh attestation verify ${BINARY} --repo ${REPO}"
fi

# Inspect attribute names only. Never remove quarantine or change system trust.
ATTRIBUTES="$(/usr/bin/xattr "${DOWNLOAD_PATH}")" || fail 'Could not inspect downloaded executable metadata.'
while IFS= read -r attribute; do
    if [ "${attribute}" = "com.apple.quarantine" ]; then
        fail 'Downloaded executable is quarantined; it will not be run or installed. Review its origin and macOS security status manually.'
    fi
done <<< "${ATTRIBUTES}"

echo "• Verifying the release signature and pinned certificate..."
REQUIREMENT="=identifier \"com.nejmlabs.things-index\" and certificate leaf = H\"${MACOS_RELEASE_CERTIFICATE_SHA1}\""
LC_ALL=C /usr/bin/codesign --verify --strict --all-architectures \
    --test-requirement "${REQUIREMENT}" "${DOWNLOAD_PATH}" ||
    fail 'Release signature or certificate does not match the trusted ThingsIndex release identity.'

chmod 0755 "${DOWNLOAD_PATH}"
"${DOWNLOAD_PATH}" install-worker "$@"
