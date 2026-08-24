# Mail filters plugin

Adds search-driven filtering for Rolltop mail.

## Structure

- `manifest.json` declares the runtime backend binary and frontend bundle.
- `backend/` contains the Go plugin entry point, rule engine, protected API routes, and stored-message hook.
- `frontend/` contains the settings UI source.
- `frontend_dist/` contains generated browser assets from `npm run build:plugins`.
- `migrations/user/` contains plugin-owned, user-scoped tables.

## Behavior

A rule stores one Rolltop search string. The settings editor writes the three
common conditions -- sender, subject, and an age in days -- into that string
through named fields, and still offers the search itself for anything the fields
do not cover. An open message can start one too: its action menu offers a filter
on that sender or on that subject, which opens the editor with the condition
already written.

```text
from:studio@example.com subject:Reservation older_than:7d
```

A rule then does two things at most: it forwards, and it moves. Forwarding goes
through the user's configured SMTP identity; forwarded mail receives an opaque
`X-Rolltop-Forwarded-By` header, and the plugin refuses to forward a message
that already carries the same marker.

A move names its destination in one of two ways, and never both:

- **Delete** (`move_role: "trash"`) and **Archive** (`move_role: "archive"`) are
  relative to the message's *own* account, resolved per message. One rule over
  several accounts therefore files each account's mail in that account's own
  folder.
- **One folder** (`move_mailbox_id`) is an exact destination, which only fits a
  rule that stays inside that folder's account: a move cannot cross accounts.
  The editor says so when a rule's scope reaches further than its destination.

Trash comes from the mailbox role. Archive does not have one -- in Rolltop the
Archive folder is a choice the reader made, in an identity's settings or in the
swipe mapping -- so the plugin asks the host for it
(`StoredMessageHost.ArchiveMailboxID`) rather than looking for a role that does
not exist. That keeps a filter archiving to the same folder the header's Archive
button uses. An account with no Trash, or none with an Archive chosen, records
an action failure; it does not quietly count as a match that moved nothing.

Filters read the mail the whole-account lists show -- folders that opt into All
Mail, never Junk, never a hidden cross-account duplicate -- so Sent, Drafts and
Trash are out of reach of a rule by default. That is what keeps an age rule from
emptying the reader's own Sent folder. What a rule may *write* is unaffected: a
rule may move mail into any folder, including one the rule itself cannot read.

"Delete mail from this sender after N days" is the sender condition, an age of N
days, and Delete. The N days run from the message's own `Date`, not from when
the rule saw it: mail already older than N goes on the next pass, and the rest
waits in the pending queue with its due date shown. Deletion means the same
thing it means everywhere else in Rolltop -- the message lands in Trash, and
only emptying Trash removes it from the server.

Every rule evaluation is recorded for 30 days, including matches, misses, skipped account scope, scheduled age checks, action failures, and loop prevention. The settings page shows both halves of that record: what the filters have already done, and what they are still waiting to do.

## Build

```sh
go build -buildmode=plugin -o plugins/mail_filters/backend/mail_filters.so ./plugins/mail_filters/backend
npm run build:plugins
```

## Hook Locations

- Backend ABI: `backend/plugins/backend.go`
- Sync dispatch: `backend/syncer/autocrypt.go`
- Stored-message call site: `backend/syncer/syncer.go`
- Frontend settings route: `/settings/account/plugins/filters`

## Notes

`older_than:` is the one term that is not a search. It names when the rule may
act rather than something to find in the message, so it is taken out of the
query and answered from the message's own date; what is left decides whether the
rule matches at all. A message that matches the rest but is still too young is
recorded as scheduled, in whichever phase it was first seen -- arrival or
backfill alike, because a rule created today has to reach the mail that is
already in the mailbox. A rule whose only term is the age matches every message
its scope reaches. A lightweight plugin worker processes due scheduled rows
every 15 minutes while the backend plugin is enabled, and `Run due` on the
settings page does the same pass on demand.

Only one scheduled row may wait for a given rule and message. A partial unique
index says so (`migrations/user/002`), not just the code that inserts, because a
concurrent arrival and backfill would otherwise pass each other between a lookup
and an insert and queue the same move twice.

Backfill walks stored mail oldest first, one page per request, and returns the
cursor to continue from; the settings page follows that cursor to the end. The
mail an age rule exists to clean up is the oldest in the mailbox, which a single
newest-first page left out. The walk skips what the rule already decided on
since its last edit: a rule acts where it matches, so a second Backfill over the
same message would forward it a second time. Saving an edited rule puts every
message back in front of it. A page that fails part way reports what it
evaluated and where it stopped, because those rows are already committed.

A wait whose message has since been deleted is purged rather than left in the
queue: it can never resolve, and its due date is in the past, so it would sort
to the front of the pending list for good.
