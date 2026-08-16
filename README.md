# ThingsIndex

ThingsIndex is a small, write-only MCP service for capturing tasks from Pebble
Index 01 into Things on a Mac.

This first phase does not use, modify, or depend on the Things3 Pages Obsidian
plugin. A single `ThingsIndex Helper` Shortcut uses Things' native Shortcuts
actions for prompt-free routine capture, confirmation, and crash recovery.

The Mac worker includes a short-lived setup GUI. Run
`things-index-worker --setup` to install the bundled Shortcut and verify Things
access without assembling actions or entering protocol commands. Its labelled
capture test grants Create and Edit access before the first real request. The
setup page listens only on loopback and stops when setup is complete.

## Deployment profiles

The same binaries support two layouts.

### Mac-only

```text
Pebble Cloud ──HTTPS──> tunnel or reverse proxy ──> Mac server on loopback
                                                          ▲
                                                          │ HTTP loopback
                                                          │
                                                   Mac worker ──> Things
```

This is the simplest option for anyone with an always-on Intel or Apple silicon
Mac. The server and worker run as separate LaunchAgents so the durable queue and
delivery journal retain their existing crash-recovery boundaries.

### Homelab

```text
Pebble Cloud ──HTTPS──> reverse proxy ──> Linux server and durable queue
                                                       ▲
                                                       │ private HTTPS
                                                       │
                                                Mac worker ──> Things
```

This keeps the public service isolated from the Mac and allows captures to wait
while the Mac or Things is temporarily unavailable.

See [Deployment profiles](docs/deployment.md), [Mac-only](docs/macos-only.md),
and [Homelab](docs/homelab.md).

## MCP tools

- `capture_things_task` queues one task and waits briefly for the Mac to create
  it. It returns `created`, `queued`, or `failed` plus a stable `request_id`.
- `things_capture_status` checks a previous `request_id`.

An optional, independently authenticated `/dashboard` shows authoritative
sent-to-Mac, processing, retry, failure, and confirmed-in-Things states for
retained jobs. It is disabled unless a separate dashboard token is configured.

Capture supports title, notes, an exact project, area, or project heading,
Anytime/Someday/date, This Evening, reminder time, deadline, existing tags, and
checklist items.

## Dependencies

The binaries use Go's standard library plus:

- the official Model Context Protocol Go SDK; and
- `go-sqlite3`, which embeds SQLite and requires CGO when building.

The required Shortcut adds no Go dependency. Its installable, Apple-signed
artifact, readable source, versioned protocol, and maintainer rebuild notes are
under [`shortcuts/`](shortcuts/README.md).

## Build and test

Build on the target operating system so CGO uses the correct C toolchain:

```sh
go test ./...
go build -trimpath -o dist/things-index-server ./cmd/things-index-server
go build -trimpath -o dist/things-index-worker ./cmd/things-index-worker
```

The worker is macOS-only at runtime because it controls Things. The server runs
on macOS for the Mac-only profile or Linux for the homelab profile.

## Security properties

- Public MCP and worker APIs require different bearer tokens of at least 32
  characters.
- The server refuses wildcard and public-interface listen addresses.
- Worker HTTP is accepted only for a literal loopback IP; every remote worker
  connection requires HTTPS.
- Public and worker routes use separate URL prefixes so an ingress can publish
  only `/mcp`.
- Request bodies, headers, and task fields have explicit limits.
- Dynamic values are exchanged with the Shortcut in private JSON request files;
  they are never interpolated into shell source.
- Queue and delivery state use SQLite WAL with full synchronous durability.
- Configurable retention removes only terminal server jobs and Mac deliveries
  that the server has already acknowledged.
