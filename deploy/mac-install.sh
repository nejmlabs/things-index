#!/usr/bin/env bash
# ThingsIndex - one-command all-in-one Mac install (local mode):
#
#   bash -c "$(curl -fsSL https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/mac-install.sh)"
#
# Downloads the latest released universal binary (Apple Silicon + Intel) to
# ~/.local/bin, verifies its GitHub build-provenance attestation when the gh
# CLI is available, then asks how you want to connect: print the Claude
# Desktop / Cursor stdio config, or start the local HTTP MCP server right
# away. Everything runs on this Mac; no server or worker setup involved.
# (Connecting this Mac to a remote ThingsIndex server instead? That is
# mac-worker-install.sh.)

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
echo "  ⬇️  ThingsIndex All-in-One Mac Installer"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

mkdir -p "${BIN_DIR}"
trap 'rm -f "${BINARY}.download"' EXIT
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

chmod 0755 "${BINARY}.download"
mv "${BINARY}.download" "${BINARY}"
echo "  ✓ Installed ${BINARY} ($("${BINARY}" version))"

case ":${PATH}:" in
    *":${BIN_DIR}:"*) ;;
    *)
        echo "  • Note: ${BIN_DIR} is not on your PATH; add it to run things-index by name."
        ;;
esac

echo "  ℹ The heading tools need a one-time extra: ${BINARY} install-shortcut"
echo "─────────────────────────────────────────────────────────────"
echo "How do you want to connect?"
echo "  1) Print the Claude Desktop / Cursor stdio config (recommended)"
echo "  2) Start the local HTTP MCP server now (stays in this terminal)"
read -r -p "Choose [1/2, default 1]: " CHOICE
case "${CHOICE:-1}" in
    2)
        exec "${BINARY}" start
        ;;
    *)
        echo
        "${BINARY}" config
        echo
        echo "Paste the block above into your MCP client's configuration."
        echo "Later: '${BINARY} start' runs the local HTTP mode, '${BINARY} update' updates."
        ;;
esac
