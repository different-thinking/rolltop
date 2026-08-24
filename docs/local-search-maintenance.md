# Local Search Maintenance

This file used to cover one-off SQLite surgery as well. That half is gone with
SQLite: the relational data is in PostgreSQL, and a one-off change there is a
`psql` session against a database you have a `pg_dump` of, not a documented
procedure. What is left is the part that never depended on the database — the
per-tenant Bleve search indexes on `/data`, which are still single-process,
still memory-mapped, and still the reason the instance lock exists.

The database still appears below, because search recovery reads and writes it.
Where it does, it is PostgreSQL: no WAL, no checkpoints, no file-level locking.

Before making a one-time local change to stored data:

1. Stop the app.
2. Take a `pg_dump` (see the README's Backups section).
3. Prefer a checked-in migration when the change should be repeatable.
4. Document any manual-only cleanup in this file or the related change notes.

Disabling a plugin should not drop its data. Data removal should remain an
explicit admin maintenance action.

## Automatic Tenant Search Recovery

Search writes pass through a shared priority- and byte-aware coordinator. It
allows bounded concurrency across users, never admits two writes for the same
tenant, prioritizes direct purge/rebuild work over queued attachment enrichment,
and ages background jobs so they cannot starve. Before projecting message
content, each indexing chunk obtains a conservative coordinator reservation
covering the target chunk plus one maximum-size document. Rolltop then builds a
soft-target chunk and reconciles that reservation first to its projected string
payload and then to Bleve's actual batch size. If one active Bleve write does not
return within two minutes, Rolltop writes
`data/users/<user-id>/bleve.recovery-required` and requests a controlled process
restart. The writer-stall path gives normal cleanup 15 seconds, then returns to
the process entrypoint even if a plugin or database cleanup remains blocked.
Docker must have a restart policy such as `unless-stopped` for this recovery to
be hands-off.

During startup, under the normal data-directory lock and before opening the
marked index, Rolltop performs these steps in order:

1. Mark the tenant's search-visible message rows pending — one committed
   PostgreSQL `UPDATE`. The ordering matters and the durability does not have
   to be arranged: the rows must be pending *before* the index is quarantined,
   or a crash in between leaves messages in neither the index nor the rebuild
   queue, and a committed transaction is already durable. Under SQLite this
   step needed its own connection, `synchronous=FULL`, and a full WAL
   checkpoint to establish the same barrier.
2. Rename only `data/users/<user-id>/bleve` to a timestamped quarantine path
   and persist that directory rename.
3. Clear the recovery marker and open a fresh derived index.
4. Queue the normal local indexing worker for all pending rows, including rows
   in `manual` and `never` sync folders.

This does not delete or alter message rows, IMAP state, or blobs. The rebuild
reads the message rows from PostgreSQL and the retained local `.eml` blobs; when
raw data has expired, normal index hydration may contact IMAP. Startup leaves
the marker in place if the pending-row write or durable quarantine cannot
complete safely. If persisting
marker removal fails after quarantine, startup restores the marker when it can
and fails; the already-pending rows and durable quarantine keep either crash
outcome safe for the next start.

## Tenant Search Index Reset

The offline command is the manual fallback for recovery that cannot complete
automatically. Use it instead of deleting Bleve files manually:

```sh
rolltop reset-search --user-id 1 --confirm-offline
```

The command requires a positive numeric local user ID, takes the same
data-directory lock as the server, and refuses to proceed without an explicit
offline confirmation. It marks only that user's messages in search-visible
mailboxes pending, then atomically renames only
`data/users/<user-id>/bleve`. It does not contact IMAP or delete message,
attachment, or blob records.

Keep the timestamped quarantine directory until the replacement index has been
verified. The command prints both paths; before restarting Rolltop, the old
index can be restored by renaming the quarantine path back to `bleve`.

This command repairs only derived search state. It cannot restore a message
whose database row is missing. Deploy mailbox recovery and let the local row
count reach the remote mailbox count first; use `reset-search` only if Bleve repair is
still stalled. After restart, indexing may retrieve raw messages from IMAP when
the local `.eml` retention window has already expired.

## Why a Rebuild Says It Did Not Start

The admin search-index card and the per-account rebuild in folder settings both
queue the same work: one run per mail server, purging and refilling every
search-visible folder of that server. Because the run claims all of those
folders at once, a single busy folder refuses the whole mail server, and the
request answers `409 Conflict` rather than waiting — an HTTP request cannot sit
behind a sync that may take hours.

The refusal names the mail server and what the sync runner found holding it.
Each reason carries its own next step, because the step differs — work listed in
Activity is waited out there, and the recovery gate is not listed at all:

- **`Folder sync is already running for the folder "INBOX". Follow it in
  Activity, then try again.`** — ordinary sync work, which ends by itself.
- **`Search index rebuild is already running for the folder …`** — the rebuild
  is already in flight, most likely from an earlier click. Rebuilding an account
  with a Gmail `All Mail` folder re-reads the whole mailbox, so this can hold for
  a long time. There is nothing to retry.
- **`Folder maintenance is already running for the folder …`** — a purge, a
  folder deletion, or another destructive local task.
- **`Folder recovery is still pending for this user, and it holds every mail
  server until it finishes.`** — the generation-recovery gate above. It is keyed
  on the user, not on one server: nothing is reserved, every rebuild is refused
  until recovery clears, and waiting for a sync to finish will not help. See
  Mailbox Recovery Status. Servers held by one reason are named together, so this
  appears once however many mail servers the tenant has.
- **`Whatever held it has since been released — press rebuild again.`** — the
  cause cleared between the refusal and the question.

The distinction is the point of the message. All of these once read "sync or
full-text reindexing is already running", which sent an operator to Activity to
look for work that, in the recovery case, is not there.

## Mailbox Recovery Status

Generation recovery is serialized to one active mailbox per user. A queue line
lists every durable marker, the active target, and up to eight waiting targets:

```text
recover mailbox generation queue user_id=1 pending_markers=3 active_target={account_id=1 mailbox="Gmail Forward"} queued_targets=[{account_id=1 mailbox="Archive"} {account_id=3 mailbox="INBOX"}] other_mailbox_work_active=false
```

The active turn emits a heartbeat every 15 seconds with its current phase, UID,
durable checkpoint, fetched/stored counts, and the same queue summary. An
unchanged queue is also repeated every two minutes. During recovery, at most one
unrelated live Inbox sync may write alongside the recovery turn; later Inbox
polls log that another live Inbox writer is already running. The
`other_mailbox_work_active` field is true for that concurrent reservation and
for other user-scoped mail work that currently blocks the next recovery turn,
including account-wide sync planning, foreground plugin operations, sender
statistics, and an attachment-index worker that is still exiting.

Remote IMAP migration routines log a start line, a 30-second progress heartbeat,
and a terminal completed, deferred, failed, or canceled line. If mailbox
generation recovery pauses a migration, separate pause heartbeats identify that
wait, so an IDLE subscription is not mistaken for an actively copying routine.
Long migrations release their foreground reservation every 25 scanned messages,
allow a destination mailbox refresh to run, and then reacquire it for the next
bounded turn.
