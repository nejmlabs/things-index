# Homelab deployment

This profile keeps the public-facing MCP server and durable queue on Linux while
the Things worker runs outbound-only on an always-on Mac:

```text
Pebble Cloud ──HTTPS──> reverse proxy ──private HTTP──> Linux server
                                                              ▲
                                                              │ private HTTPS
                                                              │
Things <── native Shortcut <── Mac worker ─────────────────────┘
```

## Linux server

Give the server a stable private address and set `THINGS_INDEX_LISTEN_ADDR` to
that exact address, for example `192.168.1.50:8080`. The binary rejects
hostnames and public IP addresses, and rejects `0.0.0.0` / `[::]` unless
`THINGS_INDEX_ALLOW_UNSPECIFIED_BIND=1` is set explicitly — the Docker and
Proxmox LXC deployments set it because containers must bind all interfaces for
port publishing to work. If the address comes from DHCP, reserve the lease in
the DHCP server: the reverse proxy and firewall rules below pin this address
and break silently when it changes.

Install the server as `/usr/local/bin/things-index-server`. The fast path is
the static release binary (amd64 and arm64, provenance-attested like the Mac
asset, runs on any distro):

```sh
curl -fL -o /usr/local/bin/things-index-server \
  "https://github.com/nejmlabs/things-index/releases/latest/download/things-index-server-linux-$(dpkg --print-architecture)"
chmod 0755 /usr/local/bin/things-index-server
# Verify provenance when the gh CLI is available:
#   gh attestation verify /usr/local/bin/things-index-server --repo nejmlabs/things-index
```

Or build it natively (the Proxmox installer automates this whole section):

```sh
# Go 1.26+ (distribution-packaged Go is usually too old) plus a C toolchain;
# the CGO SQLite build wants roughly 2GB of free memory or it thrashes.
apt-get install -y git build-essential ca-certificates
curl -fsSL "https://go.dev/dl/go1.26.0.linux-$(dpkg --print-architecture).tar.gz" | tar -C /usr/local -xz

git clone https://github.com/nejmlabs/things-index.git
cd things-index
/usr/local/go/bin/go build -o /usr/local/bin/things-index-server ./cmd/things-index-server
```

The example files under `deploy/systemd/` use a dedicated `things-index`
service account and store the durable queue under `/var/lib/things-index/`.
Install and activate them with:

```sh
useradd -r -s /bin/false -d /var/lib/things-index things-index
mkdir -p /etc/things-index /var/lib/things-index
chown things-index:things-index /var/lib/things-index
install -m 600 deploy/systemd/server.env.example /etc/things-index/server.env
install -m 644 deploy/systemd/things-index-server.service /etc/systemd/system/
# edit /etc/things-index/server.env (listen address + the three tokens), then:
systemctl daemon-reload
systemctl enable --now things-index-server
```

Set distinct public and worker tokens in `/etc/things-index/server.env`, make
the file root-owned with mode `0600`, and restrict the host firewall so port
8080 accepts traffic only from the reverse proxy, for example:

```sh
ufw default deny incoming
ufw allow 22/tcp
ufw allow from <proxy-ip> to any port 8080 proto tcp
ufw enable
```

The Proxmox installer applies this restriction when `THINGS_INDEX_PROXY_IP`
is set before running it; otherwise it opens 8080 to the LAN and its final
banner prints the commands to tighten the rule once the proxy exists.

The example environment file retains successful jobs for seven days and failed
jobs for thirty days. See [Deployment profiles](deployment.md) for retention
settings and cleanup guarantees.

## Reverse proxy

Any reverse proxy or tunnel that satisfies the HTTPS contract in
[`deploy/README.md`](../deploy/README.md) works: publicly trusted TLS,
exactly `/mcp` published, everything else on private routing. The generic
Traefik file under `deploy/traefik/` is one worked example of that contract;
it demonstrates separate routes (its
[README](../deploy/traefik/README.md) has the fill-in checklist and the
live-edit gotchas):

- public `https://things-index.example.com/mcp`;
- private `https://things-index-worker.internal.example.com/worker/...`; and
- private `https://things-index-worker.internal.example.com/healthz`; and
- optional private `https://things-index-dashboard.internal.example.com/dashboard`.

Replace every example hostname, address, LAN range, entry point, and certificate
setting with values appropriate for the deployment. The public router must not
match `/worker/`, `/healthz`, or `/dashboard`.

To enable the read-only dashboard, set a third independent
`THINGS_INDEX_DASHBOARD_TOKEN` in the server environment file and route
`/dashboard` only through private HTTPS. Authenticate with username
`things-index` and the dashboard token. If the setting is empty, the route does
not exist.

Pebble Cloud needs public DNS and HTTPS reachability to the MCP hostname. This
can use a narrowly scoped inbound route or an outbound tunnel that publishes
only `/mcp` — `deploy/cloudflare/` shows a Cloudflare Tunnel configuration
doing exactly that, with no inbound ports on the LAN at all. Its README is a
step-by-step runbook, including the headless `cloudflared tunnel login` flow.

## No-domain alternative: persistent SSH tunnel

The worker's one exception to the HTTPS requirement is plain HTTP to a
literal loopback address, so a persistent SSH tunnel from the Mac to the
server satisfies a LAN-only deployment with no domain, certificates, or
reverse proxy at all. The trade-off: nothing is published, so Pebble Cloud
(or any client outside the tunnel) cannot reach the server. Local MCP
clients on the same Mac can, at `http://127.0.0.1:8080/mcp`.

On the Mac, create a dedicated key for the tunnel:

```sh
ssh-keygen -t ed25519 -N "" -C things-index-tunnel -f ~/.ssh/things-index-tunnel
```

On the server, append the public key to the SSH user's
`~/.ssh/authorized_keys`, restricted so the key can only forward to the
server port — it cannot run commands even if it leaks:

```text
restrict,port-forwarding,permitopen="127.0.0.1:8080" ssh-ed25519 AAAA... things-index-tunnel
```

Back on the Mac, install the launchd template that keeps the tunnel up
across reboots and dropped links (`ServerAliveInterval` detects dead
connections and exits; launchd restarts ssh):

```sh
mkdir -p ~/Library/Logs/ThingsIndex ~/Library/LaunchAgents
curl -fsSL -o ~/Library/LaunchAgents/com.nejmlabs.things-index-tunnel.plist \
  https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/launchd/com.nejmlabs.things-index-tunnel.plist.example
# Edit the file: REPLACE_ME → your username, REPLACE_WITH_SSH_TARGET → <user>@<server-host>
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.nejmlabs.things-index-tunnel.plist
curl -s http://127.0.0.1:8080/healthz   # proves the tunnel end to end
```

Then run the worker wizard with `http://127.0.0.1:8080` as the server URL.

The forward destination must be an address the server actually listens on:
`127.0.0.1:8080` works for the Proxmox and Docker installs (they bind all
interfaces), while a native systemd install bound to its LAN address needs
that exact `THINGS_INDEX_LISTEN_ADDR` value as the destination — change it
in the plist's `-L` argument and mirror it in the key's `permitopen`. The
same applies when the SSH target is a different LAN host rather than the
server itself (and the server's firewall must then admit that host on 8080).

## Internal DNS

LAN clients — the Mac worker above all — reach the proxy by name, so both
hostnames need records in the network's internal DNS (Pi-hole, router local
DNS, dnsmasq, …) pointing at the reverse proxy host:

```text
things-index.example.com          → <proxy-ip>
things-index-worker.example.com   → <proxy-ip>
```

The public MCP hostname can be split-horizon: on the public internet it
resolves to the Cloudflare Tunnel CNAME, on the LAN to the reverse proxy. LAN
traffic then stays local while Pebble Cloud comes in through the tunnel. The
consequence for testing: from inside the LAN a plain `curl` against the
hostname exercises the proxy, not the tunnel — the verification checklist
below pins the tunnel path explicitly.

## Mac worker

The supported path is the one-command installer, run in the logged-in Mac GUI
session:

```sh
bash -c "$(curl -fsSL https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/mac-worker-install.sh)"
```

It installs the latest released universal binary to `~/.local/bin`, verifying
GitHub's build-provenance attestation when the `gh` CLI is available, and
launches the setup wizard (`things-index worker --setup`, rerunnable anytime).
The wizard verifies the server URL and worker token against the live server, validates
the optional Things auth token with one disposable test task, installs the
bundled ThingsIndex Helper shortcut and settles its privacy dialogs, installs
the LaunchAgent, and waits for the daemon to record its one-time Things 3
automation consent. Approve each macOS dialog with **Always Allow** during this
deliberate setup run.

To install the binary without launching the wizard (for example, ahead of the
HTTPS route existing), pass `--no-setup` after a placeholder for `argv[0]`:

```sh
bash -c "$(curl -fsSL https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/mac-worker-install.sh)" install --no-setup
```

The worker requires HTTPS because its server is not on loopback; a reverse
proxy hostname, an SSH tunnel to loopback, or a Cloudflare Tunnel hostname
(see `deploy/cloudflare/`) all satisfy it.

For a manual install instead of the wizard, build `things-index-worker`
natively on the Mac and copy and fill in
`deploy/launchd/com.nejmlabs.things-index-worker.plist.example` with:

```text
THINGS_INDEX_SERVER_URL=https://things-index-worker.internal.example.com
THINGS_INDEX_WORKER_TOKEN=<independent worker token>
THINGS_INDEX_JOURNAL_PATH=<user-owned path>/journal.sqlite
THINGS_INDEX_THINGS_AUTH_TOKEN=<optional Things URL-scheme auth token>
THINGS_INDEX_JOURNAL_RETENTION_DAYS=30
```

The daemon runs its automation-consent preflight at first start either way.
Routine operation must not prompt, and the worker accepts no inbound
connections.

## Upgrading

- **Proxmox LXC** — run the update script on the Proxmox host; it finds the
  `things-index` container (or takes a CTID argument), raises memory for the
  build, pulls, rebuilds, restarts, and verifies `/healthz`. It refreshes the
  binary only — the systemd unit and env file stay as installed:

  ```sh
  bash -c "$(wget -qLO - https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/proxmox-update.sh)"
  ```

- **Docker Compose** — `git pull && docker compose up -d --build`.
- **Native systemd** — re-download the release binary (or repeat the native
  build, which needs the same ~2GB of free memory as at install time), then
  `systemctl restart things-index-server`.
- **Mac worker** — `things-index update` self-updates: it queries the latest
  release, downloads that pinned tag's binary, verifies its provenance
  attestation when an authenticated `gh` CLI is available, smoke-tests it,
  and swaps it in (keeping a `.old` rollback copy) before restarting the
  launchd agent. If Things permission prompts reappear after an update
  (replacing a binary can reset macOS automation grants), re-run
  `things-index worker --setup` from the Mac's GUI session.

Back up `/var/lib/things-index/queue.sqlite` on the server and the worker's
journal before upgrades that change their schema.

## Verification order

1. Confirm `GET /healthz` through the private route.
2. Start the Mac worker and confirm it remains in its long poll.
3. Call the MCP endpoint with MCP Inspector using the public token.
4. Create a disposable Inbox task and verify its temporary title was replaced.
5. Test an exact project, area, and heading, plus This Evening, a reminder,
   deadline, tags, and checklist.
6. Verify the public path explicitly — with split-horizon DNS a LAN `curl`
   only exercises the proxy. Pin the request to the Cloudflare edge:

   ```sh
   EDGE=$(dig +short things-index.example.com @1.1.1.1 | head -1)
   curl -s -o /dev/null -w '%{http_code}\n' \
     --resolve "things-index.example.com:443:${EDGE}" \
     https://things-index.example.com/mcp       # expect 401: reachable, auth enforced
   curl -s -o /dev/null -w '%{http_code}\n' \
     --resolve "things-index.example.com:443:${EDGE}" \
     https://things-index.example.com/healthz   # expect 404: not published
   ```

7. Configure Pebble only after those checks pass.
