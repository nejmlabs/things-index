# ThingsIndex

ThingsIndex is a production-grade, zero-prompt Model Context Protocol (MCP) server for capturing, reading, searching, updating, and archiving tasks from **Pebble Index 01**, **Claude Desktop**, **Cursor**, and AI agents into **Things 3** on macOS.

---

## ⚡ Quick Start & Onboarding

### Option 1: All-in-One Local Mac Mode
For running directly on a Mac for local Claude Desktop or Cursor:

```bash
# 1. Build the binary
make build

# 2. Get ready-to-paste Claude Desktop / Cursor stdio configuration:
./bin/things-index config

# 3. Or start a local Streamable HTTP MCP server:
./bin/things-index start
```

---

### Option 2: Homelab / Distributed Mode (Proxmox / Docker + Mac mini)
For 24/7 homelab infrastructure where the MCP server runs on Linux and leases jobs to a remote Mac:

1. **Deploy Server on Linux / Proxmox**:
   * **Proxmox VE 1-Click LXC Installer**:
     ```bash
     bash -c "$(wget -qLO - https://raw.githubusercontent.com/nejmlabs/things-index/main/deploy/proxmox-install.sh)"
     ```
   * **Docker Compose** (the server refuses to start without tokens):
     ```bash
     make tokens > .env   # or set the three THINGS_INDEX_*_TOKEN vars yourself
     docker compose up -d
     ```

2. **Connect the Mac Worker (10-Second Setup Wizard)**:
   ```bash
   things-index worker --setup
   ```
   * Auto-detects Things 3 SQLite database.
   * Verifies read-only connectivity.
   * Installs and starts the background daemon automatically with `@reboot` persistence.

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

## 🔒 Key Guarantees
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
