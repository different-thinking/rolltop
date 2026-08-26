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

A rule then does three things at most: it marks mail read, it forwards, and it
moves. Forwarding goes through the user's configured SMTP identity; forwarded
mail receives an opaque `X-Rolltop-Forwarded-By` header, and the plugin refuses
to forward a message that already carries the same marker.

Marking read (`mark_read`) is the one action that composes with the others
rather than choosing between them: mail a rule forwarded to the person who
handles it, filed in a folder, or deleted is mail with no unread badge left to
answer for. It runs **before** the forward and the move, because the move is what
ends the message's life in the folder it is in -- `\Seen` is pushed against the
UID the message still has, and the copy the move leaves behind carries the flag
with it. The push is the same generation-proved one the star uses
(`StoredMessageHost.MarkMessageRead`); the `star` action was removed as a product
decision (`migrations/user/003`), not because sync could not write it back.

The audit keeps three outcomes apart, because they are three different states of
the message:

- `already_read` -- the message needed nothing, and no host call was made. The
  alternative is one IMAP round trip per matched message to set the flag it has.
- `queued` -- read locally, with `\Seen` waiting on a mailbox generation the
  mirror cannot prove. Not `ok`: the server does not have the flag yet, and the
  pending-read push is what will put it there. The move refuses on exactly that
  same evidence, so a queued flag is never dropped along with the local row a
  completed move deletes.
- `failed` -- the push errored, and the rest of the rule **still runs**. An
  evaluation row is terminal for its message, so returning here would leave a
  "forward it, then delete it" rule with the mail neither forwarded nor deleted
  and nothing that ever comes back to it. The row ends as `action_failed`.

Every rule in one arrival is handed the same message, so the first rule to mark
it read leaves the rules behind it looking at mail that is read -- otherwise the
second rule spends another push on the flag the first one set.

A forward may be limited to the mail the rule saw arrive (`forward_new_only`,
which the editor turns on for a new rule). The mailbox behind a rule written
today holds years of mail, and forwarding is the one action a rule takes that
leaves the account -- every copy is also filed by the sending provider, which
mirrors it back into the reader's own lists. So the option exists, and it is not
"do not backfill this rule": Backfill still walks the mail already in the
mailbox, still matches it, still moves it and still records what it decided. Only
the forward is skipped, and the audit row says so instead of showing a match that
sent nothing. Mail that arrived while the rule was running and then waited on
`older_than:` is still forwarded when its wait is released -- the waiting row
carries the pass it was written in for exactly that reason.

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
an action failure; it does not quietly count as a match that moved nothing. A
failure is terminal for that message until the rule is saved again -- saving is
what puts already-decided mail back in front of the rule -- and Backfill is what
walks it; the scheduled worker only picks up rows still waiting on age.

The copies the provider keeps of what a rule forwarded are out of the reader's
lists, and out of the filters' own reach: every send records its Message-ID for
the account it left from, and the mirror recognises that copy as the reader's own
outgoing mail (`messages.own_outgoing_copy`). Without it a forwarding rule fed
itself -- the copy of a forward matches the same sender or subject the original
did -- and a Gmail account mirroring All Mail showed every forward back in Inbox
and in the categories.

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

Only one scheduled row may wait for a given rule and message, and that row keeps
the phase it was written in when that phase was the arrival: saving an edited
rule puts waiting mail back in front of the rule through the backfill, and mail
this rule watched arrive must not become mail it merely found -- a rule that
forwards new mail only would stop forwarding it.

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
