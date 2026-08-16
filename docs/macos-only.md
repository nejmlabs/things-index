# Mac-only deployment

This profile runs both ThingsIndex processes on one always-on Intel or Apple
silicon Mac:

```text
Pebble Cloud ──HTTPS──> tunnel or reverse proxy ──> 127.0.0.1:8080/mcp
                                                          ▲
                                                          │ HTTP loopback
                                                          │
Things <── native Shortcut <── Mac worker <────────────────┘
```

The public ingress must route exactly `/mcp`. The worker API and health endpoint
must not be published.

## Requirements

- Things 3 installed for the macOS user running the worker;
- the `ThingsIndex Helper` Shortcut installed and approved for Things access;
- that user logged into the GUI session after each reboot;
- a public HTTPS tunnel or reverse proxy that can restrict the published path;
- two independent bearer tokens as described in [Deployment profiles](deployment.md).

## Build and install

Build both binaries on the target Mac so SQLite is compiled for its native
architecture:

```sh
mkdir -p dist
go build -trimpath -o dist/things-index-server ./cmd/things-index-server
go build -trimpath -o dist/things-index-worker ./cmd/things-index-worker
```

Create the runtime directories and install the binaries:

```sh
mkdir -p "$HOME/.local/bin"
mkdir -p "$HOME/Library/Application Support/ThingsIndex"
mkdir -p "$HOME/Library/Application Support/ThingsIndex/HelperRequests"
mkdir -p "$HOME/Library/Logs/ThingsIndex"
mkdir -p "$HOME/Library/LaunchAgents"
cp dist/things-index-server "$HOME/.local/bin/things-index-server"
cp dist/things-index-worker "$HOME/.local/bin/things-index-worker"
chmod 0755 "$HOME/.local/bin/things-index-server"
chmod 0755 "$HOME/.local/bin/things-index-worker"
```

Copy the two service templates:

```sh
cp deploy/launchd/com.nejmlabs.things-index-server.plist.example \
  "$HOME/Library/LaunchAgents/com.nejmlabs.things-index-server.plist"
cp deploy/launchd/com.nejmlabs.things-index-worker.plist.example \
  "$HOME/Library/LaunchAgents/com.nejmlabs.things-index-worker.plist"
chmod 0600 "$HOME/Library/LaunchAgents/com.nejmlabs.things-index-server.plist"
chmod 0600 "$HOME/Library/LaunchAgents/com.nejmlabs.things-index-worker.plist"
```

In both copied plists:

1. replace `REPLACE_ME` with the macOS account's short username;
2. use the same `REPLACE_WITH_WORKER_TOKEN` value;
3. put the separate public token in the server plist; and
4. replace the worker's `REPLACE_WITH_SERVER_URL` with
   `http://127.0.0.1:8080`.

Plain HTTP is accepted only for a literal loopback IP. The worker rejects HTTP
for hostnames, LAN addresses, and public addresses.

The templates retain successful server history for seven days and failed or
confirmed-delivery history for thirty days. Adjust the three retention values
as described in [Deployment profiles](deployment.md), or set one to `0` to keep
that category indefinitely.

To enable the optional status page, put a third random token in the server
plist's empty `THINGS_INDEX_DASHBOARD_TOKEN` value. Open
`http://127.0.0.1:8080/dashboard` locally and authenticate as `things-index`
with that token. Leave the value empty to keep the route disabled. The public
ingress path restriction below deliberately excludes the dashboard.

## First permission check

Validate the plists and start the server:

```sh
plutil -lint "$HOME/Library/LaunchAgents/com.nejmlabs.things-index-server.plist"
plutil -lint "$HOME/Library/LaunchAgents/com.nejmlabs.things-index-worker.plist"
launchctl bootstrap "gui/$(id -u)" \
  "$HOME/Library/LaunchAgents/com.nejmlabs.things-index-server.plist"
```

Before loading the worker as a background agent, launch its setup GUI:

```sh
"$HOME/.local/bin/things-index-worker" --setup
```

Use **Install Shortcut**, choose **Add Shortcut** in Shortcuts, then use
**Verify Access** and **Test Capture**. Choose **Always Allow** for the
deliberate first-use prompts. Finish setup only after the labelled Inbox test
task has been created and finalised. The loopback setup page then exits; it is
not a third background process.

Next, run the worker once in Terminal with the same environment values from the
plist and stop it after it logs that it is ready and polling.

Then load the worker:

```sh
launchctl bootstrap "gui/$(id -u)" \
  "$HOME/Library/LaunchAgents/com.nejmlabs.things-index-worker.plist"
launchctl print "gui/$(id -u)/com.nejmlabs.things-index-server"
launchctl print "gui/$(id -u)/com.nejmlabs.things-index-worker"
```

Logs are stored under `~/Library/Logs/ThingsIndex/`. The queue and crash-recovery
journal are stored under `~/Library/Application Support/ThingsIndex/`.

## Public ingress

Configure the chosen tunnel or reverse proxy with:

```text
public URL:  https://things-index.example.com/mcp
origin:      http://127.0.0.1:8080
path match:  exactly /mcp
```

Configure Pebble with the full public `/mcp` URL, Streamable HTTP transport, and
the authorization value:

```text
Bearer <THINGS_INDEX_PUBLIC_TOKEN>
```

Do not configure the ingress until its path restriction has been verified.

## Verification order

1. Confirm the server and worker LaunchAgents remain running.
2. Confirm the worker log shows its long poll without authentication errors.
3. Call `/mcp` with MCP Inspector through the public HTTPS endpoint.
4. Create a disposable Inbox task and verify its temporary title was replaced.
5. Test an exact project, area, and heading, plus This Evening, a reminder,
   deadline, tags, and checklist.
6. Configure Pebble only after those checks pass.
