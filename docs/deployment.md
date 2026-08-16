# Deployment profiles

ThingsIndex uses the same server and worker protocol in both supported
deployment profiles:

- [Mac-only](macos-only.md): the server and worker run on one always-on Mac;
- [Homelab](homelab.md): the server runs on Linux and the worker runs on a Mac.

The Mac worker and bundled `ThingsIndex Helper` Shortcut are always required
because Things automation is available only on macOS. The worker must run in
the GUI session of the user who owns the Things library. A locked session is
fine, but the user must log in after a reboot.

Before starting the background worker, run `things-index-worker --setup` in
that GUI session. Its temporary loopback-only page installs the correctly named
bundled Shortcut and verifies Things access; it is not another persistent
service. macOS may request separate one-time grants for the helper's external
dictionary input and Things actions. Choose **Always Allow** during setup;
replacing the Shortcut resets those grants.

Both profiles require a public HTTPS route from Pebble Cloud to exactly `/mcp`.
The worker API under `/worker/` must remain local or private.

## Shared security requirements

- Generate different public and worker bearer tokens with at least 32 random
  characters each.
- Terminate public TLS at a tunnel or reverse proxy; the server deliberately
  listens only on loopback or a private IP.
- Publish only `/mcp`. Do not publish `/worker/` or `/healthz`.
- Keep token-bearing environment files and LaunchAgent plists out of source
  control and readable only by their service account.
- Back up the server queue and Mac delivery journal before upgrades that change
  their schema.

Generate suitable hexadecimal tokens with:

```sh
openssl rand -hex 32
openssl rand -hex 32
```

Use one value as `THINGS_INDEX_PUBLIC_TOKEN` and the other as
`THINGS_INDEX_WORKER_TOKEN`.

## Optional status dashboard

Set `THINGS_INDEX_DASHBOARD_TOKEN` to a third independent token of at least 32
characters to enable `GET /dashboard`. Leave it empty to keep the route
disabled. The browser credentials are:

```text
username: things-index
password: <THINGS_INDEX_DASHBOARD_TOKEN>
```

The read-only page shows the newest 100 retained jobs with their creation time,
structured task title and destination, sent-to-Mac state, confirmation state,
attempt count, warnings, and errors. It refreshes every ten seconds.

Keep `/dashboard` on loopback or a private HTTPS route. It must not be included
in the public Pebble ingress, and its token must differ from both API tokens.

## Retention

Cleanup runs at startup and every six hours. Retention is configured as whole
numbers of days:

| Setting | Default | Applies to |
|---|---:|---|
| `THINGS_INDEX_SUCCEEDED_RETENTION_DAYS` | 7 | Confirmed server jobs |
| `THINGS_INDEX_FAILED_RETENTION_DAYS` | 30 | Terminally failed server jobs |
| `THINGS_INDEX_JOURNAL_RETENTION_DAYS` | 30 | Mac deliveries already acknowledged by the server |

Set a value to `0` to retain that category indefinitely. Queued jobs, active or
expired leases, retryable failures, and incomplete Mac delivery records are
never removed by retention cleanup.
