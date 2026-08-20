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
   * **Quick test without either**: keep an SSH tunnel open from the Mac (`ssh -N -L 8080:<server-ip>:8080 <user>@<lan-host>`) and use `http://127.0.0.1:8080` in the next step.

   Hostnames also need internal DNS records pointing at the proxy — the full phase-by-phase pathway (server, proxy, DNS, tunnel, worker, verification checklist) is in [`docs/homelab.md`](docs/homelab.md).

3. **Connect the Mac Worker (One Command)**:
   ```bash
   bash -c "$(curl -fsSL https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/mac-worker-install.sh)"
   ```
   This downloads the latest released universal binary (Apple Silicon + Intel) to `~/.local/bin`, verifies its GitHub build-provenance attestation when the `gh` CLI is present (`gh attestation verify ~/.local/bin/things-index --repo nejmlabs/things-index` by hand otherwise), and launches the setup wizard, which:
   * Verifies the server connection **and** the worker token before installing anything.
   * Validates your optional Things auth token with a disposable test task (the token unlocks deadline/tag/checklist updates).
   * Auto-detects the Things 3 SQLite database and verifies read-only connectivity.
   * Installs the bundled **ThingsIndex Helper** shortcut and settles its one-time privacy dialogs.
   * Installs a launchd LaunchAgent that starts at login, auto-restarts the worker if it crashes, and logs to `~/Library/Logs/ThingsIndex/`.
   * Walks you through the two one-time macOS permission dialogs (data access + Things automation) so background operation stays prompt-free.

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
| `update_things_task` | Write | Update an open task’s title or notes, reschedule it, or add deadlines, tags, and checklists (deadline/tags/checklist/non-today schedules need the Things auth token). |
| `create_things_heading` | Write | Create a new section heading inside an existing project (runs the bundled ThingsIndex Helper shortcut; idempotent, verified via SQLite before reporting success). |
| `rename_things_heading` | Write | Rename an existing section heading inside a project (runs the bundled ThingsIndex Helper shortcut; verified via SQLite before reporting success). |
| `archive_things_heading` | Write | Archive a section heading from an active project (runs the bundled ThingsIndex Helper shortcut; verified via SQLite before reporting success). |
| `archive_things_task` | Write | Archive a task: mark `completed` (Logbook), `canceled` (Logbook), or move to `trash`. |
| `archive_things_project` | Write | Archive an entire project: mark `completed` or `canceled`. |
| `things_capture_status` | Read | Check async status of any queued operation via `request_id` (server mode only; stdio mode captures synchronously). |

---

## 🔒 Aims
* **Zero-Prompt Execution**: Uses the official Things URL Scheme (`things:///add`, `add-project`, `update`) & read-only SQLite preflights. Task/project archiving falls back to AppleScript, which triggers macOS's one-time Automation permission grant on first use. Heading operations run Things' native App Intents through the bundled signed **ThingsIndex Helper** shortcut — the only automation surface that reaches headings — after its one-time install and privacy grant (see `shortcuts/README.md`).
* **Zero Foreground Steal**: Suppresses window focus and automatically quits Things 3 (no Dock dot) if it was closed before capture.
* **Strictly Read-Only SQLite (`_query_only=1`)**: Never performs raw SQL writes to Cultured Code's database; Cultured Code's official engine handles writing and Things Cloud sync.
* **Durable Queue**: In server mode, If the Mac is asleep or rebooting, tasks wait safely in the server queue and process immediately on wakeup.

---

## 🗑️ Clean Uninstallation
To completely remove all daemons, databases, crontab entries, and launcher scripts:
```bash
things-index uninstall
```
