# MCP tools and Index 01 behaviour

[Back to the README](../README.md)

HTTP/server mode exposes **15 tools**. Direct stdio mode exposes the same tools
except `things_capture_status` (**14 tools**), because it returns results
synchronously. In server mode, reads as well as writes can return queued;
use the status tool with their `request_id` to retrieve the eventual result.

The **Interface** column identifies what reads the data or makes the change in
Things. Write tools also perform lookups and verification using read-only
SQLite or AppleScript. ThingsIndex never writes to Things' SQLite database.

| Tool | Type | Description | Interface |
| :--- | :---: | :--- | :--- |
| `get_things_today` | Read | Return up to 50 open tasks in Today, with project, area, heading, and notes where available. | AppleScript for Today membership; read-only SQLite for task details. |
| `get_things_inbox` | Read | Return up to 50 open Inbox tasks. | Read-only SQLite. |
| `list_things_projects` | Read | Return up to 50 active projects, with their areas, notes, and open task counts. | Read-only SQLite. |
| `search_things_tasks` | Read | Search task titles, notes, and project titles; filter by scope, project, area, or tag. Can also list projects. See [search options](#search-options-and-limits). | Read-only SQLite; AppleScript also supplies Today, Anytime, and Someday membership. |
| `capture_things_task` | Write | Create a task with notes, existing tags, checklist lines, schedule, deadline, and reminder. Place it in Inbox, an existing project or area, or an existing project heading. **Fuzzy project matching** handles clear partial names and small typos; missing or ambiguous projects fall back to Inbox with a warning. | URL scheme `add`; final title via URL scheme `update` with a Things auth token, otherwise AppleScript. |
| `create_things_project` | Write | Create a project with an optional existing area, notes, existing tags, start schedule, and deadline. Reuse a unique exact title in the same area, with warnings for differing fields. | URL scheme `add-project`. |
| `create_things_area` | Write | Create an area, or reuse a unique exact name ignoring case, with a warning. | AppleScript. |
| `create_things_tag` | Write | Create a tag, optionally under an existing parent tag. Reuse requires the same parent. | AppleScript. |
| `update_things_task` | Write | Change an open task's title, replace or append notes, set its schedule or deadline, add existing tags, or append checklist lines. See [update limits](#task-update-options-and-limits). | URL scheme `update`; without a Things auth token, AppleScript supports title, notes, and Today only. |
| `create_things_heading` | Write | Create a heading inside an existing project, or reuse an existing matching heading. | Helper Shortcut → Things **Create Heading** action. |
| `rename_things_heading` | Write | Rename an existing heading using its project and heading names. | Helper Shortcut → Things **Edit Items / Title** action. |
| `archive_things_heading` | Write | Mark a heading completed/archived. | Helper Shortcut → Things **Edit Items / Status** action. |
| `archive_things_task` | Write | Use `action: "complete"` (default), `"cancel"`, or `"trash"`. | AppleScript. |
| `archive_things_project` | Write | Use `action: "complete"` (default) or `"cancel"`. | AppleScript. |
| `things_capture_status` | Read | Retrieve the status and available result of any queued operation using its `request_id`. HTTP/server mode only. | ThingsIndex's own queue database; no call to Things. |

Only the **three heading tools** use the helper Shortcut for their changes.
Adding a task under an existing heading uses the URL scheme.

## Search options and limits

`get_things_today`, `get_things_inbox`, and `list_things_projects` each return
up to **50** items. To request more, use `search_things_tasks` with the matching
scope and a `limit` from **1 to 200**. An omitted, zero, negative, or greater-than-200
limit uses 50. There is currently no pagination parameter.

Supported scopes are `today`, `inbox`, `anytime`, `someday`, `all`, and
`projects`. Omitting `scope` currently searches across all task scopes. Specify
it explicitly when you want a particular list:

```json
{"scope":"today","limit":200}
```

For task scopes, `query` searches titles, notes, and project titles. The
`project`, `area`, and `tag` filters match names exactly, ignoring case. Results
exclude Trash and default to open tasks; `include_completed: true` also allows
completed and canceled tasks, subject to the selected scope.

The `projects` scope lists active projects and honours `limit`; it does not
apply the task search filters or `include_completed`.

## Task update options and limits

`update_things_task` uses an ID or an exact task title, optionally narrowed by
project. Title matching here does not use the fuzzy project matching available
when capturing a new task.

Without a Things auth token, updates support a new title, replacement or
appended notes, and scheduling for Today. Setting deadlines, adding tags or
checklist lines, and other supported schedules require
`THINGS_INDEX_THINGS_AUTH_TOKEN`. Schedule and deadline edits reject repeating
tasks. See [where to find the token](homelab.md#mac-worker).

The current tool appends checklist lines; it does not check off, rename,
remove, or replace existing lines. It also does not expose moving an existing
task or clearing its notes or deadline.

For new task capture, reminder timestamps require an `on_date` schedule.
Requested tags must already exist: capture skips missing tags with a warning,
while project creation and task updates reject missing tags.

## Project capture from Index 01

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
remain exact; missing or ambiguous areas or headings fail. Project creation
does not use fuzzy matching. If an explicit
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

## Retrying writes

All write tools accept an optional `idempotency_key`. See the
[write recovery guide](write-recovery.md) for retry rules and interrupted writes.
