# ThingsIndex

ThingsIndex is a Model Context Protocol (MCP) server for capturing, reading, searching, updating, and archiving tasks from **Pebble Index 01**, **Claude Desktop**, **Cursor**, and AI agents into **Things 3** on macOS.

---

## ⚡ Quick Start & Onboarding

### Option 1: All-in-One Local Mac Mode
For running directly on a Mac for local Claude Desktop or Cursor:

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/mac-install.sh)"
```

This downloads the attested universal binary to `~/.local/bin` and ends with a choice: print the ready-to-paste Claude Desktop / Cursor stdio configuration, or start the local Streamable HTTP MCP server right away. Run `things-index install-shortcut` once to enable the heading tools in local mode (the worker wizard does this automatically in homelab mode).

Building from source instead: `make build`, then `./bin/things-index config` or `./bin/things-index start`.

---

### Option 2: Homelab / Distributed Mode (Proxmox / Docker + Mac mini)
For 24/7 homelab infrastructure where the MCP server runs on Linux and leases jobs to a remote Mac:

1. **Deploy Server on Linux / Proxmox**:
   * **Proxmox VE 1-Click LXC Installer**:
     ```bash
     # Open to the LAN (the final banner prints the commands to tighten it later):
     bash -c "$(wget -qLO - https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/proxmox-install.sh)"

     # Or locked to your reverse proxy from the start (IPv4 or CIDR, validated up front):
     THINGS_INDEX_PROXY_IP=<proxy-ip> bash -c "$(wget -qLO - https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/proxmox-install.sh)"
     ```
   * **Docker Compose** (the server refuses to start without tokens):
     ```bash
     make tokens > .env   # or set the three THINGS_INDEX_*_TOKEN vars yourself
     docker compose up -d
     ```

2. **Put HTTPS in front** (the worker refuses plain HTTP off loopback):
   Any reverse proxy or tunnel satisfying the contract in [`deploy/README.md`](deploy/README.md) works. Two worked examples ship in the repo:
   * [`deploy/traefik/`](deploy/traefik/) — inbound LAN reverse proxy (fill-in config + checklist).
   * [`deploy/cloudflare/`](deploy/cloudflare/) — outbound Cloudflare Tunnel runbook: publishes only `/mcp`, no inbound ports on your network.
   * **No domain at all**: an SSH tunnel to loopback also satisfies the worker — one-off for a quick test (`ssh -N -L 8080:<server-ip>:8080 <user>@<lan-host>`), or persistent across reboots via the shipped launchd template ([`deploy/launchd/com.nejmlabs.things-index-tunnel.plist.example`](deploy/launchd/com.nejmlabs.things-index-tunnel.plist.example), walkthrough in [`docs/homelab.md`](docs/homelab.md)). Use `http://127.0.0.1:8080` in the next step. LAN-only: nothing is published for Pebble Cloud.

   Hostnames also need internal DNS records pointing at the proxy — the full phase-by-phase pathway (server, proxy, DNS, tunnel, worker, verification checklist) is in [`docs/homelab.md`](docs/homelab.md).

3. **Connect the Mac Worker (One Command)**:
   ```bash
   bash -c "$(curl -fsSL https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/mac-worker-install.sh)"
   ```
   This downloads the latest released universal binary (Apple Silicon + Intel) to `~/.local/bin`, verifies its GitHub build-provenance attestation when the `gh` CLI is present (`gh attestation verify ~/.local/bin/things-index --repo nejmlabs/things-index` by hand otherwise), and launches the setup wizard, which:
   * Verifies the server connection **and** the worker token before configuring the background worker.
   * Validates your optional Things auth token with a disposable test task (the token unlocks deadline/tag/checklist updates).
   * Auto-detects the Things 3 SQLite database and verifies read-only connectivity.
   * Installs the bundled **ThingsIndex Helper** shortcut and checks basic input and Things lookup access. Heading writes may need separate first-use approval; verify them through the background worker during attended setup before unattended use.
   * Installs a launchd LaunchAgent that starts at login, auto-restarts the worker if it crashes, and logs to `~/Library/Logs/ThingsIndex/`.
   * Checks the worker's signing identity and guides **Full Disk Access** setup before querying Things or starting the worker. It opens the settings and shows the actual executable to add; an old entry may need to be removed and added again.
   * Verifies fresh database access and Things Automation consent through the background worker, then repeats startup after a restart. A failed check leaves the worker stopped and reports setup as incomplete. macOS still requires you to approve the initial permissions; see [Mac worker permissions](docs/homelab.md#mac-worker).

   Releases through v0.2.5 use ad hoc signing, which can invalidate permissions after an update. The release workflow now requires a persistent signing certificate, and the updater checks the certificate and identity requirements before replacing an installed signed build. Moving to the first signed release still needs a manual permission refresh; see [release signing and migration](docs/macos-signing.md). Prompt-free operation across an update must be verified on the Mac after this migration.

4. **Updating** — both halves update with one command:
   * **Server** (on the Proxmox host — finds the `things-index` container, pulls, rebuilds, restarts):
     ```bash
     bash -c "$(wget -qLO - https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/proxmox-update.sh)"
     ```
   * **Mac worker** (self-update: downloads the latest release, verifies its provenance attestation, swaps the binary, restarts the agent):
     ```bash
     things-index update
     ```

---

## 🛠️ Complete MCP Tools Directory

| Tool Name | Type | Description |
| :--- | :---: | :--- |
| `get_things_today` | Read | Returns all tasks currently scheduled for **Today** with area, project, heading, and notes. |
| `get_things_inbox` | Read | Returns all unprocessed tasks in the **Inbox**. |
| `list_things_projects` | Read | Lists all active **Projects**, their parent Areas, notes, and open task counts. |
| `search_things_tasks` | Read | Search tasks across any scope (`today`, `inbox`, `anytime`, `someday`, `all`) by text query, project, area, or tag. |
| `capture_things_task` | Write | Create a task in Inbox, Project, Area, or under a Heading with notes, tags, checklists, deadlines, and reminders. |
| `create_things_project` | Write | Create a new project inside an Area with notes, tags, start schedule, and deadline. |
| `create_things_area` | Write | Create an area, or reuse one unique exact name (ignoring case) with a warning. |
| `create_things_tag` | Write | Create a tag with an optional existing parent tag; reuse requires the same parent. |
| `update_things_task` | Write | Update an open task’s title or notes, reschedule it, or add deadlines, tags, and checklists (deadline/tags/checklist/non-today schedules need the Things auth token). |
| `create_things_heading` | Write | Create a new section heading inside an existing project (runs the bundled ThingsIndex Helper shortcut; verified via SQLite before reporting success). |
| `rename_things_heading` | Write | Rename an existing section heading inside a project (runs the bundled ThingsIndex Helper shortcut; verified via SQLite before reporting success). |
| `archive_things_heading` | Write | Archive a section heading from an active project (runs the bundled ThingsIndex Helper shortcut; verified via SQLite before reporting success). |
| `archive_things_task` | Write | Archive a task: mark `completed` (Logbook), `canceled` (Logbook), or move to `trash`. |
| `archive_things_project` | Write | Archive an entire project: mark `completed` or `canceled`. |
| `things_capture_status` | Read | Check async status of any queued operation via `request_id` (server mode only; stdio mode captures synchronously). |

### Project capture from Index 01

Index 01 handles each spoken command without a clarification exchange. Say
“Add buy paint to the kitchen project” to capture a task, or explicitly ask
“Create a project called Kitchen renovation” to create a project.

`create_things_project` creates the requested title in an optional exact area.
It reuses an active exact-name project only in that same area; omitting the area
means an unfiled project. If requested notes, tags, or dates differ on a reused
project, it returns a warning describing which fields were not applied. After
creation succeeds, the returned `things_id` can
be used directly in a task's destination:

```json
{"title":"Buy paint","destination":{"kind":"project","id":"<returned things_id>"}}
```

For an existing project, `capture_things_task` accepts its name as spoken:

```json
{"title":"Buy paint","destination":{"kind":"project","name":"kitchen"}}
```

Project matching prefers exact names, then handles punctuation, reordered
words, a unique whole-word partial name, or a small typo in the full name
(such as “Kichen renovation”). Only active projects are considered. A matching
notice names the selected project so the voice client can confirm it. Numeric
differences such as 2025 versus 2026 are not treated as typos.

If “kitchen” could mean both “Kitchen renovation” and “Kitchen supplies”, the
task is saved in Inbox. Its notes retain the requested project and heading,
and the confirmation explains the Inbox fallback. The same applies when no
project matches or an explicit project ID is unavailable. Index 01 does not
need to ask a follow-up question or offer choices, and an uncertain match does
not create a new project.

Explicit project IDs take precedence over names. Area names and heading names
remain exact, and project creation does not use fuzzy matching. If an explicit
project creation request fails, the client reports the failure without asking
for clarification.

In server mode, check `things_capture_status` until a queued project creation
succeeds before adding tasks using its ID. A queued result means the Mac has
not yet confirmed the operation.

Area and tag creation also work without a clarification exchange. For example,
`create_things_area` accepts `{"title":"Work"}`, and `create_things_tag` accepts
`{"title":"Waiting","parent":"Work"}` when the parent tag already exists.
Missing or ambiguous parent names fail before creation. Existing tags are never
moved to a different parent implicitly. Tag names cannot contain commas because
Things' tag application interface uses comma-separated names.

### Write recovery

All write tools accept an optional `idempotency_key`. Reuse the same key and
identical input when retrying a command; use a new key for a new intentional
change. The same key with different input is rejected. Both direct stdio MCP
and the queued Mac worker keep a durable local write journal.

Task capture keeps its unique pending title until its Things UUID is saved in
the journal, then finalises the title. A restart can recover that marker and
preserve any placement or missing-tag warnings. Completed writes retain their
result so a lost server acknowledgement does not repeat the mutation.

Task updates save their intended before/after values before dispatch. Appending
notes becomes an exact replacement computed once; retries check for conflicting
edits. Checklists are appended once and verified against the existing item IDs,
titles, and statuses. An uncertain checklist or native creation is reconciled
where possible and otherwise reported as uncertain without repeating it. Things
does not provide an atomic transaction with this journal, so some interrupted
writes require checking the item in Things. Concurrent edits to the same fields
can still race with the native write.

Ordinary native commands have a 30-second deadline, each job has 45 seconds,
and reporting has a separate 10-second allowance within the 90-second lease.
Initial attended Automation setup retains its two-minute allowance. Date checks
use public Things calendar properties; SQLite remains read-only. The Evening
check also depends on Things' current read-only bucket schema and rejects
unrecognised values.

Only server-acknowledged journal entries are eligible for retention cleanup.
Uncertain writes and unacknowledged results remain available for recovery;
stdio results are retained because stdio has no durable delivery acknowledgement.
Server idempotency protection is bounded by retained queue and journal history.
The existing signed Shortcut remains the implementation for heading operations.

For an upgrade that adds area/tag tools, update the Mac worker before the MCP
server. Older workers cannot execute the new operations. See
[deployment profiles](docs/deployment.md) for signing, setup, and retention.

---

## 🔒 Aims
* **Unattended Execution**: Uses native Things automation for writes and read-only SQLite for item details. Today, Anytime, and Someday membership comes from Things' public lists. Background operation across restarts requires Full Disk Access for the worker, separate Things Automation permission for AppleScript, and the **ThingsIndex Helper** shortcut's permissions for headings. Release signing and updater identity checks preserve the identity used by those grants; existing ad hoc installations need a one-time migration and verification. See [Mac worker permissions](docs/homelab.md#mac-worker).
* **Zero Foreground Steal**: Suppresses window focus and automatically quits Things 3 (no Dock dot) if it was closed before capture.
* **Strictly Read-Only SQLite (`_query_only=1`)**: Never performs raw SQL writes to Cultured Code's database; Cultured Code's official engine handles writing and Things Cloud sync.
* **Durable Queue**: In server mode, If the Mac is asleep or rebooting, tasks wait safely in the server queue and process immediately on wakeup.

---

## 🗑️ Clean Uninstallation
To completely remove all daemons, databases, crontab entries, and launcher scripts:
```bash
things-index uninstall
```
