# ThingsIndex Helper Shortcut

`ThingsIndex Helper` is the only supported macOS capture adapter. The worker
invokes it with Apple's built-in `shortcuts` command and exchanges versioned
JSON files. No AppleScript or Things URL authorization token is used.

The Shortcut requires Things 3.17 or newer and macOS 14 or newer. It must be
installed for the same logged-in user that runs `things-index-worker`.

## Install

The supported onboarding path is the worker's local setup GUI:

```sh
things-index-worker --setup
```

It opens a loopback-only page with **Install Shortcut**, **Verify Access**,
**Test Capture**, and **Finish Setup** controls. The capture test creates one
clearly labelled disposable Inbox task and verifies both Create and Edit before
Finish is enabled. The page runs only for onboarding and exits after the test
succeeds. The signed Shortcut is embedded in the worker, so this does not
require another download or network request.

On first use, macOS can present separate privacy dialogs for external
dictionary input and for Things actions. Choose **Always Allow** during this
deliberate setup. Apple documents that this choice persists for later runs.
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
    "finalise-capture-v1"
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

## First-run verification

The `ping` operation performs one harmless lookup for an impossible Things ID;
it does not create or edit anything. The setup GUI's **Verify Access** button
performs this check. Its subsequent **Test Capture** step creates and finalises
one labelled disposable Inbox task so all routine action permissions are
settled during onboarding.

For manual verification, run it once from Terminal and approve the Shortcut's
Things access during this deliberate setup run:

```sh
/usr/bin/shortcuts run "ThingsIndex Helper" \
  --input-path "$PWD/shortcuts/examples/ping.json" \
  --output-type public.json
```

Run the same command a second time and confirm that it returns the three
capabilities without a prompt. Only then load the worker LaunchAgent.

The optional `capture-task.json` and `finalise-capture.json` fixtures exercise
the two-step create/finalise contract. The capture fixture creates one clearly
labelled disposable Inbox task. Copy its returned `id` into the finalise
fixture before running that fixture.

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
