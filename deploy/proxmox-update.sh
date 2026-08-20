#!/usr/bin/env bash
# ThingsIndex - Update the server inside its Proxmox LXC container.
# Run on the Proxmox host:
#   bash -c "$(wget -qLO - https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/proxmox-update.sh)"
# Pass a container ID as the first argument; without one, the container
# named "things-index" is found automatically.

set -euo pipefail

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  🔄 ThingsIndex Server Updater"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

CT_ID="${1:-}"
if [ -z "${CT_ID}" ]; then
    CT_ID=$(pct list | awk 'NR>1 && $NF=="things-index" {print $1}')
fi
case "${CT_ID}" in
    "")
        echo "✗ No container named 'things-index' found; pass its ID: proxmox-update.sh <CTID>" >&2
        exit 1
        ;;
    *[!0-9]*)
        echo "✗ Several containers named 'things-index' exist; pass the ID of the one to update:" >&2
        pct list | awk 'NR==1 || $NF=="things-index"' >&2
        exit 1
        ;;
esac

if ! pct status "${CT_ID}" | grep -q running; then
    echo "✗ Container ${CT_ID} is not running (pct start ${CT_ID} first)" >&2
    exit 1
fi
if ! pct exec "${CT_ID}" -- test -d /root/things-index/.git; then
    echo "✗ Container ${CT_ID} has no /root/things-index checkout; was it provisioned by proxmox-install.sh?" >&2
    exit 1
fi
echo "• Updating container ${CT_ID}..."

# The CGO sqlite3 build thrashes below ~2GB when Go's build cache is cold, so
# mirror the installer: raise memory for the build, restore it afterwards.
CURRENT_RAM=$(pct config "${CT_ID}" | awk '/^memory:/ {print $2}')
if [ "${CURRENT_RAM}" -lt 2048 ]; then
    echo "• Raising container memory to 2048MB for the build (currently ${CURRENT_RAM}MB)..."
    pct set "${CT_ID}" --memory 2048
    trap "pct set ${CT_ID} --memory ${CURRENT_RAM}" EXIT
fi

pct exec "${CT_ID}" -- bash -c '
set -euo pipefail
cd /root/things-index
BEFORE=$(git rev-parse --short HEAD)
git pull --ff-only --quiet
AFTER=$(git rev-parse --short HEAD)
if [ "${BEFORE}" = "${AFTER}" ] && [ -x /usr/local/bin/things-index-server ]; then
    echo "  ✓ Already up to date at ${AFTER}."
    exit 0
fi
echo "• Building ${BEFORE} → ${AFTER}..."
/usr/local/go/bin/go build -o /usr/local/bin/things-index-server ./cmd/things-index-server
systemctl restart things-index-server
echo "  ✓ Rebuilt and restarted at ${AFTER}."
'

STATE=$(pct exec "${CT_ID}" -- systemctl is-active things-index-server)
HEALTH=$(pct exec "${CT_ID}" -- bash -c 'curl -s -o /dev/null -w "%{http_code}" --max-time 5 http://127.0.0.1:8080/healthz || true')
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
if [ "${STATE}" = "active" ] && [ "${HEALTH}" = "200" ]; then
    echo "  ✅ Update complete: service ${STATE}, health check ${HEALTH}."
else
    echo "  ⚠ Service state: ${STATE}, health check: ${HEALTH} - inspect with:"
    echo "      pct exec ${CT_ID} -- journalctl -u things-index-server -n 30"
    exit 1
fi
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
