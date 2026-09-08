# ThingsIndex

Connect **Things 3** to **Pebble Index 01**, **Claude Desktop**, **Codex**, and
other AI clients through the Model Context Protocol (MCP). Capture tasks by
voice, find what needs doing, and organise tasks, projects, areas, tags, and
headings.

Built primarily for Index 01's voice workflow, so task capture works from a
single spoken request without follow-up questions. Things runs on your Mac;
you can connect a local AI client directly, or run a server and queue for remote
access.

[What's new](#whats-new-in-the-latest-release) · [Setup](#setup) ·
[What you can do](#what-you-can-do) · [Updating](#updating) ·
[Roadmap](#roadmap) · [Documentation](#documentation)

## What's new in the latest release

**[v0.2.6](https://github.com/nejmlabs/things-index/releases/tag/v0.2.6)** —
7 September 2026

- **Create areas and tags from MCP**, including tags under an existing parent.
- **Recover interrupted writes more reliably.** Saved write records and retry
  keys help recover confirmed results without repeating the same change.
- **Check results more thoroughly**, including project details, checklists,
  and dates. Today, Anytime, and Someday use Things' public lists.
- **Simpler Mac worker setup.** The Go installer guides permissions and checks
  startup and restart. Persistent release signing and update identity checks
  help preserve macOS permissions across updates. Python is not required.

**Upgrading:** update the Mac worker before the server. Users moving from
v0.2.5 or earlier need a one-time permission refresh. Heading operations still
use the ThingsIndex Helper Shortcut and need their own initial approvals.
See [updating](#updating) and the [full release notes](https://github.com/nejmlabs/things-index/releases/tag/v0.2.6).

## What you can do

| In Things | Available through ThingsIndex | Interface |
| --- | --- | --- |
| Tasks | Capture, search, edit, schedule, complete, cancel, and move to Trash. Include reminders when capturing; append checklist lines when updating. | URL scheme and AppleScript for changes; read-only SQLite for details. |
| Projects | List, create, complete, and cancel. Capture tasks into an existing project using **fuzzy matching** on a spoken project name. | URL scheme for creation; AppleScript for archiving; read-only SQLite for listing. |
| Areas and tags | Create areas and tags, including tags under an existing parent. | AppleScript. |
| Headings | Create a heading inside an existing project, rename it, and mark it completed/archived. | Helper Shortcut using native Things actions. |
| Lists and status | Read Today and Inbox, search other scopes, and check whether a queued request has finished. | AppleScript and read-only SQLite for Things lists; ThingsIndex's queue for request status. |

For example, ask your connected client:

- “What's in my Today list?”
- “Add buy paint to the kitchen project.”
- “Create a project called Kitchen renovation.”
- “Create a tag called Waiting under Work.”

For task capture, project matching handles a unique partial name or a small
spelling mistake. If the requested project is missing or ambiguous, the task goes to
Inbox with the requested destination recorded in its notes. The response
explains what happened; Index 01 does not need to present choices. Creating a
new project is a separate, explicit request.

See the [complete MCP tool reference](docs/mcp-tools.md) for all **15 tools**
(**14 in direct stdio mode**), each tool's interface, result limits, examples,
matching rules, and Things auth-token requirements.

<a id="-quick-start--onboarding"></a>

## Setup

Choose the path that matches where your MCP client runs:

| Your setup | Start here |
| --- | --- |
| Index 01 with a Mac mini and a Linux, Proxmox, or Docker server | [Index 01 / homelab setup](#index-01--homelab-setup) |
| Claude Desktop or Cursor on the same Mac as Things | [Local Mac setup](#local-mac-setup) |
| Remote access with the server and worker both on one Mac | [Advanced Mac-only deployment](docs/macos-only.md) |

**Before you start:** install and open Things 3.17+ on macOS 14+. Run Mac setup
commands in Terminal as the user who owns the Things library, **without
`sudo`**, with the desktop visible for permission approvals.

For unattended operation, the Mac must stay awake and that user must remain
logged in. The screen can be locked after setup; after a reboot, log in again
to start the worker.

<a id="option-2-homelab--distributed-mode-proxmox--docker--mac-mini"></a>

### Index 01 / homelab setup

The **server** receives MCP requests and keeps queued work on Linux. The
**Mac worker** connects to that server and performs the work in Things.

#### 1. Install the server

Choose **one** of these methods.

**Proxmox:** run in the **Proxmox host's root shell**:

```bash
bash -c "$(wget -qLO - https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/proxmox-install.sh)"
```

The default opens the server to the LAN. The installer's final banner explains
how to restrict access to your reverse proxy. To restrict it from the start,
see the [Proxmox network options](deploy/README.md#proxmox-network-options).

**Docker Compose:** run on the **Docker host**:

```bash
git clone https://github.com/nejmlabs/things-index.git
cd things-index
umask 077
make tokens > .env
docker compose up -d
```

Run token generation once for a new installation. Keep `.env` private: it
contains the credentials used to connect your clients and Mac worker.

#### 2. Configure the connection

Set up a reverse proxy or tunnel with two separate routes:

| Connection | What to use |
| --- | --- |
| Index 01 / Pebble Cloud → MCP server | Public HTTPS URL ending in `/mcp`, with `THINGS_INDEX_PUBLIC_TOKEN`. |
| Mac worker → server | Private HTTPS base URL, with `THINGS_INDEX_WORKER_TOKEN`. |

Publish only `/mcp`; keep the worker API, health endpoint, and optional dashboard
private. The Mac worker rejects plain HTTP except to a loopback address.

Follow the [network setup guide](deploy/README.md), with worked examples for
[Traefik](deploy/traefik/README.md) and
[Cloudflare Tunnel](deploy/cloudflare/README.md).
[Internal DNS and the full setup sequence](docs/homelab.md) are covered in the
homelab guide. An [SSH tunnel](docs/homelab.md#no-domain-alternative-persistent-ssh-tunnel)
is available for LAN-only use, but does not make the server reachable by Pebble Cloud.

#### 3. Install the Mac worker

Run in **Terminal on the Mac that runs Things**:

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/mac-worker-install.sh)"
```

Have the private worker **base URL** and worker token ready. The optional Things
auth token enables additional task updates; see
[where to find each setup value](docs/homelab.md#mac-worker).

The installer downloads the signed release to `~/.local/bin/things-index`,
checks its signature, and opens the Go setup wizard. Follow its Full Disk
Access and Things Automation steps, add the bundled **ThingsIndex Helper**
Shortcut, and wait for the startup and restart checks to pass.

**Before leaving the Mac unattended**, complete the
[one-time heading-permission walkthrough](shortcuts/README.md#first-run-verification).
The wizard's basic Shortcut check does not prove permission to create, rename,
or archive headings; those actions can need separate approvals.

#### 4. Verify and connect Index 01

Follow the [verification checklist](docs/homelab.md#verification-order),
including a test task and the background heading checks. Then configure
Index 01 with the **public `/mcp` URL and public token**.

A queued response means the Mac has not yet confirmed the operation. The client
should check `things_capture_status` until it reports success or failure.

<a id="option-1-all-in-one-local-mac-mode"></a>

### Local Mac setup

For Claude Desktop or Cursor running on the same Mac as Things, run:

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/mac-install.sh)"
```

Choose the option to print the client configuration, then paste it into your
MCP client's settings. The installer can also start a local HTTP MCP server
that stays running in Terminal. GitHub build provenance is checked when an
authenticated `gh` CLI is available.

Restart your MCP client, ask for your Today list, and create a disposable Inbox
task. Approve any requested Things Automation or data access while you are at
the Mac, and confirm both results in Things. This local path does not run the
worker setup wizard. Remove the test task once the check passes.

To enable heading tools, run:

```bash
~/.local/bin/things-index install-shortcut
```

Then complete the [heading-permission walkthrough](shortcuts/README.md#first-run-verification).
The helper is used for heading edits; ordinary task capture uses Things' URL
scheme directly.

## Updating

**For a server-and-worker installation, update the Mac worker first.**

On the Mac:

```bash
~/.local/bin/things-index update
```

Then update the server using the method you installed with:

- **Proxmox:** run this on the Proxmox host:

  ```bash
  bash -c "$(wget -qLO - https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/proxmox-update.sh)"
  ```

- **Docker Compose:** run in the server's repository checkout:

  ```bash
  git pull
  docker compose up -d --build
  ```

- **Manual installation:** follow the [upgrade guide](docs/homelab.md#upgrading).

For v0.2.5 or earlier, follow the [signing and permission migration guide](docs/macos-signing.md).
Stable signing helps preserve grants across updates; it does not guarantee
that future macOS or Things updates will never request consent again.

## How it works

ThingsIndex is written in **Go**. Most changes use Things' URL scheme or
AppleScript. The helper Shortcut handles creating headings in existing
projects, renaming headings, and archiving them.

ThingsIndex reads item details from Things' SQLite database **without writing
to it**. Today, Anytime, and Someday membership comes from Things' public lists.
Native Things automation performs changes and leaves syncing to Things.

In server mode, requests wait in a durable queue while the Mac is unavailable.
The worker processes them when it reconnects. Data returned through MCP is
shared with the AI client and provider you choose.

### Write recovery

All write tools accept an optional `idempotency_key`. Retry a command using the
**same key and identical input**; use a new key for a new intentional change.
Saved write records help recover results after interruptions. An uncertain
write may require checking the item in Things before retrying.

See the [write recovery guide](docs/write-recovery.md) for the full guarantees,
limitations, deadlines, and retention rules.

## Roadmap

Future work, roughly in priority order. Scope and timing may change.

- [ ] **Guided setup and permission checks.** Bring the heading create,
  rename, and archive checks into setup, with clear results for each capability
  and guidance for any remaining attended macOS approvals.
- [ ] **Consistent Mac installers.** Give local and worker installations the
  same signature checks, signing-identity checks, backup protections, and
  permission guidance.
- [ ] **Easier recovery.** Make interrupted or uncertain requests easier to
  inspect, explain what was confirmed in Things, and guide the next action
  without requiring users to inspect databases or interpret raw logs.
- [ ] **More complete reading tools.** Add pagination for larger libraries,
  explicit area and tag listing, and detailed item retrieval.
- [ ] **Optional desktop clarification.** Let conversational clients ask which
  project the user means before capturing a task. Keep Index 01's current
  single-request behaviour and Inbox fallback as the default.

Later candidates include moving existing tasks between projects, areas, and
headings; editing existing project details; and editing individual checklist
items. New operations should use supported Things interfaces, with helper
Shortcuts reserved for functionality that needs them.

## Documentation

| Guide | Covers |
| --- | --- |
| [MCP tools](docs/mcp-tools.md) | All tool names, examples, project matching, and Index 01 behaviour. |
| [Homelab deployment](docs/homelab.md) | Server, network, Mac worker, verification, and upgrades. |
| [Advanced Mac-only deployment](docs/macos-only.md) | Running the server and worker together on one Mac. |
| [Deployment settings](docs/deployment.md) | Tokens, the optional dashboard, and data retention. |
| [Helper Shortcut](shortcuts/README.md) | Installation, heading permissions, and how the helper works. |
| [macOS signing](docs/macos-signing.md) | Release identity and permission migration. |
| [Write recovery](docs/write-recovery.md) | Retry behaviour and interrupted operations. |

## Building from source

Install Go 1.26+ and a C toolchain for SQLite, then run from a repository checkout:

```bash
make build
./bin/things-index config
```

The configuration command prints settings for a local MCP client. Use
`./bin/things-index start` for local HTTP mode. Released Mac binaries do not
require Go, Python, or Xcode to install.

The `cmd/` folder contains the executable entry points. The `internal/` folder
contains the Go packages used by this project. **`internal` controls which Go
code can import those packages; it does not mean confidential or unpublished.**
These source files belong in the public repository so others can build and
review the application.

## Uninstalling

On the Mac, this removes the worker's local configuration, logs, and recovery
state. Back up any state you need before running it:

```bash
~/.local/bin/things-index uninstall
```

Follow the printed manual steps to remove the helper Shortcut, Automation
grant, and executable. Also remove any Full Disk Access grant and the MCP
client configuration you added. Separately installed servers and tunnels need
their own removal.
