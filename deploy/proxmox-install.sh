#!/usr/bin/env bash
# ThingsIndex - Automated Proxmox VE LXC Provisioning Script
# Run on Proxmox Host: bash -c "$(wget -qLO - https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/proxmox-install.sh)"

set -euo pipefail

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  🚀 ThingsIndex Proxmox LXC Installer"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# 1. Find Next Available Container ID
CT_ID=$(pvesh get /cluster/nextid)
CT_NAME="things-index"
CT_RAM="512"
CT_CORES="1"
CT_DISK="4"
BRIDGE="vmbr0"

echo "• Allocating Container ID: ${CT_ID}"

# 2. Use an existing Debian 12/13 standard template matching the host
#    architecture, or download the newest one. The arch filter matters: a
#    stray template for another architecture must never be picked.
HOST_ARCH=$(dpkg --print-architecture)
TEMPLATE=$(pveam list local | awk '{print $1}' | grep -E 'debian-1[23]-standard' | grep "_${HOST_ARCH}" | sort -V | tail -n 1 || true)
if [ -z "${TEMPLATE}" ]; then
    echo "• Downloading latest Debian standard template (${HOST_ARCH})..."
    pveam update
    TEMPLATE_NAME=$(pveam available --section system | awk '{print $2}' | grep -E '^debian-1[23]-standard' | grep "_${HOST_ARCH}" | sort -V | tail -n 1 || true)
    if [ -z "${TEMPLATE_NAME}" ]; then
        echo "✗ No Debian 12/13 standard template for ${HOST_ARCH} found in the pveam index" >&2
        exit 1
    fi
    pveam download local "${TEMPLATE_NAME}"
    TEMPLATE=$(pveam list local | grep "${TEMPLATE_NAME}" | head -n 1 | awk '{print $1}')
fi

echo "• Using template: ${TEMPLATE}"

# 3. Pick a storage pool that can hold container root disks; the default
#    'local' directory storage often cannot, so relying on pct's default
#    fails with "storage 'local' does not support container directories".
STORAGE=$(pvesm status --content rootdir 2>/dev/null | awk 'NR>1 && $3 == "active" {print $1; exit}')
if [ -z "${STORAGE}" ]; then
    echo "✗ No active storage supports container disks (rootdir); enable one under Datacenter > Storage" >&2
    exit 1
fi
echo "• Using container storage: ${STORAGE}"

# 4. Create Container
echo "• Creating unprivileged LXC container..."
pct create "${CT_ID}" "${TEMPLATE}" \
    --hostname "${CT_NAME}" \
    --cores "${CT_CORES}" \
    --memory "${CT_RAM}" \
    --swap 0 \
    --rootfs "${STORAGE}:${CT_DISK}" \
    --features nesting=1 \
    --net0 "name=eth0,bridge=${BRIDGE},ip=dhcp" \
    --unprivileged 1 \
    --onboot 1 \
    --start 1

echo "• Waiting for container network..."
sleep 5

# 5. Generate Tokens
PUB_TOKEN=$(openssl rand -hex 32)
WRK_TOKEN=$(openssl rand -hex 32)
DSH_TOKEN=$(openssl rand -hex 32)

# 6. Install Dependencies & Build inside LXC
#    (single-quoted so nothing from the host - especially tokens - lands in argv)
echo "• Installing dependencies and building (this downloads the Go toolchain)..."
pct exec "${CT_ID}" -- bash -c '
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl git ufw sqlite3 ca-certificates build-essential > /dev/null

# Debian ships a Go too old for this module; install the official toolchain.
GO_VERSION=1.26.0
ARCH=$(dpkg --print-architecture)
curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" | tar -C /usr/local -xz

# Clone and build ThingsIndex
git clone https://github.com/nejmlabs/things-index.git /root/things-index
cd /root/things-index
/usr/local/go/bin/go build -o /usr/local/bin/things-index-server ./cmd/things-index-server

# Create dedicated system user
useradd -r -s /bin/false -d /var/lib/things-index things-index || true
mkdir -p /etc/things-index /var/lib/things-index
chown -R things-index:things-index /var/lib/things-index
'

# 7. Push server configuration into the container as a file, so tokens never
#    appear on a command line visible in the host process list.
ENV_FILE=$(mktemp)
chmod 600 "${ENV_FILE}"
cat << EOF > "${ENV_FILE}"
THINGS_INDEX_LISTEN_ADDR=0.0.0.0:8080
THINGS_INDEX_ALLOW_UNSPECIFIED_BIND=1
THINGS_INDEX_DB_PATH=/var/lib/things-index/queue.sqlite
THINGS_INDEX_PUBLIC_TOKEN=${PUB_TOKEN}
THINGS_INDEX_WORKER_TOKEN=${WRK_TOKEN}
THINGS_INDEX_DASHBOARD_TOKEN=${DSH_TOKEN}
EOF
pct push "${CT_ID}" "${ENV_FILE}" /etc/things-index/server.env --perms 600 --user 0 --group 0
rm -f "${ENV_FILE}"

# 8. Install systemd service & firewall
pct exec "${CT_ID}" -- bash -c '
set -euo pipefail
cat << EOF > /etc/systemd/system/things-index-server.service
[Unit]
Description=ThingsIndex MCP Server
After=network.target

[Service]
Type=simple
User=things-index
Group=things-index
WorkingDirectory=/var/lib/things-index
EnvironmentFile=/etc/things-index/server.env
ExecStart=/usr/local/bin/things-index-server
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now things-index-server

ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp
ufw allow 8080/tcp
echo y | ufw enable
'

IP=$(pct exec "${CT_ID}" -- ip -4 addr show eth0 | grep -oP '(?<=inet\s)\d+(\.\d+){3}' | head -n 1)

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅ ThingsIndex Server Successfully Deployed!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  • Container ID:      ${CT_ID}"
echo "  • IP Address:        ${IP}"
echo "  • Server Endpoint:   http://${IP}:8080/mcp"
echo "  • Public Token:      ${PUB_TOKEN}"
echo "  • Worker Token:      ${WRK_TOKEN}"
echo "  • Dashboard Token:   ${DSH_TOKEN}"
echo "────────────────────────────────────────────────────────────"
echo "  Paste this into your Claude Desktop / Pebble config:"
echo ""
cat << EOF
{
  "mcpServers": {
    "things": {
      "url": "http://${IP}:8080/mcp",
      "headers": {
        "Authorization": "Bearer ${PUB_TOKEN}"
      }
    }
  }
}
EOF
echo "────────────────────────────────────────────────────────────"
echo "  Connect the Mac worker (it requires HTTPS off-loopback):"
echo "    • Behind an HTTPS reverse proxy (see deploy/traefik):"
echo "        things-index worker --setup   # URL: https://<your-proxy-host>"
echo "    • Or keep an SSH tunnel open from the Mac:"
echo "        ssh -N -L 8080:${IP}:8080 <user>@<a-lan-host>"
echo "        things-index worker --setup   # URL: http://127.0.0.1:8080"
echo ""
echo "  ⚠ The MCP endpoint above is plain HTTP on your LAN, so bearer tokens"
echo "    travel unencrypted. Prefer terminating HTTPS at a reverse proxy for"
echo "    anything beyond a trusted home network."
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
