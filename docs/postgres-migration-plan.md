# PostgreSQL Migration Plan

Status: proposal, not started. This document describes how to move Rolltop's
relational storage from per-tenant SQLite files to a single PostgreSQL
database, what has to change in the code, where the risks are, and what the
migration deliberately does not cover.

## 1. Why

The production data volume is CephFS. SQLite in WAL mode requires a coherent
shared memory mapping of the `-shm` file across all connections; CephFS with
`client cache = auto` does not guarantee that coherence. The result is the
observed recurring corruption and disk-I/O errors, and it is documented
SQLite behavior on network filesystems, not an edge case. Roughly 2,000 lines
of salvage/repair/integrity machinery in this repository exist only to
compensate for that.

PostgreSQL removes the filesystem from the application's problem space
entirely: Rolltop speaks TCP to a database whose storage is someone else's
(working) responsibility — ideally a managed instance, alternatively a
Postgres container on an RBD block device. **Postgres on CephFS itself would
inherit the same class of problems and is explicitly out of scope as a
deployment target.**

Bleve search indexes and raw `.eml`/attachment blobs are *not* part of this
migration (see §9 and §10). Blobs are write-once whole-file reads and are fine
on CephFS. Bleve is not fine on CephFS (mmap'd scorch segments) and is the
follow-up phase.

## 2. Current state (inventory)

Verified against the tree as of this writing:

| Surface | Size |
| --- | --- |
| `backend/store` | 86 non-test files, ~18.8k LOC (+ ~12.6k test LOC) |
| Query call sites (`ExecContext`/`QueryContext`/`QueryRowContext`) | 663 non-test, ~1,000 incl. tests |
| Schema migrations | system 001–004, user 001–035, plus named side migrations and plugin migrations |
| `CREATE TABLE` statements / resulting tables | 87 statements across the migrations, 75 tables in the final schema (55 core + 20 from plugins) |
| Production triggers | 1 (`messages_clear_duplicate_pointer`, `migration_user_033.go`) |
| `AUTOINCREMENT` | 54 (all in migration DDL) |
| `INSERT OR IGNORE` | 13 |
| `ON CONFLICT ... DO UPDATE` upserts (`excluded.`) | 214 references |
| `COLLATE BINARY` / `COLLATE NOCASE` | 58 / 1 |
| `PRAGMA` (non-test backend/cmd) | 12 sites |
| `LastInsertId` | 15 |
| Plugins executing their own SQL | 11 backends |
| Corruption/salvage/repair machinery | ~2,050 LOC (`salvage.go`, `corruption.go`, `recover_db.go`, `repair_db.go`, `startup_integrity.go`, `repair_marker.go`, `backup_db.go`) |

Architecture today: `OpenServer` opens the system DB (`/data/rolltop.db`);
`UserStore`/`dataDB` lazily open one SQLite file per tenant
(`/data/users/<id>/rolltop.db`). Tests mostly use `Open()`, which creates a
*combined* schema in one file — this matters below.

## 3. Decisions (with recommendations)

### 3.1 One database, not schema-per-user — recommended

Options considered:

- **(A) One database, one schema, `user_id` scoping.** Every user-schema
  table already carries `user_id` (AGENTS.md makes that a hard rule), and the
  combined schema already exists and is exercised by the whole test suite via
  `Open()`. The per-user file split existed for writer sharding (irrelevant in
  Postgres), corruption blast-radius (irrelevant), and purge-by-file-delete
  (replaced by `DELETE ... WHERE user_id = ?`, which `account_purge.go`
  already does row-wise anyway).
- **(B) Postgres schema per user.** Mirrors today's layout, cheap purge via
  `DROP SCHEMA`, but multiplies migration runs, breaks connection pooling
  (search_path juggling), and buys nothing the `user_id` columns don't
  already provide.

**Recommendation: (A).** It deletes the split-mode machinery
(`userStores` cache, `mirrorUser`, `PrepareUserStores`, per-tenant health
latch) instead of porting it.

### 3.2 Hard cutover, no dual-backend support — recommended

Per `docs/local-sqlite-maintenance.md`, this deployment is currently the only
running instance. Maintaining two SQL dialects behind an abstraction layer is
the single most expensive way to do this migration and benefits nobody.
The SQLite driver survives only inside the one-shot data migration command
(§7), then gets removed from the server path.

Consequence to accept explicitly: Rolltop stops being a single-container app.
The README's deployment story changes to "app container + Postgres" (compose
file provided) or "app container + managed Postgres DSN".

### 3.3 Driver: `pgx/v5` via `database/sql` (stdlib adapter) — recommended

- Keeps all 663 call sites on `*sql.DB`/`*sql.Tx`, so the change per call
  site is the SQL text, not the API.
- Keeps the compiled-plugin ABI unchanged: plugin hooks receive `*sql.DB`
  (`backend/plugins/compiled_hooks.go`) and keep working as long as their SQL
  is ported.
- Native pgx (batching, `CopyFrom`) is used only inside the data migration
  tool where it pays off.

### 3.4 Database collation: `C` — recommended

Create the database with `LC_COLLATE=C` (or use `COLLATE "C"` defaults).
This reproduces SQLite's byte-order `ORDER BY` and byte-exact comparisons,
which the code assumes in 58 explicit `COLLATE BINARY` sites and implicitly
everywhere else. Human-facing sorting that wants locale rules should opt in
explicitly rather than the reverse. The single `COLLATE NOCASE` site becomes
`lower(...)` or `citext`.

Hoster constraint (2026-08): databases are created with the cluster default
locale, not configurable per database. That is acceptable, for two reasons:

1. **Equality is safe regardless.** Under any *deterministic* collation —
   which every libc locale collation is — Postgres compares text equality
   byte-wise. The 58 `COLLATE BINARY` comparison sites and every implicit
   `=` on hashes/message-ids therefore keep SQLite semantics even under the
   cluster default. Only a nondeterministic default would break this, and
   both preflight twins probe for it behaviorally (see the note below).
2. **Ordering is pinned per column.** The WP2 baseline declares
   `COLLATE "C"` on text columns instead of relying on a database-level
   default (`key text COLLATE "C"`), which also applies to their indexes.
   This confines the locale question to the DDL in one place. Queries that
   `ORDER BY` an expression rather than a bare column add
   `COLLATE "C"` explicitly where byte order matters (keyset pagination);
   purely human-facing sort order may use the locale default deliberately.

`scripts/pg-preflight.sql` verifies all of this against the actual target
database (version/encoding gate, byte-exact equality, column- and
index-level `COLLATE "C"` byte ordering, extensions, UTF-8 strictness, and
the SQL features WP3 relies on). The same checks also run from the admin
Database page ("PostgreSQL preflight", `backend/pgpreflight`), which
exercises the real app-to-database network path and reports its round-trip
latency — prefer that over the bastion route. Run either before phase 1;
both were validated against a stock Postgres 16, and `TestTwinsAgree` keeps
them in sync. Only one preflight runs at a time (they share a scratch
schema); do not run the script and the page against one database at once.

Note on how determinism is verified: a catalog lookup on the `pg_collation`
row named `default` is *not* a check — that row is a pinned placeholder whose
`collisdeterministic` is hard-wired true. Both twins therefore probe the
behavior (`'a' = 'A'`, ligature, padding, accent, normalization). Postgres
also does not currently accept a nondeterministic collation as a database
default — verified on 16, where even an ICU `und-u-ks-level1` database still
compares `'a' <> 'A'` — so these probes guard against a future relaxation
rather than a live hazard.

The startup fail-fast check becomes: text equality is byte-exact and the
baseline's own `COLLATE "C"` columns are present.

## 4. Work packages

### WP1 — Connection, config, startup

Shipped so far (`backend/store/postgres.go`): `OpenPostgres` connects through
`pgx/v5`'s `database/sql` adapter, sizes the pool (default 10, half the hosted
role's limit of 20), and puts the WP2 baseline into an empty database. It does
**not** run the SQLite migration chain — PostgreSQL gets the squashed baseline
recorded as one `schema_migrations` row, so drift is caught by the same
checksum rule rather than a second mechanism. Three states are distinguished
rather than collapsed into "create if missing", because the two failure states
lose data quietly:

| Database | Behaviour |
| --- | --- |
| empty | apply the baseline, record its checksum |
| carries the recorded baseline | verify the checksum, open |
| has tables but no recorded baseline | refuse: this is somebody else's database |

The baseline is applied as one simple-protocol query rather than split on
semicolons — it contains a dollar-quoted function body full of them, and
PostgreSQL wraps a multi-statement simple query in one implicit transaction, so
a failure halfway through leaves no partial schema. A test asserts that.

Still to do, deliberately held until WP3 makes the store serve queries:

- `backend/config/config.go`: replace `ROLLTOP_DB_PATH` with
  `ROLLTOP_DATABASE_URL` (standard Postgres DSN). Add pool knobs
  (`ROLLTOP_DB_MAX_CONNS`, default ~10). Remove
  `ROLLTOP_STARTUP_INTEGRITY_CHECK`. Landing the environment variables before
  the store can answer a query would advertise a configuration that starts a
  server which cannot serve.
- `backend/store/store.go`: `open()` loses the SQLite DSN parameters, the
  `MkdirAll`, the corruption classification on migrate, and split mode.
  `dataDB`/`mustDataDB`/`UserStore`/`UserDB` remain as API but resolve to the
  one shared pool — callers do not change.
- Startup ordering: the `flock` instance lock (`cmd/rolltop/instance_lock.go`)
  **stays** — Bleve still requires a single process on `/data` (README calls
  it the stricter constraint). Only its justification narrows.
- Wait-for-database loop at startup (Postgres may come up after the app
  container; today's code never needed this).

### WP2 — Schema baseline (squash) — **done**

Do **not** port 40+ incremental migrations. Freeze the current combined
schema (what `Open()` produces after all system, user, side, and plugin
migrations) and write it as one Postgres baseline, keeping the existing
`schema_migrations` checksum runner (it is dialect-portable).

Shipped as `backend/store/pgschema`. The baseline covers the system, user,
side, *and* plugin schemas — including the file-backed migrations under
`plugins/*/migrations/`, which `store.Open()` alone does not apply and which
contribute 20 of the 75 tables. Plugin migrations run when a plugin is
enabled, not at open, so the baseline has to contain every table any plugin
could create: after the cutover, enabling one would otherwise replay
SQLite-dialect DDL against PostgreSQL.

The baseline is **derived, not written**:
`TestBaselineMatchesSQLiteSchema` opens a fully migrated combined SQLite
store, translates its `sqlite_master` contents, and fails when the committed
`baseline.sql` differs — so a SQLite migration landing during the transition
cannot silently leave the two schemas apart (`-update` regenerates).
`TestBaselineAppliesToPostgres` then applies it to a real server and asserts
the properties the migration depends on: every text column C-collated, the
foreign keys present, identity sequences accepting `setval`, and the
translated trigger behaving like the SQLite original.

Two things the derivation surfaced that hand-writing would likely have
missed:

- PostgreSQL requires a referenced column list to be backed by a unique
  constraint or index, and this schema has composite keys —
  `mailboxes(user_id, account_id, id)` — whose uniqueness comes from a
  `CREATE UNIQUE INDEX`. Inline foreign keys are therefore rejected. The
  baseline emits four phases: tables, indexes, foreign keys, triggers.
- The final schema has 75 tables (55 core plus 20 from plugins), not the 87
  the inventory above counts; that number counts `CREATE TABLE` statements
  across migrations, several of which recreate the same table.
- The plugin migrations carry SQLite-only constructs the core schema does
  not: `WITHOUT ROWID` (dropped — PostgreSQL has no equivalent storage form)
  and one `NOT GLOB` check, translated deliberately to a regex match. The
  translator fails closed on any other SQLite-only construct rather than
  passing it through, because a silently dropped CHECK is a constraint the
  database stops enforcing.

Translation rules for the baseline:

| SQLite | PostgreSQL |
| --- | --- |
| `INTEGER PRIMARY KEY AUTOINCREMENT` | `BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY` |
| `INTEGER` (unix timestamps, counters, 0/1 booleans) | `BIGINT` (keep 0/1 ints — avoids touching every `Scan`; genuine `BOOLEAN` conversion is a later cleanup) |
| `TEXT` | `TEXT` |
| `BLOB` (2 columns) | `BYTEA` |
| `COLLATE BINARY` in DDL | drop (DB is `C`-collated) |
| `COLLATE NOCASE` | expression index on `lower(col)` |
| trigger in `migration_user_033.go` | PL/pgSQL function + trigger, same semantics (AGENTS.md documents why this lives in SQL, keep it there) |

Plugin migrations: `plugins.Migration.Statements` get ported per plugin;
`EnsureColumns` exists because SQLite lacks `ADD COLUMN IF NOT EXISTS` —
Postgres has it, so the mechanism simplifies but the interface stays.

### WP3 — Dialect codemod over 663 call sites

All queries are constant string literals; this is mechanical but must be
reviewed, not fully automated blindly:

1. **Placeholders `?` → `$1..$n`.** Includes the dynamic
   `IN (...)`-builders (`sqlPlaceholders` in `account_purge.go:217`,
   `int64ListPlaceholders` in `message_lists.go`) which must become
   position-aware, or better: replace `IN (…)` builders with `= ANY($1)` and
   pass an int64 slice — pgx supports array binding, which deletes the
   builder pattern entirely. **Recommended.**
2. **`INSERT OR IGNORE` (13) → `INSERT ... ON CONFLICT DO NOTHING`.** Where
   the code inspects `RowsAffected` to detect "was it new" (e.g.
   `new_mail_events.go:54`), semantics are preserved (`DO NOTHING` reports 0
   affected rows).
3. **Upserts:** SQLite's `ON CONFLICT (…) DO UPDATE SET … excluded.…` is
   already PG syntax. Verify each conflict target has a matching unique
   index in the baseline schema (PG enforces this; SQLite did too, so
   mismatches are unlikely but each of the 41 sites gets checked).
4. **`LastInsertId` (15) → `RETURNING id`** with `QueryRowContext`.
5. **`COLLATE BINARY` in queries (inbox_arrivals dedup, etc.)**: drop — the
   `C` database default makes `=` byte-exact.
6. **`PRAGMA` sites:**
   - `message_indexing.go` (`synchronous=FULL` pinning + `wal_checkpoint(FULL)`
     before search indexing): the whole durability dance collapses —
     a committed PG transaction *is* the durable prerequisite. Delete the
     dedicated-connection logic.
   - `table_info` (`plugins.go:330`, `migration_search_preferences.go:57`) →
     `information_schema.columns`.
   - `quick_check`, `foreign_keys` toggles: deleted with their files (§6).
7. **`sqlite_master`** (only `salvage.go`): deleted with the file.
8. **`strftime`** (only in `migration_user_022`): gone via squash.
9. **`LIKE`:** the three live sites (`contacts.go`) already wrap both sides
   in `lower()`, so PG's case-sensitive `LIKE` changes nothing. Audit any
   future `LIKE` for this.

Suggested mechanics: one PR per store file group, each converting the SQL and
its tests together, so review stays possible. A `go vet`-style CI grep that
rejects `?`-placeholders, `PRAGMA`, `INSERT OR`, `AUTOINCREMENT` in
non-migration code keeps regressions out during the transition.

### WP4 — Behavioral changes in the store layer

- **Transactions:** drop the `BEGIN IMMEDIATE` model (`_txlock=immediate`).
  Postgres has real MVCC concurrency; the 65 `BeginTx(ctx, nil)` sites work
  as-is. Add retry-on-serialization-failure only if we later raise the
  isolation level (default `READ COMMITTED` matches current semantics
  closely enough).
- **Busy/locked handling:** `busy_timeout`, `IsCorrupt`, `NoteError`
  health-latching disappear. Error classification narrows to
  connection-level errors (retryable) vs constraint violations.
- **`sync_runs` / syncer:** no logic change, but note the latency shift in
  §8 — per-row loops that were free against a local file now pay a network
  round trip each. The syncer's batch writes should be spot-checked with
  timing logs after cutover; candidates for batching already exist
  (`contacts_batch.go` pattern).

### WP5 — Admin/maintenance surface rework

`backend/web/database_maintenance.go` + `cmd/rolltop` subcommands:

| Today | After |
| --- | --- |
| Integrity check job (`PRAGMA quick_check` over every file) | remove, or replace with a trivial connectivity/`pg_is_in_recovery` health card |
| Backup job (`VACUUM INTO` per file) | remove from app; document `pg_dump` (and provider snapshots) as the supported path. If the in-app button must survive, shell out to `pg_dump` (adds the client tools to the image) — recommended: drop it |
| Schedule-repair-at-restart marker flow | remove |
| `check-db`, `repair-db`, `recover-db`, `backup-db` subcommands | remove (`backup-db` may keep a thin `pg_dump` wrapper if desired) |
| Crash log on `/data` | keep (unrelated) |

README sections "Backups", "Corruption", "Shutdown And Restart" get rewritten
accordingly — shutdown no longer needs the WAL-checkpoint grace budget.

### WP6 — Plugins

The 11 SQL-executing plugin backends compile against `*sql.DB` and need the
same WP3 treatment (their `AUTOINCREMENT`/`ON CONFLICT` DDL sits in their
migration files). Because plugins load via `-buildmode=plugin`, the main
binary and plugins must keep identical module versions — adding `pgx` to
`go.mod` is a lockstep change across all plugin builds (CI already builds
every plugin, `.github/workflows/ci.yml:101`, so drift is caught). CGO stays
required by `-buildmode=plugin` regardless of dropping `mattn/go-sqlite3`, so
the Dockerfile build stages are unchanged in shape.

### WP7 — One-shot data migration tool

**Open obligation, to be settled before the first durable Postgres database
exists.** `OpenPostgres` records the baseline as one `schema_migrations` row and
refuses to start when the recorded checksum differs from the running binary's.
That is right while every Postgres database is a throwaway test one, but the
baseline is *designed to be regenerated in place*, so the same signal will mean
two different things once a real database exists: "the schema was tampered with"
and "this binary is newer than the database". The second needs an upgrade path,
not a refusal. Whatever WP3 does to the schema — the `lower()` expression index
is already queued — regenerates the baseline, so this has to be decided in the
same phase that creates the first database worth keeping. The likely shape is
incremental Postgres migrations layered on the baseline, with the baseline row
pinned to the version it was created at rather than to the current file.

New subcommand `rolltop migrate-to-postgres`:

1. Takes the instance lock (server must be stopped) and opens the system
   SQLite file plus every `data/users/<id>/rolltop.db` **read-only** with the
   existing driver.
2. Requires an **empty** target database; runs the WP2 baseline.
3. Streams tables in FK order via pgx `CopyFrom` (system tables, then per
   user). Sizing input (2026-08): the deployment holds 200k+ messages and
   growing — with the 4 KB preview compaction that is roughly 1–2M rows and
   a low single-digit GB, which `CopyFrom` moves in minutes. The cutover
   window is therefore dominated by verification, not transfer, and dry
   runs against a production copy are cheap to repeat.
4. **Sanitizes text on the way over** — see §8.1, this is the step most
   likely to surface dirty data. Every sanitized value is logged with table,
   rowid, and column.
5. Resets identity sequences (`setval`) to `max(id)+1` per table.
6. Verifies: row counts per table per user, plus spot checksums (e.g.
   sum of `size`, count by `mailbox_id`) — printed as a report.
7. All-or-nothing: any failure ⇒ drop and re-run. No partial resume.

The SQLite files are left untouched and serve as the rollback (§11).
Skipped-as-corrupt tenant databases (the health latch case) must be repaired
with today's tooling *before* migrating — the tool refuses to silently skip a
tenant.

### WP3 query obligations from the baseline

Two translations in the baseline change how queries must be written, and both
are easy to regress by porting a query mechanically:

- **`COLLATE NOCASE` became `lower(col)`.** `idx_contacts_user_name` is an
  expression index on `(user_id, lower(display_name))`. A query written
  `ORDER BY display_name` cannot use it; it has to say `lower(display_name)`.
  The store's contact queries already do, but a new one would not by default.
- **Identity columns are `GENERATED BY DEFAULT`**, not `GENERATED ALWAYS`, so
  an explicit id on insert is accepted. That is what lets WP7 copy ids
  verbatim; do not "tighten" it to ALWAYS.

### WP8 — Tests and CI

The biggest hidden cost. ~12.6k LOC of store tests plus the web/syncer suites
all call `store.Open(tempfile)`.

- `backend/store/pgtestdb` (shipped) hands out one empty database per test from
  `TEST_DATABASE_URL` and drops it in `t.Cleanup`. It skips when the variable
  is unset and *fails* when it is set but unusable, so a broken CI service
  cannot report a green suite that verified nothing.
- The **template database** the plan called for is not built yet, because the
  measurement it was justified by came out differently: applying the baseline
  to an empty database takes ~360 ms locally. A template clone would save most
  of that, but only matters once the suite has hundreds of store tests — which
  is exactly when WP3 converts them. Build it then, with the real number in
  hand, rather than now against five tests. When it lands, template creation
  happens once per `go test` process (sync.Once + advisory lock for parallel
  packages).
- New helper `storetest.Open(t)` wrapping `pgtestdb` and `OpenPostgres`, added
  alongside the first converted package so it has callers on the day it lands.
- `Open(path)` callers migrate to `storetest.Open(t)` mostly mechanically.
- CI (`ci.yml`, `pr.yml`): add a `postgres:17` service container, set
  `TEST_DATABASE_URL`. Local dev: `docker compose -f compose.dev.yml up db`
  documented in README/AGENTS.md.
- Tests that specifically exercise SQLite corruption/salvage/backup are
  deleted with their subjects.

### WP9 — Docs and deployment

- README: storage model, configuration, backups, corruption section
  (largely deleted), new compose example with a Postgres service and a named
  volume **on RBD/local disk, never CephFS**, `stop_grace_period` note
  simplified.
- AGENTS.md: replace SQLite-specific guidance (writer turns, WAL) with
  Postgres equivalents; keep the `user_id`-scoping rule — it is now the
  *only* tenant isolation layer, which makes review discipline on new
  queries more important, not less.
- `docs/local-sqlite-maintenance.md`: superseded; keep for the Bleve parts,
  rename accordingly.

## 5. Suggested phasing

| Phase | Content | Depends on |
| --- | --- | --- |
| 0 | **Done** (shipped on `main`, `f9f686d` + `0552945`): WAL without the shared `-shm` index via `locking_mode=exclusive` and one connection per database, auto-detected from the filesystem superblock (`backend/store/access.go`) with a `ROLLTOP_SQLITE_ACCESS` override; live maintenance rerouted through the owning handle. Deviation from this plan's sketch: `synchronous` stays `NORMAL`, so durability of the most recent commits still depends on checkpoint-time fsync — acceptable residual risk for the interim, since the corruption vector (shm coherence) is gone. | — |
| 1 | WP2 baseline + CI Postgres service (**done**). WP1's config/pool and WP8's per-test template-database helper both move to phase 2, where the store conversion gives them a consumer; landing either earlier would be unused code | ~~hoster answer: managed PG or RBD~~ resolved, see §12 — managed PG 16 confirmed; collation provisioning (§3.4) still to coordinate |
| 2 | WP1's connection/schema layer (**done**: `OpenPostgres`, `pgtestdb`), then WP3 + WP4 store conversion, package by package, tests green against PG | 1 |
| 3 | WP6 plugins | 2 |
| 4 | WP5 maintenance-surface removal + WP9 docs | 2 |
| 5 | WP7 migration tool + dry run against a copy of production data | 2 |
| 6 | Cutover (§11) | 3–5 |
| 7 | Bleve → Postgres FTS (§10, separate plan) | 6 |

Phases 2–4 are parallelizable per package. Realistic effort: phase 1 ~2–4
days, phase 2 the bulk (1–2 weeks of focused work given test conversion),
phases 3–5 ~1 week combined, plus a dry-run/cutover day. The corruption
machinery deletion (§6) lands as its own satisfying PR.

## 6. Code that gets deleted

- `backend/store/salvage.go` (731), `corruption.go` (342),
  `repair_marker.go` (165)
- `cmd/rolltop/recover_db.go` (302), `repair_db.go` (187),
  `startup_integrity.go` (173), `backup_db.go` (144)
- Split-mode plumbing in `store.go` (userStores cache, `mirrorUser`,
  `PrepareUserStores`, health latch), the `synchronous`/checkpoint logic in
  `message_indexing.go`, the maintenance jobs in
  `backend/web/database_maintenance.go`
- Their tests

Net: roughly 2,500+ LOC of production code whose only job was surviving
SQLite-on-CephFS.

## 7. What deliberately does not change

- **Blob store** (`backend/blob`): stays on `/data` (CephFS). Write-once
  files, no locking, no mmap — appropriate workload for CephFS.
- **Bleve** stays on `/data` *for now* (see §10) — therefore the instance
  lock, the search recovery marker flow, and the single-process constraint
  all stay until phase 7.
- **Store API**: `*Store` method signatures, `dataDB` call shape, plugin
  hook signatures (`*sql.DB`).
- Session/auth model, crypto, IMAP/SMTP paths: untouched. (Token hashes are
  hex-encoded before storage, `backend/crypto/secret.go:74`, so collation and
  case questions do not reach them.)

## 8. Stolpersteine (the honest list)

### 8.1 UTF-8 strictness — the sneakiest one

SQLite stores any byte sequence in `TEXT`. **Postgres rejects invalid UTF-8
and NUL bytes (`0x00`) in `TEXT` outright.** Mail is hostile input: subjects,
sender names, message-ids, and header fragments with broken encodings exist
in real mailboxes and are almost certainly sitting in the current data.

Two mandatory countermeasures:

1. **Write-path sanitization** at the parse boundary (`backend/mailparse`):
   `strings.ToValidUTF8(s, "�")` + strip `\x00` for every header-derived
   string before it reaches the store. Today only scattered sites do this
   (`api_message.go:228`, `compose.go:415`); it must become systematic, or
   the syncer will hit insert errors on the first malformed message after
   cutover.
2. **Migration-tool sanitization** (WP7) with logging, since existing rows
   contain whatever SQLite accepted over the years.

Columns that must stay byte-faithful (none known — hashes are hex, tokens are
base64) would need `BYTEA`; audit during WP2 confirms.

### 8.2 Latency profile

A per-user SQLite file made every query ~µs and made N+1 loops invisible.
Postgres adds a network round trip per statement. The web request paths are
fine (few queries per request), but syncer inner loops (per-message location
upserts, fingerprint checks) deserve measurement in the phase-5 dry run.
Note the shape of the risk with the current 200k+ message corpus:
incremental sync touches deltas only, so steady-state cost barely changes —
the paths that walk the whole corpus are the rebuild flows (mailbox
generation rebuild, index hydration), and those are what the dry run must
time at full production scale.
Mitigations if needed: `= ANY($1)` batching (already introduced by WP3),
multi-row `INSERT ... VALUES (...),(...)`, pgx batch API in hot spots.
Do not pre-optimize; measure.

### 8.3 Type strictness at `Scan`

SQLite's dynamic typing forgave `Scan(&int64)` on messy columns. PG is
strict. Keeping 0/1 `BIGINT` booleans and unix-int timestamps (WP2) minimizes
this, but expect a tail of test failures pointing at spots where a column's
declared and actual types drifted. The test suite is the safety net; this is
a reason *not* to shortcut WP8.

### 8.4 Ordering and comparison semantics

With `LC_COLLATE=C` (§3.4), `ORDER BY` on text and all `=` comparisons match
SQLite's `BINARY` behavior, so no query-by-query collation audit is needed.
If the database is *not* created with `C` collation, that audit (58+ sites,
plus every implicit text `ORDER BY`) becomes mandatory — easier to enforce
the collation at provisioning and verify it at startup (fail fast if
`datcollate != 'C'`).

### 8.5 Connection pool vs long transactions

`SetMaxOpenConns(4)` becomes a real pool (~10). Watch for transactions held
across IMAP fetches or Bleve writes — a store-layer audit during WP4 should
confirm no `*sql.Tx` spans network I/O (spot checks so far found none; the
message-indexing checkpoint dance was the closest and gets deleted).

### 8.6 Operational surface shift

- Backups: `VACUUM INTO` self-service is gone. For this deployment (see
  §12): the hoster runs continuous WAL archiving plus hourly base backups
  (7-day retention), but those are disaster-recovery only — not
  self-service, not browsable, not restorable by us. Our own restore path
  is therefore scheduled `pg_dump` (via the hoster's SSH bastion or from
  inside the platform against the same DSN), keeping a copy outside the
  platform. This replaces the in-app backup button (WP5).
- For generic self-hosters the compose file should ship a scheduled
  `pg_dump` sidecar or the docs must cover it well — otherwise this is a
  feature regression.
- One more service to run, upgrade (PG major versions), and monitor.
- Postgres major-version upgrades need `pg_upgrade`/dump-restore — worth a
  README paragraph so it does not surprise anyone in two years.

### 8.7 Plugin lockstep

Any `go.mod` bump (adding pgx) requires rebuilding all `.so` plugins with the
identical dependency graph — an existing constraint, but the migration makes
it bite once. CI already covers it.

## 9. Explicit non-goals of phase 1–6

- No dual SQLite/Postgres backend abstraction.
- No schema redesign, no `BOOLEAN`/`TIMESTAMPTZ` type modernization beyond
  the mechanical mapping (candidates for later cleanup, not for the
  migration diff).
- No multi-node app deployment (Bleve and the instance lock still force a
  single process).
- No Postgres-on-CephFS deployment support.

## 10. Phase 7 sketch: search on Postgres

Kept out of scope above, but the direction that makes the CephFS story
complete, since Bleve's mmap'd segments are the remaining risk on `/data`:

- `tsvector` column(s) over subject/body/attachment text, GIN index,
  per-language configs; `pg_trgm` for the fuzzy path and
  `backend/search/similarity.go`.
- The user ranking knobs (`search_recency_bias`, `search_sender_boost`,
  `search_contact_boost`, `search_attachment_weight`, `users.go:18`) map to a
  `ts_rank_cd` weighting expression plus app-side score blending — feasible,
  but a real design task of its own.
- Retires: the search coordinator's byte-budget machinery, the
  `bleve.recovery-required` restart flow, quarantine/rebuild, and finally the
  single-process constraint itself.
- Requires `pg_trgm` (and possibly `unaccent`) — confirm the hoster allows
  extensions before committing to this phase.

## 11. Cutover and rollback

1. Freeze: stop the container (instance lock released), take a final Ceph
   snapshot of `/data`.
2. `rolltop migrate-to-postgres` against the empty target; review the
   verification report and the sanitization log.
3. Start the new image with `ROLLTOP_DATABASE_URL`. Bleve indexes on `/data`
   remain valid — they reference message ids, which the migration preserves
   (identity values are copied verbatim, sequences bumped past them).
4. Watch the syncer complete one full cycle per account; compare message
   counts per mailbox against the report.
5. Rollback path (until the old image is retired): stop, restart the
   previous image — the SQLite files were never written after the freeze.
   Mail changes made in the Postgres interim would be lost locally but
   re-mirrored from IMAP; locally-created state (snoozes, contacts edits) in
   the interim would not survive a rollback. Keep the rollback window short.

## 12. Open questions

Hoster answers received 2026-08-18:

1. ~~Hosting: managed PostgreSQL available?~~ **Yes — managed Postgres 16.**
   `pg_trgm`, `citext`, and `unaccent` are trusted extensions and our
   database user may `CREATE EXTENSION` them directly. Phase 7 (search) is
   therefore unblocked in principle.
2. ~~Connection limits?~~ **20 connections per database user.** App pool
   default `ROLLTOP_DB_MAX_CONNS=10` (WP1) leaves headroom for the
   migration tool, scheduled `pg_dump`, and manual `psql` without ever
   tripping the limit.
3. ~~Backup story?~~ Hoster-side: continuous WAL archiving + hourly base
   backups, 7-day retention — **disaster recovery only, not self-service.**
   Our own backups: scheduled `pg_dump` via SSH bastion (or in-platform),
   stored off-platform. §8.6 updated accordingly; the in-app backup button
   is dropped (WP5).

4. ~~Collation of the provisioned database~~ **Resolved** — the hoster
   cannot set a per-database locale, so §3.4 switches to per-column
   `COLLATE "C"` in the baseline DDL. Equality stays byte-exact under any
   deterministic default collation, so this narrows the concern to
   ordering, which the column collation pins. Verified end-to-end by
   `scripts/pg-preflight.sql` and by the in-app admin preflight
   (`backend/pgpreflight`, admin Database page); run one of them against
   the provisioned database before phase 1 starts.

Still open:
5. ~~Timing of phase 0~~ **Done** — shipped on `main` (see §5, phase 0).

## 13. Preflight result against the provisioned database (2026-08-18)

Run from the app container through the admin Database page, so these are the
real app-to-database numbers. All checks passed in 99 ms.

| Fact | Measured |
| --- | --- |
| Server | PostgreSQL 16.6 (Ubuntu, pgdg) |
| Encoding | UTF8 |
| Database locale | `LC_COLLATE=en_US.utf-8`, `LC_CTYPE=en_US.utf-8`, provider `libc` |
| Round-trip latency | median 0.58 ms, fastest 0.55 ms |
| Sort order under the default collation | `a,ä,B,Z` (byte order is `B,Z,a,ä`) |
| Connection budget | `max_connections=200`, per-role limit 20, `CREATEDB=false` |

Three of these change how the plan is executed:

**The default collation diverges from byte order, as measured.** `a,ä,B,Z`
against `B,Z,a,ä` is dictionary order, not byte order — so the per-column
`COLLATE "C"` decision in §3.4 is load-bearing, not a precaution. Without it
every text `ORDER BY` would sort differently than SQLite does today, which
would silently break keyset pagination in the mail list. Equality is
unaffected (the probe confirmed byte-exact `=`), so the split holds exactly
as designed: equality needs nothing, ordering needs the column collation.

**Latency sets the batching budget.** 0.58 ms per round trip is good for a
network hop, and web request paths (a handful of queries each) will not
notice. The number to watch is the full-corpus walk: with 200k+ messages, a
per-row loop costs about two minutes of pure round trips. That is the
concrete threshold for the phase-5 dry run in §8.2 — measure the mailbox
generation rebuild and index hydration, and batch with `= ANY($1)` or
multi-row inserts wherever they exceed it.

**`CREATEDB=false` constrains two work packages.** The application role
cannot create databases, so:

- WP7's migration tool cannot provision its own target. The hoster must
  create the (empty) destination database before the cutover, and the tool
  should verify emptiness rather than attempt creation.
- WP8's test strategy — `CREATE DATABASE ... TEMPLATE rolltop_test_tmpl` per
  test — cannot run against this instance. That is fine as designed, because
  tests run against a CI-local Postgres container with full privileges, but
  the helper must fail with a clear message rather than an opaque
  permissions error when someone points `TEST_DATABASE_URL` at the hosted
  database.

The per-role limit of 20 confirms the hoster's figure and leaves the planned
pool of 10 room for the migration tool, a scheduled `pg_dump`, and a manual
`psql` session at the same time.
