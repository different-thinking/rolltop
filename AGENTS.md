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
- A move of many messages is one IMAP command, not one per message. Each message
  used to pay its own SELECT, UID SEARCH and UID MOVE — three network round
  trips against a server that also rate limits them — which is why a
  whole-filter delete took minutes and held the tenant's foreground reservation
  for all of it. `Service.moveMessagesInBatches` gathers the messages leaving one
  source mailbox generation and moves them together; three properties make that
  safe and must survive any change to it. One command names **one** source
  mailbox under **one** proven `UIDVALIDITY`, and names each UID once — so the
  walk starts a new gathering whenever either changes, and reorders a selection
  by source folder first, because a selection made in All Mail interleaves them
  by date and would otherwise batch nothing. Outcomes are read **per message**:
  a UID the server no longer has, a UID it refuses, and a UID it moved are three
  answers inside one command, and a batch the server refuses outright is settled
  by asking which of its UIDs are still in the source rather than by recording
  the refusal against all of them. And a message whose dispatch has been claimed
  is always dispatched and always recorded, even when the run is giving up — a
  claim nobody settles is a transfer the next attempt has to reconcile against
  the server. The gathering is bounded in bytes as well as in count, like every
  other batch that holds message content.
- What a move changed locally is settled once per batch, not once per message.
  `moveAnnouncer` owns both halves of that — the search documents the moved
  messages leave behind and telling the reader. A lone move still settles both
  the moment it lands. A run that did it per message spent a whole index commit
  on each document and asked every open view to reload thousands of times, and
  kept the All Mail cache re-warming itself for as long as it worked; between
  them that was most of what a large move cost everything else the reader was
  doing. Reconciliation removes its documents in one call for the same reason —
  emptying a Trash folder passes through it with everything the server confirmed
  gone.
- **A long operation reports on a pace, and reporting never costs more than the
  work.** Three separate things used to be spent per message and are now spent
  per interval or per batch, and each must stay that way:
  - The `sync_runs` progress row (`syncProgressReporter.step`, shared by the
    ordinary fetch, the sparse repair, and a move run). A folder being mirrored
    or repaired walks thousands of messages and steps over most of them, and a
    row write per step costs more than the step. The tally is still counted per
    message and never approximated; only publishing it is paced. Every boundary
    that ends a turn or a folder **commits** regardless of the pace, so what was
    mirrored is durable alongside the checkpoint proving it, and `FinishSyncRun`
    writes the full tally at the end whatever the pace withheld.
  - The chrome event stream (`syncEventMinInterval` in `apiEvents`). Each signal
    rebuilds the entire chrome snapshot — the folder list with its counts, the
    categories, the archive mapping — once per connected tab, against the same
    database the operation is competing with. Signals are lossy and the payload
    is a fresh snapshot either way, so a burst collapses into one rebuild per
    interval. The first signal after a quiet moment must still go out at once:
    something is waiting on it.
  - Reconnecting. A batch of moves and the batches of one Trash purge each hold
    a single login for their whole run (`MoveSession`, `ExpungeSession`); a
    handshake, TLS negotiation and LOGIN per batch is most of what those cost
    and is what mail hosts throttle. Each batch still selects its folder and
    proves its generation, so reuse costs nothing in safety — only the login is
    saved.
- Anything in the frontend waiting on work the server will announce waits on the
  announcement, not on a timer: `waitForChromeEvent` (`chromeEvents.ts`) is that
  wait, and the interval passed to it is the fallback for the announcement that
  never comes. A queued move that slept its full interval regardless kept rows
  hidden for seconds after the move they were hidden for had already finished.
  It also takes a floor, and a waiter that polls the server needs one: the work
  announces itself *while* it runs, not only when it finishes, so waking on
  every announcement turns a five-second poll into several a second for as long
  as the work lasts — against the database the work is competing with, which is
  the cost this whole area exists to remove.
- Settling a claimed message transfer outlives the context that failed it
  (`context.WithoutCancel`), on the failure path as much as the success path.
  A batch is claimed before it is dispatched, so one cancelled request strands
  every claim in it, and a dispatch this process owns but never finished is not
  reconcilable by anything short of a restart — the messages behind it refuse
  every later move until then.
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
- Filing mail is decoupled from the IMAP work behind it. A row a list mutation
  dismissed - delete, Report spam, archive, snooze - is gone from that list the
  moment it is clicked and stays gone while the list still returns it; only an
  undo, a proven failure, or a queued move the view stopped watching puts it
  back. Never release a dismissal because the request finished or a run
  completed: a queued move ends minutes later, and the rows return to the screen
  for the gap until the reload answers, which is the flash the dismissal exists
  to prevent. Whatever proves a move also takes the rows out of the list's own
  data (`onMessagesMoved`) rather than leaving that to the reload - and only
  what proves it may: a queued move has been accepted, not performed, so
  reporting its rows moved takes them out of the list the dismissal is measured
  against, and the next reload hands them back with nothing hiding them. Read
  outcomes per message and per run, never per batch: a move that relocated part
  of a thread restores exactly what stayed, and a set of runs that did not all
  end the same way settles each run's own messages, or a dismissal that outlives
  its mutation hides mail that never went anywhere.
- A conversation row can span accounts, and filing it means filing all of it. A
  thread carries copies of the same mail whenever several of the accounts were
  addressed, or none of them was - Bcc, a mailing list - which is exactly when
  duplicate detection refuses to hide one behind another. Delete, Archive and
  Report spam name a role, not a folder, and every account has its own folder
  for that role, so such a row is split by account and moved once per account;
  the server refuses a move into another account's folder, so never send one set
  of IDs to one mailbox. Refusing the row instead - the old "conversation
  containing messages from multiple accounts" error - leaves mail the reader
  deleted sitting in another account. The row says which account each of its
  messages is in: `conversationView.MessageAccountIDs` is parallel to
  `MessageIDs`, not the distinct set of accounts, and the two are built together
  so nothing can shift them out of step. Each destination reports on its own,
  the way per-run outcomes already do above. Split filing brings two traps the
  single-folder version never had. A dismissal may cover only the messages that
  are really going: a row whose own account's copy is already in the
  destination, or was skipped for any other reason, stays on the list that holds
  it, and hiding it there hides it for good, because a dismissal lapses only
  when the list stops returning the message and this list never will. And
  "already there" is answered per account for the same reason - the row's
  mailbox speaks for its own copy alone, so it drops that copy from the move
  instead of refusing the whole row, and a row is refused only when no account
  has anything left to move. Every gate that greys out one of these actions
  (`rowSpamState` and anything added beside it) has to answer the same question
  the action does, or the button refuses what the swipe beside it would do. The
  keepalive request budget is the page's, not each move's: destinations share
  `keepaliveMoveChunkBudget` through `keepaliveChunkBudgets`, which hands the
  ones past it nothing rather than a rounded-up share, because a truncated
  background commit must under-claim rather than over-send.
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
  One Message-ID held by two accounts is one message - every copy carrying that
  header is a copy of the same mail, whether the second account fetched it or the
  sender addressed both - so a group resolves whenever some copy can stand in for
  the others, and the only question is which one stays. Recipients answer it when
  they can: an account named in `To` or `Cc` holds a delivery of its own, and one
  that named none merely fetched the mail. When no name decides it - Bcc, a
  mailing list, or several of the user's accounts named at once - placement
  answers instead (the row the group already settled on, then an Inbox copy, then
  the oldest), and the scan reports that group as `resolved_by_placement` rather
  than as a refusal. Keeping the settled row is not a tie-break detail: the
  reader's own filing changes placement, and re-deciding on it would push a
  message they had dealt with back into another account's Inbox, unread, because
  read state lives on the row and is never mirrored onto a hidden copy.
  `duplicateCopyShowsInLists` then answers both halves of the decision the same
  way. Only a row the whole-account lists read - in All Mail, not Junk, not Sent,
  Drafts, or Trash - may stand in as the original, because hiding copies behind a
  Spam-filed row would take the message out of view entirely; and only such a row
  is ever hidden, because hiding takes a row out of its own folder's list too, so
  mail the user filed in an excluded folder or reported as spam would disappear
  from the one place they would look for it. A copy is never hidden behind a row
  of its own account either. Showing a message twice is recoverable; hiding the
  only copy is not. The Trash cleanup narrows the set once more: it moves only
  copies the account holding them was **not** addressed in, because a copy of a
  message that named that account is a delivery that server made rather than a
  fetch it can be told to stop, so hidden and sweepable are deliberately not the
  same set. A scan reports what it decided (`DuplicateScanStats.Outcomes`)
  alongside what it changed, and the settings panel counts the copies one account
  holds of its own mail separately: those are two real messages on one server and
  detection never judges them, which a user looking at mail they see twice cannot
  otherwise tell from a group it never considered.
- New attachment bodies should be indexed from raw `.eml` data and then discarded, not saved as separate attachment blobs.
- `attachment_indexed_at = 0` **is** a reindex queue, and one that publishes
  from stored data only. A pending row means one of two things and only the
  index can tell them apart, so the maintenance worker asks it
  (`Search.MessageIDsIndexed`): a row that already has a document is completed
  as it stands, because enriching it with attachment text needs the raw message
  that production does not fetch in the background
  (`AllowBackgroundAttachmentHydration` stays off); a row with no document is
  indexed from the local raw message when the blob is still there and from the
  record's own fields when it is not. Never clear pending rows in bulk without
  that question - stall recovery, generation rebuilds and re-enabling search on
  a folder all queue rows through this flag expecting documents back, and
  clearing them is how a tenant's search silently ends up holding half their
  mail. Mail that is marked done and holds no document is outside both paths, so
  a drained worker sweeps for it (`requeueMissingSearchDocuments`) behind a
  count comparison whose two sides must describe the same population - a purged
  folder is in neither, or the sweep sees a gap no walk can close and re-walks
  the mailbox forever. A purged folder is nobody's backlog: the queue and the
  sweep both skip it, and the storage page names it separately so a shortfall
  that will not move is not blamed on a worker that was told to leave it.
  What re-downloads message bodies is still only the explicit rebuild - purge
  the folder's documents, then index what the folder has and the index does not
  (`rebuildMailboxSearchIndex`) - offered per account in the folder settings,
  per tenant on the admin database page, and for their own index by the reader
  on the storage page. All three go through `startSearchRebuildForUser` or the
  per-folder call it wraps. Reuse it; do not grow a second one.
- Search runs on one of two backends (`ROLLTOP_SEARCH_BACKEND`): Bleve indexes
  on `/data` (default), or `message_search` rows in PostgreSQL
  (`docs/search-postgres-plan.md`). The service type is the same either way;
  the pg mode never enters the writer gates, stall watchdog, or quarantine
  below, because a transactional row write has nothing for them to guard.
  Everything in this section about Bleve lifecycle applies to the Bleve
  backend only.
- On the Postgres backend, a message's text is stored out of line, so what a
  ranked query costs is decided by how many rows it reads that text for - not
  by how many it returns. Three rules follow, and all three were once broken at
  once, which is how a mailbox that finished indexing started answering searches
  with a gateway timeout. Anything that only concerns the page returned belongs
  outside the layer that ranks and cuts: PostgreSQL projects below the sort that
  feeds a limit, so the weight-class columns written beside the score answered
  for every candidate to report on fifty. Fuzzy matching probes a second copy of
  that text and is a fallback, never a co-equal branch - it runs when the exact
  query finds almost nothing, which is the query that was mistyped, and the gate
  that decides is a bounded count reading a property of the query, so every page
  of one search decides alike. And a caller that needs more hits than a page
  holds must ask for a bigger page, not for more pages: every page re-ranks the
  whole match set, so the loops collecting conversations or resolving a
  whole-filter delete pay the full cost again on each round.
- The search index is derived state and must never hold the mail hostage. A
  Bleve write that fails drops its batch and marks the folders it touched as
  coverage nothing has verified (`search_index_state_known = 0`), which the
  folder list and the admin page already show and the rebuild acts on; it does
  not abort the mailbox, because the index is rebuildable from stored mail and a
  missed IMAP sync window is not. Only a cancelled context and a closing service
  still stop the sync, and both report it with one sentinel
  (`search.IsServiceClosingError`) rather than a second spelling per code path.
  An index that cannot be opened at all is quarantined and replaced on the spot,
  with the tenant's folders marked *before* the directory moves - a crash in
  between must leave a folder to rebuild, never one that claims to be indexed
  into an index that no longer exists. Only errors naming a damaged file qualify
  (`search.IsIndexCorruptionError`); a held lock or a full disk is a passing
  condition whose index is fine, and rebuilding on one of those answers a
  five-second problem with hours of reindexing.
- Exactly one goroutine may open a given tenant's index, and the gate that
  guarantees it (`Service.openGates`) also covers the quarantine. Two
  concurrent `bleve.Open` calls on one scorch directory block on its bolt lock
  indefinitely, and a handle closed as a duplicate takes the live index down
  with it - the surviving cache entry then answers every search with "index is
  closed" until the process restarts. Deduplicating after the open is not
  enough; the open itself has to be serialised.
- Repairing a search index must stay reachable from the admin database page.
  Rolltop is deployed as a container, so an operator with a broken index and no
  shell cannot reach `rolltop reset-search`; the SQLite-era verify/backup/repair
  buttons were rightly removed, that one was not theirs to take with them.
- What a page says about search must come from the backend in force, never from
  the data volume. `search.Service.Backend()`, `PerUserIndexBytes` and
  `FuzzyAvailable` answer for both; walking `/data` answers only for Bleve, and
  on the Postgres backend it reports an index of zero bytes and no missing
  folders for a search that is working. The admin database page and
  `/api/storage` both ask the service. Index upkeep that runs beside the request
  path registers with `Service.StartMaintenance` so the activity view can show
  it — a trigram index being built is minutes of search answering without typo
  tolerance, and a log line is not where the reader who just missed a word will
  look.
- One data directory belongs to one process, and the instance lock is taken
  before anything opens Bleve or the blob store. The database no longer needs it
  — PostgreSQL handles concurrent clients — but Bleve does, and that is now the
  whole justification. A serving start waits for the lock
  (`ROLLTOP_STARTUP_LOCK_WAIT`) because deployments overlap the old and new
  container; `reset-search` keeps failing immediately instead. Do not move any
  open ahead of the lock, and do not make `reset-search` wait.
- One **database** also belongs to one process, enforced separately: the
  directory lock is per volume and misses two deployments with their own volumes
  pointed at one DSN. `PostgresOptions.ExclusiveInstance` holds a session-scoped
  advisory lock for the store's lifetime (`backend/store/instance_lock.go`), on
  the same wait budget, and `reset-search` takes it without waiting. Without it
  both servers start, each start's `MarkRunningSyncRunsInterrupted` stamps the
  other's live sync runs interrupted, and every mailbox is fetched twice. Tests
  leave it off on purpose: they open several stores against one database.
- That lock has to be able to **expire**, and three things make it. A container
  killed without closing its connection — an OOM kill, an evicted pod — leaves
  the backend `idle` and holding the lock, and the operating system's default
  keepalives leave it there for over two hours, during which no start succeeds.
  So the lock session sets its own keepalives (about a minute to notice), the
  holder pings it every `instancePingEvery` so its silence means something, and
  a start whose wait has run out reads `pg_locks`/`pg_stat_activity` and ends a
  session that carries `instanceLockAppName` and has been silent for
  `instanceStaleAfter`. Keep all three: the ping is what distinguishes a running
  holder from an abandoned one, and without it the takeover would hand a live
  server's database to a second one. Only a marked, silent holder is taken over;
  anything else is named in the error, and `ROLLTOP_BREAK_INSTANCE_LOCK` is the
  operator's one-start override for it — which `instanceLockHolder.answering`
  refuses to apply to a holder that pinged inside the window, so an override
  left set in the environment cannot take a running server's database on the
  next overlapping deploy. A stop signal during the wait is reported as a stop,
  wrapping `context.Canceled` so `stoppedDuringStartup` keeps it out of the
  crash report — never as the second-server message; every failure inside the
  claim goes through `stoppedWaiting` first for that reason.
- The holder lookup must stay scoped to the current database. Advisory locks are
  per database and `pg_locks` is per cluster, so an unscoped query can read
  another deployment's session as this database's holder and terminate it. Its
  nullable columns are coalesced so that "not known" reads as answering, never
  as abandoned: a role that cannot see another session's row must make the start
  refuse, not make it end a session it cannot inspect.
- The schema is `backend/store/pgschema/baseline.sql`, applied to an empty
  database and recorded as one row in `schema_migrations` under a checksum over
  its text. It was derived once from the SQLite schema that preceded it; that
  derivation and its translator are gone, so the file is now **hand-owned and
  authoritative — edit it directly**, keeping the conventions its header lists.
  What that reaches is only a database that does not exist yet: editing it
  changes the recorded checksum, and a server whose database was built from an
  older version refuses to start rather than run against a schema it does not
  recognise. Changing the schema of a database already in service needs a
  migration layered on the baseline, which is `postgresMigrations` in
  `backend/store/postgres_migrations.go`: an **append-only** list of numbered,
  checksummed entries, each applied once under the schema lock inside one
  implicit transaction with the row that records it. A database's applied rows
  must always be a prefix of that list, so editing a shipped entry, renumbering
  one, or leaving a gap is refused at startup rather than guessed at. Add a
  migration; never edit the baseline for a database that already exists.
- SQL is written with `?` placeholders and translated to `$1..$n` in the driver
  (`backend/pgbind`). That is deliberate: many statements are assembled at run
  time from fragments, and numbering them in the source would mean every
  fragment knowing how many parameters the fragments before it contributed.
  Write `?`. Mixing the two styles inside one statement is refused rather than
  guessed at — `Rebind` returns a `*pgbind.MixedPlaceholderError`, because
  numbering `?` from `$1` in a statement that already says `$1` binds two
  arguments to one slot and returns another tenant's rows without failing.
- Five PostgreSQL rules the SQLite schema let us ignore, each of which produced
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
  - `TEXT` holds valid UTF-8 and nothing else: an invalid byte sequence or a
    NUL byte is rejected outright (SQLSTATE 22021), where SQLite stored any
    bytes it was handed. Mail carries both, and a header written as raw
    ISO-8859-1 rather than as an encoded word is the ordinary case, not a
    corruption — so text derived from a message is repaired at the parse
    boundary with `mailparse.SanitizeText`, which reads the bad bytes as
    Windows-1252 and leaves the valid UTF-8 around them alone. `Parse`,
    `ParseDisplayBody` and `DecodeTextBytes` already do it for what they
    return; anything that produces message text outside the parser — a plugin
    that decrypted a body, an error string quoting the header it choked on —
    repeats it for its own values before storing them.
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
