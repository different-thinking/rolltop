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
| fuzzy (`Behavior.Fuzzy`) | **shipped, as a fallback**: per-term `pg_trgm` **strict** word similarity (`<<%`) against a distinct-words column (migration 0002), OR-composed with the term's lexeme match; floors 0.35 (balanced) / 0.30 (forgiving) set per query with `SET LOCAL pg_trgm.strict_word_similarity_threshold`, minimum term length 5/4 runes, quoted phrases never fuzz. Strict, because the loose operator scores against the best *substring* of the haystack and German compounds then read as synonyms of their parts: measured on PostgreSQL 16, "rechnung" against a body carrying "kreditkartenabrechnung" scores 0.778 loose and 0.280 strict, while a real typo ("rehcnung") scores 0.385 under both. Extension and trigram index are runtime-optional (`EnsureTrigramSearch`) — absent privileges degrade to exact matching, never block startup. It runs only when the exact query finds fewer than `pgFuzzyFallbackBelow` messages (§5a), and never outranks an exact match (§5b) |
| ranking boosts (subject > from > body > attachments) | `ts_rank_cd` weight array `{D,C,B,A}` |
| recency bias | a bucket `CASE` on `messages.date_unix`, normalized to at most one doubling and **multiplied** into the rank (§5b) |
| sender/contact boosts | `CASE WHEN position(pattern in from_addr) > 0 THEN boost` factors, summed, capped at one doubling and **multiplied** into the rank (§5b) |
| `Hit.Terms`/`Fields`/`QueryTerms` | per-field match detection on the returned page only: run the tsquery against per-field `to_tsvector` of the joined row — page-sized, so cost is bounded by the page |
| `Explain*` (term contributions panel) | reduced fidelity: per-field rank components instead of Bleve's scorer tree; the panel renders what it gets |
| `SimilarMessages` | candidates already come from the store; scoring becomes `ts_rank` of the weighted term set restricted to `message_id = ANY(candidates)` |
| `MatchMessageWithOptions` | the search query `AND message_id = $x` |

## 5a. What the read path costs

A message's vector and its word list are large enough to live outside the heap
row, so the cost of a ranked query is how many rows it reads them for. Ranking
has no way around it — `ts_rank_cd` needs the vector of every match — but the
two things that used to ride along with it did:

- **Weight-class reporting.** The four `ts_filter` columns concern the page that
  comes back, and the table above always said so ("on the returned page only").
  Written in the same `SELECT` as the score they did not behave that way:
  PostgreSQL projects below the sort that feeds a `LIMIT`, so they answered for
  every candidate. Measured on 85,000 messages with 7,083 matches, they cost
  more than three times the ranking they sat next to. They now run in an outer
  layer over the rows the inner one cut to.
- **Fuzzy matching.** The similarity probe reads the word list, a second copy of
  the message's text, for every candidate the query touches — 6.3s against 360ms
  for the same search on that corpus. It earns nothing when the word was spelled
  correctly, so it is now gated: a capped count off the GIN index (~6ms, and
  0.04ms when the term matches nothing) decides, and only a query finding almost
  nothing (`pgFuzzyFallbackBelow`, a handful) pays for typo tolerance. The gate
  reads the query, not the page, so paging a search stays consistent. It was
  fifty until the gate was measured against real searches: terms are ANDed, so a
  misspelled term takes the whole query to zero exact matches, and a threshold
  set at a full page fired for nearly every specific search anyone types — the
  search that least needs it.

The third multiplier is above this package. Callers that need more hits than one
page holds — collecting distinct conversations, resolving a whole-filter delete —
get them by asking again at a higher offset, and each of those asks re-ranks the
whole match set. They now ask for pages of up to `maxHitsPerRequest` (500)
instead of 100, which is the same query for a fifth of the rounds.

## 5b. What the ranking is on, and why the nudges multiply

§8 listed result-quality comparison as an open question, and this is the first
answer it produced. The knobs were mapped from Bleve one for one — the boost
values carried across unchanged — and on this backend they are not knobs at all.

`ts_rank_cd` answers on a scale the query's own width sets. Measured on
PostgreSQL 16 with the weight array above, a two-term query scores **0.51** with
both terms in the subject, **0.10** for an attachment name and **0.033** for a
body mention; a one-term query, 1.8 / 0.1 / 0.2. Beside that, `senderReadBoost`
produces up to **8** and the "normal" recency bias up to **1.6**. Added, as they
were, every gap the text can open is an order of magnitude below them: the top
of a result page was whoever writes most often, in date order, with the search
term acting as a filter rather than a ranking. A body mention from a familiar
sender outranked a subject line from a stranger by two hundred times the margin
the text had to give.

So they multiply, and each is normalized to at most one doubling — sender boosts
divided by `pgSenderBoostCeiling` and capped by `LEAST(…, 1.0)`, recency buckets
divided by `pgRecencyBoostCeiling`. The widest reach a nudge has is then 3x,
against a narrowest measured gap between two field classes of 5.1x (attachment
against subject): familiarity and freshness reorder comparable matches and never
promote a passing mention over a subject line.

`pgRecencyBoostCeiling` is the largest bucket *any* bias produces, not the
largest of the chosen one, and the difference is the whole setting. Dividing
each bias by its own peak maps the freshest bucket of every profile to exactly
1.0, so `light` and `strong` become one curve and the recency-bias control stays
visible while doing nothing. Both ceilings are read from the tables they bound
rather than written down, so changing a bucket cannot leave a constant behind.

Membership needs the same rule made structural rather than arithmetic. A row
reached by similarity alone contributes up to 0.3 per term while an exact body
mention measures 0.033, so an `exact_match` column sorts ahead of the score
whenever the fuzzy fallback is open. Ordering it by arithmetic would have to be
re-argued after every change to a weight, and one weight is already allowed to be
zero — the attachment knob at "off" makes `ts_rank_cd` return 0 for a genuine
match on that class.

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
  It also names the folders included in search that have never been synced
  (`ListUnsyncedSearchFoldersForUser`). That gap is outside the coverage figures
  and cannot be derived from them: both sides count rows in `messages`, so mail
  that was never fetched is in neither, and a mailbox missing whole folders
  reports full coverage. What closes it is a folder setting, not the rebuild
  beside it, which cannot fetch what the sync was never asked for.
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
| C | **Done**: read path — search/match/similar via one SQL spec (`message_search_query.go`), ranking knobs (weights, sender boosts, recency buckets), explain as weight-class matches. query-side compound splitting leans on the indexed split terms; fuzzy shipped via pg_trgm strict word similarity (runtime-optional), gated and outranked by exact matches (§5, §5b) | yes |
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
  not proven equivalent. Partly answered: §5b measured the scale the knobs land
  on here and rebuilt their composition around it, and the fuzzy operator was
  measured against German compounds (§5). What is still unmeasured is the two
  backends side by side on the same mailbox, which is what phase D asks for.
