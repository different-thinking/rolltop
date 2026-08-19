# Search on PostgreSQL

This is the separate plan §10 of `postgres-migration-plan.md` promised for its
phase 7: moving full-text search from the per-tenant Bleve indexes on `/data`
into the PostgreSQL database the relational data already lives in.

## 1. Why, and why now

On 2026-08-19 a 232KB Bleve commit stalled for two minutes and forced the
controlled restart the stall watchdog exists for (see
`console-error-collection.md`). The mechanics: Bleve reads its segments through
`mmap`, the page cache shares the container's 4GiB with the Go heap, and the
836MiB index no longer fits in what the heap ceiling leaves over. Every commit
pages the index in and out; on FUSE-backed storage that turns kilobytes into
minutes. The index grows with every synced message, so tuning
`ROLLTOP_MEMORY_LIMIT` only reschedules the incident.

PostgreSQL manages its own buffer pool, plans around indexes larger than RAM,
and is already the durability story for everything else. Moving search there
retires, in order of how much they cost to keep alive:

- the active-writer stall watchdog and the controlled-restart flow
  (`search.go:711`, `cmd/rolltop/main.go:536`),
- the recovery markers, index quarantine, and offline rebuild orchestration
  (`recovery_marker.go`, `quarantine.go`, `index_repair.go`),
- the write coordinator's byte-budget machinery (`write_coordinator.go`),
- the index footprint measurement and its startup warning
  (`index_footprint.go`, `reportIndexMemoryHeadroom`),
- and eventually the single-process constraint on `/data`, once blobs are the
  only thing left there.

## 2. Prerequisite: incremental schema migrations (WP7 residue)

The production database now exists, and `baseline.sql` is frozen by its own
checksum: `verifyPostgresBaseline` (`postgres.go:406`) refuses to start when
the recorded checksum differs from the binary's, and there is no upgrade path.
Adding `message_search` — adding *any* table — needs the mechanism
`migrations.go` says is missing and the Postgres plan scheduled as its phase 5.

Design, copying the `plugin_migrations` model as that plan instructs:

- **The baseline never changes again.** Its checksum stays the recorded
  identity of the database's origin. Every schema change from now on is a
  numbered migration layered on top.
- A migration is Go data: `{version, statements}`, ordered, append-only,
  checksummed with the existing `schemaChecksum`. Shipped entries are
  immutable; a checksum mismatch on an applied row is a refusal, exactly as
  for the baseline and for plugin migrations.
- Rows live in the existing `schema_migrations` table (scope `postgres`,
  version `0001-...`), so a database created before the mechanism needs
  nothing retrofitted.
- Startup order in `ensurePostgresSchema`: verify the baseline row as today,
  then compare applied rows against the binary's list. Outstanding migrations
  are applied in order under the existing advisory schema lock, each as one
  simple-protocol script whose last statement inserts its own
  `schema_migrations` row — the same atomicity trick `applyPostgresBaseline`
  documents, so a cancelled startup cannot leave DDL without its row.
- A fresh database gets baseline plus all migrations in sequence; an
  up-to-date database answers with the same single read as today.
- A database carrying rows the binary does not know (downgrade) is a refusal
  with a message naming the newer version, not a silent start.

## 3. Schema (migration 0001)

```sql
CREATE TABLE message_search (
    message_id bigint PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    user_id    bigint NOT NULL,
    tsv        tsvector NOT NULL
);
CREATE INDEX idx_message_search_tsv  ON message_search USING GIN (tsv);
CREATE INDEX idx_message_search_user ON message_search (user_id);
```

Deliberately narrow:

- **No volatile flags.** `is_read`, `is_starred`, `mailbox_id` and the other
  filter fields live in `messages` and change constantly; duplicating them is
  the staleness bug Bleve forces and SQL does not. Every query joins
  `messages` — filters and flags are always current, and flag changes stop
  needing index writes at all.
- **Deletes are the cascade.** Deleting a message row deletes its search row;
  `DeleteMessages`/`PurgeMailbox` in the Postgres backend reduce to explicit
  `DELETE ... USING messages` only for the paths that prune search rows
  without deleting messages (mailbox visibility toggles).
- `user_id` is duplicated (immutable, never reassigned) so the planner can
  bitmap-AND the GIN scan with the tenant restriction without a join.

The `tsv` is built app-side from the same bounded text the Bleve document uses
today (`document_limits.go` limits stay), with `setweight`:

| weight | content |
|---|---|
| A | subject, plus the app-side compound-split subject terms |
| B | from/to/cc (address + display-name terms), from-domain terms |
| C | body text (bounded to `maxIndexedBodyBytes`) |
| D | attachment names, types, and extracted attachment text |

Tokenization is app-side, not PostgreSQL's: each stream is normalized with the
same `normalizeSearchText` the query side runs before it reaches
`to_tsvector('simple', …)`. Leaving it to the parser keeps an address, URL, or
host as a single lexeme, so a body mentioning `kontakt@firma-beispiel.de`
would be unreachable by "firma" — where the Bleve tokenizer split it. One
tokenizer on both sides is the only way index and query agree. Runs longer
than PostgreSQL's 2000-byte lexeme ceiling are split rather than dropped.

The four streams also share a combined 384KiB input budget, filled in weight
order: PostgreSQL rejects a tsvector over 1MiB outright, and that error is not
recoverable — the repair path re-projects the same message and fails again.
Measured worst case is a doubling (409KB of short distinct words → 819KB
vector), so the budget cannot reach the limit; a message that hits it loses
attachment text before body text and never loses its subject.

Text search configuration: `simple` (no stemming — closest to
Bleve's standard analyzer, keeps exact-match semantics and the
app-side German compound splitting doing the work it already does). Evaluate
adding `german`/`english` stemmed lexemes as a second `setweight` pass once
result quality can be compared side by side; that is a tuning decision to
measure, not to guess — it roughly doubles the tsv.

## 4. Backend selection and the write path

`search.Service` stays the single type every consumer holds (`web.Server`,
`syncer.Service`, plugin hosts); it already dispatches on an internal mode
(`perUser`). A new mode, selected by `ROLLTOP_SEARCH_BACKEND=bleve|postgres`
(default `bleve` until the cutover phase), routes:

- `IndexMessage(s)` → batched `INSERT ... ON CONFLICT (message_id) DO UPDATE`,
  computing `tsv` in SQL from bounded text parameters. The upstream batching
  (25 docs / 8MiB) stays; the coordinator's reservation accounting is
  bypassed — a Postgres write does not hold an mmap writer gate.
- `DeleteMessages*`, `PurgeMailbox*` → `DELETE` with progress callbacks fed
  from row counts.
- `CountUserMessages`, `CountMailboxMessages`, `MessageIDsIndexed`,
  `MailboxMessageIDs` → plain SQL against `message_search` (+ join for
  mailbox scope).
- `DropUser` → `DELETE WHERE user_id`.
- `PerUserIndexBytes` → `pg_column_size` aggregate; the admin page keeps its
  number.

The stall watchdog, recovery markers, and quarantine paths are not entered in
Postgres mode. They are deleted only in the retirement phase, so a rollback to
Bleve keeps working machinery.

## 5. The read path

`parseQuery` (`search.go:2805`) is Bleve-independent and is reused verbatim.
Translation of the parsed query:

| today (Bleve) | Postgres |
|---|---|
| filter operators (`is:`, `has:`, `lang:`, `before:` …) | `WHERE` clauses on the joined `messages` row |
| `from:`/`to:`/`cc:`/`subject:`/`filename:` fields | `ILIKE` on the joined columns, forced to the database collation so folding covers more than A-Z (the columns are `COLLATE "C"`) |
| free text, unquoted | AND of lexemes, prefix match (`:*`) on the final term |
| free text, quoted | phrase query (`<->` / `phraseto_tsquery`) |
| negated terms | `AND NOT (tsv @@ ...)` |
| fuzzy (`Behavior.Fuzzy`) | **shipped**: per-term `pg_trgm` word similarity against a distinct-words column (migration 0002), OR-composed with the term's lexeme match; floors 0.35 (balanced) / 0.30 (forgiving) set per query with `SET LOCAL`, minimum term length 5/4 runes, quoted phrases never fuzz. Extension and trigram index are runtime-optional (`EnsureTrigramSearch`) — absent privileges degrade to exact matching, never block startup |
| ranking boosts (subject > from > body > attachments) | `ts_rank_cd` weight array `{D,C,B,A}` |
| recency bias | multiply by an exponential decay on `messages.date_unix`, in the `ORDER BY` expression |
| sender/contact boosts | `CASE WHEN from_addr = ANY($senders) THEN boost` factors |
| `Hit.Terms`/`Fields`/`QueryTerms` | per-field match detection on the returned page only: run the tsquery against per-field `to_tsvector` of the joined row — page-sized, so cost is bounded by the page |
| `Explain*` (term contributions panel) | reduced fidelity: per-field rank components instead of Bleve's scorer tree; the panel renders what it gets |
| `SimilarMessages` | candidates already come from the store; scoring becomes `ts_rank` of the weighted term set restricted to `message_id = ANY(candidates)` |
| `MatchMessageWithOptions` | the search query `AND message_id = $x` |

## 6. Backfill and cutover

No data migration tooling: the rebuild machinery that exists is the backfill.
On first start in Postgres mode, tenants whose `message_search` is empty while
search-visible messages exist are marked pending through the same store call
the quarantine flow uses today; the syncer's repair path re-indexes through
`Search.IndexMessages`, which now writes rows — attachment text re-extracted
from blobs exactly as after a Bleve quarantine. The Bleve directories stay on
`/data`, untouched, as the rollback: flipping the flag back serves the old
index, stale for the interim, and the existing repair closes the gap.

## 6a. What the pages say about it

The two backends keep the index in places a page cannot compare, so nothing
that reports on search may measure the data volume: on this backend the volume
holds no index, and a walk of it reports zero bytes and no missing folders for
a search that is working perfectly. Everything asks the service instead —
`Backend()`, `PerUserIndexBytes` (which sums `pg_column_size` over the tenant's
rows behind a one-minute cache) and `FuzzyAvailable`.

- **`/settings/account/general/storage`** names the backend, the measured size,
  the coverage (`IndexMessageCount` of `FullTextSearchMessageCount`), whether
  typo tolerance is answering, how many folders are waiting to be indexed, and
  offers the reader a rebuild of their own index —
  `POST /api/storage/search-index/rebuild`, which is `startSearchRebuildForUser`
  for the signed-in user and takes no user id. The Bleve segment breakdown
  renders only on the Bleve backend, which is the only one with files.
- **`/activity`** shows index upkeep that runs beside the request path as
  non-cancellable worker rows (`Service.StartMaintenance`; the trigram index
  build is registered whole-server, the coverage check per tenant), plus a note
  counting the folders still waiting. Without it, a trigram build is minutes of
  search silently answering without typo tolerance.
- **`/admin/database`** stays the whole-server view — connection, pool, free
  space, log tail, and a rebuild for any tenant — and names the backend so the
  volume's `index_bytes` beside it cannot be read as the search index when it
  is not. Its PostgreSQL migration console was removed with this work: it
  rehearsed a schema against an empty target, which a serving database is not.

## 7. Phases

| phase | contents | PR-sized? |
|---|---|---|
| A | **Done**: incremental schema migrations (§2) — `backend/store/postgres_migrations.go` | yes — independently valuable |
| B | **Done**: migration 0001, Postgres write path + counts/ids/purge/drop, backfill trigger, `ROLLTOP_SEARCH_BACKEND` flag | yes |
| C | **Done**: read path — search/match/similar via one SQL spec (`message_search_query.go`), ranking knobs (weights, sender boosts, recency buckets), explain as weight-class matches. query-side compound splitting leans on the indexed split terms; fuzzy shipped via pg_trgm word similarity (runtime-optional) | yes |
| D | flip the default after comparing result quality on real mail, README/compose, observe | small |
| E | retire Bleve: delete the watchdog/quarantine/coordinator/footprint machinery, drop the index from `/data`, revisit the single-process constraint | yes |

## 8. Open questions

- **Extensions at the hoster.** Fuzzy detects `pg_trgm` at startup
  (`EnsureTrigramSearch`) and degrades to exact matching when the managed
  database (`hpr-…`) withholds `CREATE EXTENSION` — the startup log says which
  way it went. `SELECT name FROM pg_available_extensions WHERE name = 'pg_trgm'`
  answers whether the hoster ships it at all; `unaccent` remains unused so far.
  Core FTS needs nothing beyond stock PostgreSQL.
- **Database sizing.** The tsv column and GIN index move ~the index's bytes
  into the database. The hoster sizing answers in `postgres-migration-plan.md`
  §12 were collected before this; re-check the plan's growth numbers.
- **Result-quality comparison.** Before phase D, run the same queries against
  both backends on a real mailbox and compare; the ranking knobs are mapped,
  not proven equivalent.
