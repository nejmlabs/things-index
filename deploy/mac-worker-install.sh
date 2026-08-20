#!/usr/bin/env bash
# ThingsIndex — one-command Mac worker install:
#
#   bash -c "$(curl -fsSL https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/mac-worker-install.sh)"
#
# Downloads the latest released universal binary (Apple Silicon + Intel) to
# ~/.local/bin, verifies its GitHub build-provenance attestation when the gh
# CLI is available, and launches the interactive setup wizard. Pass
# --no-setup to install the binary only.
set -euo pipefail

REPO="nejmlabs/things-index"
ASSET="things-index-darwin-universal"

if [ "$(uname -s)" != "Darwin" ]; then
    echo "✗ This installer is for macOS (the Mac that runs Things 3)." >&2
    exit 1
fi

BIN_DIR="${HOME}/.local/bin"
BINARY="${BIN_DIR}/things-index"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ⬇️  ThingsIndex Mac Worker Installer"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

mkdir -p "${BIN_DIR}"
echo "• Downloading the latest things-index release..."
curl -fL --progress-bar -o "${BINARY}.download" \
    "https://github.com/${REPO}/releases/latest/download/${ASSET}"

# Verify GitHub's build-provenance attestation when possible: proof the
# binary was built by this repository's release workflow on GitHub's
# runners, not on someone's laptop.
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    echo "• Verifying build provenance attestation..."
    gh attestation verify "${BINARY}.download" --repo "${REPO}" >/dev/null
    echo "  ✓ Provenance verified: built by GitHub Actions from ${REPO}"
else
    echo "  • Skipping provenance verification (no authenticated gh CLI)."
    echo "    To verify by hand later: gh attestation verify ${BINARY} --repo ${REPO}"
fi

mv "${BINARY}.download" "${BINARY}"
chmod 0755 "${BINARY}"
echo "  ✓ Installed ${BINARY} ($("${BINARY}" version))"

case ":${PATH}:" in
    *":${BIN_DIR}:"*) ;;
    *)
        echo "  • Note: ${BIN_DIR} is not on your PATH. The background daemon"
        echo "    does not need it, but add it to run things-index by name."
        ;;
esac

if [ "${1:-}" = "--no-setup" ]; then
    echo "• Install complete. Run '${BINARY} worker --setup' when ready."
    exit 0
fi

exec "${BINARY}" worker --setup
