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
`0.0.0.0`, `[::]`, hostnames, and public IP addresses.

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
only `/mcp`.

## Mac worker

Build `things-index-worker` natively on the Mac. Copy and fill in
`deploy/launchd/com.nejmlabs.things-index-worker.plist.example` with:

```text
THINGS_INDEX_SERVER_URL=https://things-index-worker.internal.example.com
THINGS_INDEX_WORKER_TOKEN=<independent worker token>
THINGS_INDEX_JOURNAL_PATH=<user-owned path>/journal.db
THINGS_INDEX_HELPER_TEMP_DIR=<user-owned path>/HelperRequests
THINGS_INDEX_JOURNAL_RETENTION_DAYS=30
```

The worker requires HTTPS because its server is not on loopback. Before loading
the LaunchAgent, run `things-index-worker --setup` in the logged-in Mac GUI
session. Use **Install Shortcut**, **Verify Access**, and **Test Capture**;
choose **Always Allow** for the deliberate first-use prompts. Finish only after
the labelled test task is created and finalised. The setup page then exits.
Routine captures must not prompt, and the normal worker accepts no inbound
connections.

## Verification order

1. Confirm `GET /healthz` through the private route.
2. Start the Mac worker and confirm it remains in its long poll.
3. Call the MCP endpoint with MCP Inspector using the public token.
4. Create a disposable Inbox task and verify its temporary title was replaced.
5. Test an exact project, area, and heading, plus This Evening, a reminder,
   deadline, tags, and checklist.
6. Configure Pebble only after those checks pass.
