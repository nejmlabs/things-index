# ThingsIndex Helper Shortcut

`ThingsIndex Helper` is the worker's adapter for Things heading operations —
create, rename, and archive run through the Shortcut's native Things App
Intents because no URL-scheme or AppleScript surface reaches headings. The
worker invokes it with Apple's built-in `shortcuts` command and exchanges
versioned JSON files. (Captures use the Things URL scheme directly; the
capture operations specified below remain implemented in the Shortcut, but
the worker currently calls only `ping` and the three heading operations.)

The Shortcut requires Things 3.17 or newer and macOS 14 or newer. It must be
installed for the same logged-in user that runs the worker.

## Install

The supported onboarding path is the worker setup wizard:

```sh
~/.local/bin/things-index worker --setup
```

The wizard installs the Shortcut from the copy embedded in the binary (no
extra download or network request), waits for your one **Add Shortcut**
click, then runs `ping` to check basic external input and Things lookup access.
Heading writes may need separate first-use approval during attended setup.

On first use, macOS can present separate privacy dialogs for external
dictionary input and for Things actions. Choose **Always Allow**, if offered,
during this deliberate setup. Apple documents that this choice persists for later runs.
Replacing the Shortcut or resetting its Privacy details can require those
grants again.

As a manual fallback, open
[`ThingsIndex Helper.shortcut`](ThingsIndex%20Helper.shortcut) on the Mac that
runs Things and choose **Add Shortcut**. Keep the exact name
`ThingsIndex Helper`; the worker intentionally does not guess among renamed
copies.

The artifact is compiled from the readable
[`ThingsIndex Helper.cherri`](ThingsIndex%20Helper.cherri) source and signed
with Apple's built-in `shortcuts sign --mode anyone` command.

The worker has no Cherri dependency. Cherri is only the pinned, optional
maintainer tool used to rebuild the distributable Shortcut.

## First-run verification

The `ping` operation checks external input and performs one harmless lookup
for an impossible Things ID; it does not create or edit anything. The setup
wizard runs this check automatically after installing the Shortcut. A successful
ping does not establish permission for heading writes, which may need separate
first-use approval.

For a manual ping from a **repository checkout**, run this from its root:

```sh
/usr/bin/shortcuts run "ThingsIndex Helper" \
  --input-path "$PWD/shortcuts/examples/ping.json" \
  --output-type public.json
```

To settle the write permissions, use **Terminal on the Mac with its desktop
visible**, before sending heading writes through MCP. The background worker's
30-second command deadline is too short to rely on while finding and approving
first-use dialogs. The following direct commands need no repository checkout,
Python or Go.

1. In Things, create a new, empty project named **ThingsIndex Setup Check**. If
   that name already exists, choose another unused name and replace the project
   value in all three examples. These steps create, rename and complete one test
   heading inside that project.
2. In one Terminal window, prepare and run the create request:

   ```sh
   things_setup_dir=$(mktemp -d /tmp/things-index-setup.XXXXXX)
   cat > "$things_setup_dir/create.json" <<'JSON'
   {"schemaVersion":1,"operation":"create-heading","project":"ThingsIndex Setup Check","title":"Permission Check"}
   JSON
   /usr/bin/shortcuts run "ThingsIndex Helper" --input-path "$things_setup_dir/create.json" --output-type public.json
   ```

   Keep Shortcuts visible and approve the requested input/Things action access;
   choose **Always Allow** if offered. Initial approval can take a few minutes.
   Wait for JSON containing `"ok":true` and confirm **Permission Check** appears
   in the project before continuing.
3. Run the rename request in the same Terminal. It exercises Things' **Edit
   Items / Title** action, which may need its own approval:

   ```sh
   cat > "$things_setup_dir/rename.json" <<'JSON'
   {"schemaVersion":1,"operation":"rename-heading","project":"ThingsIndex Setup Check","heading":"Permission Check","title":"Permission Check Renamed"}
   JSON
   /usr/bin/shortcuts run "ThingsIndex Helper" --input-path "$things_setup_dir/rename.json" --output-type public.json
   ```

   Wait for `"ok":true` and confirm the new heading name in Things.
4. Run the archive request. This exercises **Edit Items / Status**; approve any
   separate first-use request and wait for `"ok":true`:

   ```sh
   cat > "$things_setup_dir/archive.json" <<'JSON'
   {"schemaVersion":1,"operation":"archive-heading","project":"ThingsIndex Setup Check","heading":"Permission Check Renamed"}
   JSON
   /usr/bin/shortcuts run "ThingsIndex Helper" --input-path "$things_setup_dir/archive.json" --output-type public.json
   ```

If a command fails or is interrupted, inspect the project and the Shortcut's
Privacy details before retrying. A missing reply does not prove the write did
not happen; do not repeatedly dispatch an uncertain operation.

Finally, use your connected MCP client to create a **different** heading named
**Background Check** in that same project, rename it to **Background Check
Renamed**, and archive it. Use `create_things_heading`, `rename_things_heading`
and `archive_things_heading`, one at a time, waiting for each queued result to
finish and checking Things. This entire second sequence must complete without
clicks or approval dialogs. You can then move the disposable project to Trash
in Things. The Shortcut ping and successful worker startup alone do not replace
this background write check.

The optional `capture-task.json` and `finalise-capture.json` fixtures exercise
the two-step create/finalise contract. The capture fixture creates one clearly
labelled disposable Inbox task. Copy its returned `id` into the finalise
fixture before running that fixture.

## Safety contract

The Shortcut must:

- never use `Ask Each Time`, `Show When Run`, notifications, the clipboard, or
  shell commands;
- return one JSON document for every operation;
- resolve project, area, and heading names exactly and reject zero or multiple
  matches;
- create tasks initially with the temporary title
  `ThingsIndex pending [<requestId>]`;
- never create a second task when that exact temporary title already exists;
- leave the temporary title in place until `finalise-capture`; and
- limit all recovery lookups to at most two results so duplicates are detected.

The temporary title closes the crash window between Things creating an item and
the worker durably recording its ID. It is normally replaced with the requested
title immediately and should never be visible in a completed capture.

## Input

The first actions are:

1. `Get Contents of Shortcut Input`;
2. `Get Dictionary from Input`; and
3. read `schemaVersion` and `operation` from that dictionary.

Reject any `schemaVersion` other than `1`.

### `ping`

```json
{"schemaVersion":1,"operation":"ping"}
```

Return this dictionary:

```json
{
  "schemaVersion": 1,
  "ok": true,
  "capabilities": [
    "capture-task-v5",
    "find-capture-v1",
    "finalise-capture-v1",
    "create-heading-v1",
    "rename-heading-v1",
    "archive-heading-v1"
  ]
}
```

### `find-capture`

```json
{"schemaVersion":1,"operation":"find-capture","requestId":"32-lowercase-hex-characters"}
```

Build the exact temporary title from `requestId`. Use Things' `Find Items`
action with type `To-Do`, title containing that value, and limit `2`. Retain
only results whose `Title` equals the complete temporary title.

Return their Things IDs, including an empty array when there is no match:

```json
{"schemaVersion":1,"ok":true,"ids":["things-id"]}
```

### `capture-task`

```json
{
  "schemaVersion": 1,
  "operation": "capture-task",
  "requestId": "32-lowercase-hex-characters",
  "task": {
    "title": "Buy milk",
    "notes": "Use glass bottles",
    "destination": {
      "kind": "project",
      "name": "Shopping",
      "heading": "Groceries"
    },
    "start": "on_date",
    "startDate": "2026-08-17",
    "startDayOffset": 1,
    "evening": false,
    "reminderTime": "18:30",
    "reminderMinuteOffset": 1110,
    "deadline": "2026-08-18",
    "deadlineDayOffset": 2,
    "tags": ["Errand"],
    "checklist": "Check fridge\nBuy milk"
  }
}
```

First resolve any requested destination, then perform the same exact
temporary-title lookup as `find-capture`:

- two matches: return `manual_review_required` without creating anything;
- one match: reuse it and reapply the post-create editable fields; or
- no matches: use the placement-specific Things `Create To-Do` branch, then
  find the returned ID so the common edit path receives an item array.

Destination rules:

- missing destination or `kind: inbox`: leave `Parent` and `Heading` unset;
- `kind: area`: find exactly one Area whose full title equals `name` and pass
  that direct query result as Create To-Do's `Parent`;
- `kind: project`: find exactly one Project whose full title equals `name` and
  pass that direct query result as Create To-Do's `Parent`; and
- a project `heading`: find exactly one Heading with that full title and the
  resolved project's `Parent ID`, then pass both the Project as `Parent` and
  the Heading as `Heading` to Create To-Do.

Use the temporary title—not `task.title`—for `Create To-Do`. Placement is part
of that atomic Create because Things exposes Heading on Create, not as a
reliable Edit Items destination. Then use the already verified field-specific
Things `Edit Items` actions for native Start, reminder, deadline, tags, notes,
and newline-delimited checklist. A retry reuses the atomically placed pending
item and reapplies those post-create fields. Derive native Shortcuts dates from
`Current Date` plus the numeric day/minute offsets; do not parse display strings
as dates. Use literal `onDate`, `anytime`, and `someday` values because Things'
Start parameter is not dynamically resolvable. Keep `Show When Run` disabled
throughout.

Return:

```json
{"schemaVersion":1,"ok":true,"id":"things-id"}
```

`appliedTags` may also be returned as an array of tag titles. Omit it when tags
were not verified; an omitted field does not mean that no tags were applied.

### `finalise-capture`

```json
{
  "schemaVersion": 1,
  "operation": "finalise-capture",
  "id": "things-id",
  "title": "Buy milk"
}
```

Use `Find Items` with its `ID` filter and limit `2`. Require exactly one match,
then use `Edit Items` to set its Title. Notes and all other task details were
already written by `capture-task`, so finalisation must not edit them again.
Return:

```json
{"schemaVersion":1,"ok":true}
```

### `create-heading`

```json
{"schemaVersion":1,"operation":"create-heading","project":"Shopping","title":"Groceries"}
```

Resolve exactly one Project whose full title equals `project`
(`destination_not_found` / `destination_ambiguous` otherwise). Then look for an
existing heading with that exact title and the project's ID, limit `2`:

- two matches: return `heading_ambiguous`;
- one match: return its ID with `ok: true` — creation is idempotent, so worker
  retries never duplicate headings; or
- no matches: use Things' native `Create Heading` action with the direct
  project query result, then re-resolve the heading by title and parent ID
  (`create_failed` when it did not appear).

Return:

```json
{"schemaVersion":1,"ok":true,"id":"things-heading-id"}
```

### `rename-heading`

```json
{"schemaVersion":1,"operation":"rename-heading","project":"Shopping","heading":"Groceries","title":"Weekly Groceries"}
```

Resolve the project, then exactly one heading with that full title and the
project's ID (`heading_not_found` / `heading_ambiguous`). Use `Edit Items` to
set its Title. The worker verifies the rename against the Things database
before reporting success, so a silently ignored edit still fails honestly.
Return the heading's ID as for `create-heading`.

### `archive-heading`

```json
{"schemaVersion":1,"operation":"archive-heading","project":"Shopping","heading":"Groceries"}
```

Resolve the project and heading as for `rename-heading`, then use `Edit Items`
to set Status to the literal `completed` value (Status, like Start, is a
non-resolvable enumeration). The worker verifies the status change in the
database. Return the heading's ID as for `create-heading`.

## Errors

Expected failures are JSON results, not interactive alerts:

```json
{"schemaVersion":1,"ok":false,"code":"destination_not_found"}
```

Use these stable codes where applicable:

- `invalid_request`
- `destination_not_found`
- `destination_ambiguous`
- `heading_not_found`
- `heading_ambiguous`
- `manual_review_required`
- `create_failed`
- `finalise_not_found`
- `finalise_ambiguous`

The final Shortcut action for every branch is `Stop and Output` with JSON text.
The worker requests `public.json` output and rejects unknown fields, extra
output, unsupported versions, and `ok: false` results.

## Maintainer rebuild

The source currently targets Cherri v2.3.0 plus
[`cherri-v2.3.0.patch`](cherri-v2.3.0.patch). The small patch preserves the
nested dictionaries and property metadata required by Things App Intent raw
actions. See [`CHERRI-NOTICE.md`](CHERRI-NOTICE.md) for provenance and licence
details.

Compile in a temporary checkout; do not vendor Cherri into this project or add
it to the ThingsIndex binaries:

```sh
git clone --branch v2.3.0 --depth 1 \
  https://github.com/electrikmilk/cherri.git /tmp/things-index-cherri
git -C /tmp/things-index-cherri apply \
  "$PWD/shortcuts/cherri-v2.3.0.patch"
(cd /tmp/things-index-cherri && go build -o cherri .)
cp "shortcuts/ThingsIndex Helper.cherri" /tmp/things-index-helper.cherri
(cd /tmp && /tmp/things-index-cherri/cherri \
  /tmp/things-index-helper.cherri --skip-sign)
/usr/bin/shortcuts sign --mode anyone \
  --input "/tmp/ThingsIndex Helper_unsigned.shortcut" \
  --output "shortcuts/ThingsIndex Helper.shortcut"
```
