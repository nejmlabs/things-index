# Write recovery

[Back to the README](../README.md)

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
[deployment profiles](deployment.md) for signing, setup, and retention.
