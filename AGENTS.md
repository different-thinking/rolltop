# AGENTS.md

## Project Notes

Rolltop V1 is a Go, PostgreSQL, Bleve, and local-blob email mirror. Keep all
user-owned data scoped by `user_id` at every layer: database rows, blob paths,
search documents, sync runs, and HTTP reads.

**`user_id` scoping is now the only tenant isolation there is.** Every tenant's
rows live in one database and one set of tables; there is no per-tenant file to
fall back on. A query that forgets its `user_id` predicate reads another
person's mail, so every method that touches user-owned data takes a `userID` and
resolves its handle through `dataDB`/`mustDataDB` — not because the handle
differs any more, but because that is what keeps the tenant visible at the call
site and in review.

## Rules For Future Agents

- SMTP sending and message moves exist and are supported; extend them rather than
  reintroducing the old prohibition.
- Remote delete exists in exactly one place: emptying a folder that carries the
  Trash role (`syncer.ExpungeFetcher`, `Service.StartEmptyTrash`). Nothing else
  may flag `\Deleted` or expunge. It lists the folder live rather than trusting
  the mirror, proves `UIDVALIDITY` before deleting, and drops local rows only for
  the UIDs the server reports gone afterwards. Keep all four properties.
- Read-state sync is intentionally allowed to update only the IMAP `\Seen` flag.
- Do not accept `user_id` from normal browser routes.
- Admin routes may manage local users, but must not expose other users' mail.
- Do not log app passwords, IMAP passwords, OAuth access or refresh tokens,
  authorization codes, session tokens, or raw message bodies.
- Keep IMAP credentials and OAuth tokens encrypted with `ROLLTOP_MASTER_KEY`.
- Keep tests for tenant isolation current when changing sync, search, message, attachment, blob, or route behavior.
- Keep sync incremental: fetch by UID after each mailbox's last stored UID, stream messages into storage, and update `sync_runs` progress during long runs.
- An account's `auth_type` decides how it authenticates. A `google_oauth`
  account stores no password at all; do not add a fallback that reads one.
- Gmail's label views (All Mail, Important, Starred) must stay excluded from
  sync by default. The data model stores one folder per message, so mirroring
  them duplicates most of the mailbox.
- The whole-account lists - All Mail, Inbox, and every category view - read one
  flag, `mailboxes.show_in_all_mail`, through `Store.inPlayMailScope`. Sent,
  Drafts, Trash and Junk default it off (`defaultMailboxShowInAllMail`), and that
  default has exactly one definition: do not restate the role list in SQL. It is
  a default, not a rule - a reader may switch any of them back on, so a backfill
  that changes it belongs in a versioned migration's statements and never in a
  migration's `After` step, which runs again on every startup and would overwrite
  that choice. Junk is the one folder the lists drop by role regardless, because
  Report spam promises the message is gone from them.
- A conversation row is the message its list selected - the seed the list query
  returned - and `conversationView.ListDate` is that message's date. Threads are
  hydrated in full, so a row's thread carries messages the list excludes; taking
  one of those as the row prints a date the row is not sorted by, and the date
  section headings group on exactly that date. Thread-wide answers (message
  count, participants, read state, starred, attachments) still come from the
  whole thread.
- A mailbox's `sync_start_at` belongs in the IMAP search, not in a filter after
  the fetch. Apply it only to searches that decide what to **download** — the
  body fetches and `MailboxUIDSnapshot.FetchableUIDs`, which repair uses to pick
  missing UIDs. Never apply it to the searches that decide what to **delete or
  flag**: reconciliation deletes local messages absent from the server's UID
  list, and read/star sync marks every local message outside the returned set as
  unread or unstarred, so a cutoff-limited list there destroys mail that was
  mirrored before the cutoff existed.
- Google is the leading system for the contacts it owns. A local write to a
  contact whose `source` is `google` must reach Google before the local row
  changes, or the next sync silently undoes it. On an etag conflict, adopt
  Google's version and say so; never force the local one through.
- An email address identifies at most one contact where something has to resolve
  it to one -- the Me contact, a reply identity, an import's merge target, the
  picture beside a sender -- but the address book may hold it more than once.
  Google lets two people share an address, so a unique index over it made the
  second of them unstorable and failed the whole sync rather than one row. One
  contact still carries an address once; saving dedupes by normalized address,
  because two rows for one address are two answers to a question that has one.
- An outgoing identity is created by hand and never derived. It comes from the
  identity editor, from adding a mailbox, or from provisioning a user (sign-up,
  OIDC) -- `Store.EnsureMailIdentityForEmail` is the only door, and every caller
  is an act where the user named the address. Editing an existing mailbox is not
  one of them: it would hand back an identity the user removed. Nothing else may
  add one. Deriving them from the Me contact's addresses -- which is what
  `SyncMailIdentitiesForMeContacts` used to do -- let the address book decide
  what the From menu offered, so every address a Google sync or a vCard import
  put on the reader's own card became a sending identity they never chose. The
  sync now only re-binds and renames the rows that exist.
- An identity is removed by the user, or by the schema, and by nothing else.
  `mail_identities` cascades on its `contact_emails` row, so deleting the
  address an identity sends from takes the identity with it; that is the only
  removal the code does not ask for. The sync must not delete the rows it cannot
  match -- clearing `is_me` on a contact would then destroy every identity
  behind it, signature and server choices included.
- An identity points at a `contact_emails` row and cascades on its deletion, so
  saving a contact must not give its addresses new ids. `replaceContactEmails`
  matches them by normalized address and updates in place for exactly that
  reason -- and records the submitted order in `sort_order`, which the ids used
  to carry by accident; the other detail lists have no dependents and are still
  rewritten wholesale. `SyncMailIdentitiesForMeContacts` re-binds an identity by
  the address it stores as a second line of defence, but only when no Me address
  claims it by id: doing it for any unclaimed address would let a second Me card
  carrying the same address steal the first card's identity, and deleting that
  card would then cascade the identity away.
- Which holder answers for a shared address is decided in exactly one place,
  `contactEmailOwnerOrder`: the reader's own contact, then whoever carries it as
  their primary address, then the oldest. Resolve with
  `GetContactByEmailForUser` or the first holder from
  `ListContactEmailHoldersForUser`; never with a bare `LIMIT 1`. Anything that
  has to pick between the holders walks that list, and two of them invert part
  of the order for stated reasons: a Google-owned row is never adopted as the Me
  contact or as a local write's target, and the sync ranks the Me card last when
  choosing which local contact a Google person is, so the reader's own contact
  cannot become the mirror of somebody they share an address with.
- Provenance on a contact — `source`, `google_connection_id`, `external_id`,
  `etag` — is written only by the sync and the write-back path.
  `UpdateContact` deliberately leaves those columns alone so an ordinary edit
  cannot detach a contact from the account that owns it.
- Disconnecting a Google account demotes its contacts to local ones. Do not
  make it delete them: the user asked to stop syncing, and Rolltop's copy is
  often the only one they can still reach.
- Google is the leading system for calendars as well, and by the same rule: a
  create, edit, deletion or invitation answer reaches Google before the local
  row changes, and an etag conflict is resolved by adopting Google's version.
  Disconnecting an account does delete its calendars, unlike its contacts — a
  calendar is a pure mirror with no local editor behind it, and one left behind
  could never sync, never be edited and never be switched off again.
- An all-day event has no instant. Its bounds are stored as midnight **UTC** of
  the plain dates Google named, and everything that formats or compares one has
  to read it in UTC. Anchoring it to the viewer's zone moves a public holiday
  onto the wrong day for anyone west of that zone.
- The Calendar sync window lives inside Google's sync token. A request carrying
  a `syncToken` must not also carry `timeMin`; keep every other parameter
  (`singleEvents`, `showDeleted`) identical between the initial read and its
  deltas, or the deltas are undefined.
- A calendar the user has switched off is not synced. Switching one on triggers
  its own first sync, because an unsynced week renders as an empty one and reads
  as "nothing scheduled".
- A cross-account duplicate copy is hidden behind a pointer to the row that stays
  visible, and that pointer is only safe while it resolves. Deletes clear it in a
  SQLite trigger rather than in Go, because reconciliation, folder purges, account
  deletion, and generation rebuilds all delete message rows through different
  paths and a stale pointer in any of them hides mail that has no visible twin.
  Detection itself stays narrow on purpose: it hides a copy only when exactly one
  account in the group was addressed in `To` or `Cc`, never hides Sent, Drafts, or
  Trash copies, and only accepts a row that shows in All Mail as the original a
  copy hides behind - a Spam-filed or otherwise All-Mail-excluded row standing in
  as the original would take the message out of view entirely. Showing a message
  twice is recoverable; hiding the only copy is not.
- New attachment bodies should be indexed from raw `.eml` data and then discarded, not saved as separate attachment blobs.
- One data directory belongs to one process, and the instance lock is taken
  before anything opens Bleve or the blob store. The database no longer needs it
  — PostgreSQL handles concurrent clients — but Bleve does, and that is now the
  whole justification. A serving start waits for the lock
  (`ROLLTOP_STARTUP_LOCK_WAIT`) because deployments overlap the old and new
  container; `reset-search` keeps failing immediately instead. Do not move any
  open ahead of the lock, and do not make `reset-search` wait.
- The schema is `backend/store/pgschema/baseline.sql`, applied to an empty
  database and recorded as one row in `schema_migrations`. It is frozen: it was
  derived from the SQLite schema that preceded it, and that derivation is over.
  A schema change is a **new PostgreSQL migration layered on the baseline**,
  never an edit to `baseline.sql` — editing it changes the recorded checksum and
  makes every existing database refuse to start.
- SQL is written with `?` placeholders and translated to `$1..$n` in the driver
  (`backend/pgbind`). That is deliberate: many statements are assembled at run
  time from fragments, and numbering them in the source would mean every
  fragment knowing how many parameters the fragments before it contributed.
  Write `?`; do not mix the two styles inside one statement.
- Four PostgreSQL rules the SQLite schema let us ignore, each of which produced
  a real failure during the move:
  - In `INSERT ... SELECT`, a bare parameter in the SELECT list is typed
    **before** the target column is consulted, so it defaults to `text` and is
    then rejected. Write `CAST(? AS BIGINT)` — the SQL is executed only against
    PostgreSQL, but the cast spelling stays portable.
  - `CASE WHEN ?` needs a boolean. Pass `CASE WHEN ? <> 0` for the 0/1 integers
    this schema stores booleans as.
  - `SELECT EXISTS (...)` returns a boolean, not 0/1. Scan it into a `bool`.
  - An error inside a transaction aborts the whole transaction; there is no
    recovering and carrying on the way SQLite allowed. Where a write may
    legitimately conflict, ask for that outcome — `ON CONFLICT ... DO NOTHING`
    with an empty `RETURNING` — instead of provoking the error and handling it.
- `GROUP BY` must list every selected column that is not functionally dependent
  on a grouped primary key, and PostgreSQL derives that dependency per table:
  grouping by `mb.id` covers `mb`'s columns and none of a joined table's.
- Keep sync bounded in memory as well as in time. Anything that accumulates
  message content between commits - IMAP fetch batches, the search-index batch -
  must be bounded in **bytes**, not only in message count: mail sizes span four
  orders of magnitude, so a count-only limit lets one folder decide how much
  memory the process needs. Trim indexable text to the `backend/search` limits
  when a document is queued rather than when it is committed, and release raw
  bodies as soon as they are stored. The process also installs a soft heap
  ceiling at startup (`ROLLTOP_MEMORY_LIMIT`, `backend/memlimit`); it is a
  backstop for the unbounded case, not a licence to add one.

## Checks

Run before handing off:

```sh
npm run build:themes
TEST_DATABASE_URL=postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable go test -p 2 ./...
```

`-p 2` bounds how many package binaries run at once. Every test does real
database I/O now, and the default — one per core — oversubscribes a small
machine badly enough that timing-sensitive tests miss deadlines they normally
clear in under a second. The same run is green at 2 and red at 4,
reproducibly; CI sets it for the same reason.

`TEST_DATABASE_URL` names a **test-local** PostgreSQL the run may create and
drop databases on; `docker compose -f compose.dev.yml up -d db` starts one. Tests
skip without it and fail when it is set but unusable, so a broken database
service cannot report a green suite that verified nothing. Each test gets its
own database, cloned from a template named after the schema — which is why a
schema change does not need any template to be cleaned up by hand.

`npm run build:themes` is a prerequisite, not a convenience: manifest validation
stats the theme CSS a plugin manifest declares, so the Go suite fails on a clean
checkout without it.

Pull request CI (`.github/workflows/pr.yml`) only runs the checks the changed
paths require: Go (`gofmt`, `go test`), frontend (`typecheck`, Vite builds),
Android (unit tests and lint), and a Docker build when the image definition
changes. Keep the path filters in that workflow's `changes` job in sync when
adding a new top-level area.

`.github/workflows/ci.yml` has two jobs. `verify` runs on every push to `main`
and answers the one question a pull request cannot: two changes that are each
green alone may still be broken together. It builds the themes, runs the Go
suite, links every plugin backend with `-buildmode=plugin`, and verifies the
checked-in spam model. Keep it lean.

Do not add `-coverprofile` to that step. Tests in `backend/web` and
`backend/syncer` build a real backend plugin and load it with `plugin.Open`,
which demands an identical build fingerprint for every shared package. Coverage
over `./...` instruments `plugins/client_side_pgp/schema` in the test binary but
not in the plugin the test builds, so 14 tests fail with "plugin was built with
a different version of package rolltop/plugins/client_side_pgp/schema". If
coverage is ever wanted again, the plugin-loading tests have to be guarded with
`testing.CoverMode()` first.

`release` does the expensive packaging — Android APK, Docker image, GHCR push,
binaries, godoc — and runs only for `v*` tags and manual dispatch. Deployments
build the `Dockerfile` from source and self-hosters pull the upstream image, so
nothing consumes a per-merge `latest`. Do not move packaging back onto `main`
without a consumer that needs it.

Two ordering constraints in `release` must survive any refactor: the Android
step writes `frontend/public/android/{rolltop.apk,latest.json}`, which the
Docker frontend stage copies into the image, so it has to stay ahead of the
Docker build; and neither the workflow nor the `Dockerfile` may go back to a
hand-maintained plugin list — both derive the set from `plugins/*/backend`,
because the hardcoded lists had already drifted apart.
