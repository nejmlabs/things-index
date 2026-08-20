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
port publishing to work.

Build the server natively on Linux and install it as
`/usr/local/bin/things-index-server`. The example files under `deploy/systemd/`
use a dedicated `things-index` service account and store the durable queue under
`/var/lib/things-index/`.

Set distinct public and worker tokens in `/etc/things-index/server.env`, make
the file root-owned with mode `0600`, and restrict the host firewall so port
8080 accepts traffic only from the reverse proxy.

The example environment file retains successful jobs for seven days and failed
jobs for thirty days. See [Deployment profiles](deployment.md) for retention
settings and cleanup guarantees.

## Reverse proxy

The generic Traefik file under `deploy/traefik/` demonstrates separate routes:

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
doing exactly that, with no inbound ports on the LAN at all.

## Mac worker

The supported path is the setup wizard, run in the logged-in Mac GUI session:

```sh
things-index worker --setup
```

It verifies the server URL and worker token against the live server, validates
the optional Things auth token with one disposable test task, installs the
bundled ThingsIndex Helper shortcut and settles its privacy dialogs, installs
the LaunchAgent, and waits for the daemon to record its one-time Things 3
automation consent. Approve each macOS dialog with **Always Allow** during this
deliberate setup run.

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

## Verification order

1. Confirm `GET /healthz` through the private route.
2. Start the Mac worker and confirm it remains in its long poll.
3. Call the MCP endpoint with MCP Inspector using the public token.
4. Create a disposable Inbox task and verify its temporary title was replaced.
5. Test an exact project, area, and heading, plus This Evening, a reminder,
   deadline, tags, and checklist.
6. Configure Pebble only after those checks pass.
