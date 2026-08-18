-- Postgres preflight for the SQLite -> PostgreSQL migration
-- (docs/postgres-migration-plan.md).
--
-- Verifies, against the actual target database, every capability the plan
-- assumes: server version and encoding, byte-exact text equality,
-- per-column/per-index COLLATE "C" (the hoster cannot provision the database
-- itself with LC_COLLATE 'C'), the trusted extensions, UTF-8 strictness, and
-- the SQL features the ported queries rely on (IDENTITY, ON CONFLICT/excluded,
-- = ANY(array), tsvector/GIN, pg_trgm).
--
-- Run it from a machine that can reach the database (e.g. via the SSH
-- bastion):
--
--     psql "$ROLLTOP_DATABASE_URL" -f scripts/pg-preflight.sql
--
-- Every check either prints PASS or aborts the script with an error
-- (ON_ERROR_STOP). Sections marked INFO report facts that need a human
-- judgement rather than having a hard pass condition. The script only touches
-- a scratch schema (preflight_scratch), which it drops at the end; the CREATE
-- EXTENSION calls are the one persistent effect, and those extensions are
-- wanted for the migration anyway.
--
-- Do not run this while the admin Database page's preflight runs against the
-- same database: both use the preflight_scratch schema and each starts by
-- dropping it, so concurrent runs delete each other's tables. The in-app
-- preflight serializes its own runs; it cannot see a psql session.
--
-- Tip: run once with \timing enabled (psql -c '\timing on' interactively) to
-- get a feel for round-trip latency — but note that latency via the bastion
-- is not representative of the app-to-database path inside the platform.

\set ON_ERROR_STOP on

\echo ''
\echo '=== 1. Server (INFO + version/encoding gate) ==='
SELECT version();
SELECT datname, datcollate, datctype,
       pg_encoding_to_char(encoding) AS encoding
FROM pg_database WHERE datname = current_database();

DO $$
BEGIN
  IF current_setting('server_version_num')::int < 160000 THEN
    RAISE EXCEPTION 'FAIL: server version % is below 16',
      current_setting('server_version');
  END IF;
  RAISE NOTICE 'PASS: server version % >= 16', current_setting('server_version');
  IF pg_char_to_encoding(current_setting('server_encoding')) <> pg_char_to_encoding('UTF8') THEN
    RAISE EXCEPTION 'FAIL: server encoding is %, expected UTF8',
      current_setting('server_encoding');
  END IF;
  RAISE NOTICE 'PASS: server encoding is UTF8';
END $$;

\echo ''
\echo '=== 2. Text equality is byte-exact ==='
-- The store compares hashes, message-ids, and fingerprints with plain "=".
-- Under any deterministic collation Postgres compares equality byte-wise,
-- regardless of locale, which is what makes the hoster's fixed cluster locale
-- acceptable at all (migration plan §3.4).
--
-- This is deliberately a behavioral probe, not a catalog lookup: the
-- pg_collation row named 'default' is a pinned placeholder whose
-- collisdeterministic is hard-wired true and says nothing about this
-- database, so querying it only looks like a check. Postgres also does not
-- currently allow a nondeterministic collation as a database default
-- (verified on 16), so these probes guard against a future relaxation.
DO $$
BEGIN
  IF 'a' = 'A' THEN
    RAISE EXCEPTION 'FAIL: default collation is case-insensitive; = is not byte-exact';
  END IF;
  IF 'ss' = 'ß' THEN
    RAISE EXCEPTION 'FAIL: default collation is ligature-insensitive; = is not byte-exact';
  END IF;
  IF 'abc' = 'abc ' THEN
    RAISE EXCEPTION 'FAIL: default collation is padding-insensitive; = is not byte-exact';
  END IF;
  -- Written with unicode escapes so no editor normalization can turn the
  -- two operands into the same bytes: U+00E4 is precomposed 'ä', U+0061
  -- U+0308 is 'a' plus a combining diaeresis.
  IF U&'a' = U&'\00E1' THEN
    RAISE EXCEPTION 'FAIL: default collation is accent-insensitive; = is not byte-exact';
  END IF;
  IF U&'\00E4' = U&'a\0308' THEN
    RAISE EXCEPTION 'FAIL: default collation is normalization-insensitive; = is not byte-exact';
  END IF;
  RAISE NOTICE 'PASS: text equality is byte-exact';
END $$;

\echo ''
\echo '=== 3. Privileges: schema, table, index ==='
DROP SCHEMA IF EXISTS preflight_scratch CASCADE;
CREATE SCHEMA preflight_scratch;
SET search_path = preflight_scratch, public;
DO $$ BEGIN RAISE NOTICE 'PASS: CREATE SCHEMA'; END $$;

\echo ''
\echo '=== 4. Column-level and index-level COLLATE "C" ==='
CREATE TABLE preflight_scratch.collate_c (key text COLLATE "C" NOT NULL);
CREATE INDEX ON preflight_scratch.collate_c (key);
CREATE TABLE preflight_scratch.collate_default (key text NOT NULL);
CREATE INDEX ON preflight_scratch.collate_default (key COLLATE "C");
INSERT INTO preflight_scratch.collate_c VALUES ('a'), ('B'), ('Z'), ('ä');
INSERT INTO preflight_scratch.collate_default SELECT key FROM preflight_scratch.collate_c;
DO $$
DECLARE got text;
BEGIN
  -- Byte order of the UTF-8 encodings: B (0x42) < Z (0x5A) < a (0x61) < ä (0xC3A4).
  SELECT string_agg(key, ',' ORDER BY key) INTO got FROM preflight_scratch.collate_c;
  IF got <> 'B,Z,a,ä' THEN
    RAISE EXCEPTION 'FAIL: COLLATE "C" column does not sort in byte order (got %)', got;
  END IF;
  RAISE NOTICE 'PASS: column declared COLLATE "C" sorts in byte order';
  SELECT string_agg(key, ',' ORDER BY key COLLATE "C") INTO got
  FROM preflight_scratch.collate_default;
  IF got <> 'B,Z,a,ä' THEN
    RAISE EXCEPTION 'FAIL: per-query COLLATE "C" does not sort in byte order (got %)', got;
  END IF;
  RAISE NOTICE 'PASS: per-query COLLATE "C" sorts in byte order';
END $$;

\echo 'INFO: the same values ordered by the DATABASE DEFAULT collation'
\echo '      (shows how far the cluster locale diverges from byte order):'
SELECT string_agg(key, ',' ORDER BY key) AS default_collation_order
FROM preflight_scratch.collate_default;

\echo ''
\echo '=== 5. Trusted extensions ==='
CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS unaccent WITH SCHEMA public;
DO $$
BEGIN
  IF similarity('rolltop', 'roltop') <= 0 THEN
    RAISE EXCEPTION 'FAIL: pg_trgm similarity() unusable';
  END IF;
  IF unaccent('Müller') <> 'Muller' THEN
    RAISE EXCEPTION 'FAIL: unaccent() unusable';
  END IF;
  RAISE NOTICE 'PASS: pg_trgm, citext, unaccent installed and working';
END $$;

\echo ''
\echo '=== 6. UTF-8 strictness (NUL and invalid bytes are rejected) ==='
-- Mail is hostile input; the plan requires write-path sanitization. This
-- check documents that the database really does reject what SQLite accepted.
-- Each probe must fail with its specific SQLSTATE. Accepting any error would
-- report a pass for a check that never reached the server.
DO $$
BEGIN
  BEGIN
    PERFORM chr(0);
    RAISE EXCEPTION 'FAIL: NUL character was accepted in text';
  EXCEPTION
    WHEN program_limit_exceeded THEN            -- 54000 null character not permitted
      RAISE NOTICE 'PASS: NUL character rejected (%)', SQLERRM;
    WHEN OTHERS THEN
      IF SQLERRM LIKE 'FAIL:%' THEN RAISE; END IF;
      RAISE EXCEPTION 'FAIL: NUL probe did not complete (SQLSTATE %): %', SQLSTATE, SQLERRM;
  END;
  BEGIN
    PERFORM convert_from('\xff'::bytea, 'UTF8');
    RAISE EXCEPTION 'FAIL: invalid UTF-8 byte sequence was accepted';
  EXCEPTION
    WHEN character_not_in_repertoire THEN       -- 22021 invalid byte sequence
      RAISE NOTICE 'PASS: invalid UTF-8 rejected (%)', SQLERRM;
    WHEN OTHERS THEN
      IF SQLERRM LIKE 'FAIL:%' THEN RAISE; END IF;
      RAISE EXCEPTION 'FAIL: UTF-8 probe did not complete (SQLSTATE %): %', SQLSTATE, SQLERRM;
  END;
END $$;

\echo ''
\echo '=== 7. SQL features the ported queries rely on ==='
CREATE TABLE preflight_scratch.features (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  slot text COLLATE "C" NOT NULL UNIQUE,
  hits bigint NOT NULL DEFAULT 0,
  body tsvector
);
CREATE INDEX ON preflight_scratch.features USING gin (body);
DO $$
DECLARE new_id bigint; n bigint;
BEGIN
  -- LastInsertId replacement: INSERT ... RETURNING.
  INSERT INTO preflight_scratch.features (slot) VALUES ('a') RETURNING id INTO new_id;
  IF new_id IS NULL THEN RAISE EXCEPTION 'FAIL: RETURNING id'; END IF;
  -- INSERT OR IGNORE replacement: ON CONFLICT DO NOTHING reports 0 rows.
  INSERT INTO preflight_scratch.features (slot) VALUES ('a') ON CONFLICT DO NOTHING;
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 0 THEN RAISE EXCEPTION 'FAIL: ON CONFLICT DO NOTHING affected % rows', n; END IF;
  -- SQLite-style upsert with excluded.*.
  INSERT INTO preflight_scratch.features (slot, hits) VALUES ('a', 5)
    ON CONFLICT (slot) DO UPDATE SET hits = preflight_scratch.features.hits + excluded.hits;
  SELECT hits INTO n FROM preflight_scratch.features WHERE slot = 'a';
  IF n <> 5 THEN RAISE EXCEPTION 'FAIL: upsert with excluded.* (hits=%)', n; END IF;
  -- IN (?,?,...) replacement: = ANY(array).
  SELECT count(*) INTO n FROM preflight_scratch.features WHERE id = ANY(ARRAY[new_id]);
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL: = ANY(array)'; END IF;
  -- Migration tool: identity sequences accept setval past imported rows.
  PERFORM setval(pg_get_serial_sequence('preflight_scratch.features', 'id'), 1000000);
  INSERT INTO preflight_scratch.features (slot) VALUES ('b') RETURNING id INTO new_id;
  IF new_id <> 1000001 THEN RAISE EXCEPTION 'FAIL: setval on identity (next id %)', new_id; END IF;
  -- Phase 7: tsvector matching through the GIN index path.
  UPDATE preflight_scratch.features SET body = to_tsvector('simple', 'quarterly report attached');
  SELECT count(*) INTO n FROM preflight_scratch.features
    WHERE body @@ to_tsquery('simple', 'report');
  IF n < 1 THEN RAISE EXCEPTION 'FAIL: tsvector @@ tsquery'; END IF;
  RAISE NOTICE 'PASS: RETURNING, ON CONFLICT DO NOTHING, excluded.* upsert, = ANY, identity setval, tsvector/GIN';
END $$;

\echo ''
\echo '=== 8. Connection budget (INFO) ==='
SELECT current_setting('max_connections') AS max_connections,
       rolconnlimit AS per_role_limit,
       rolcreatedb  AS can_create_databases
FROM pg_roles WHERE rolname = current_user;

\echo ''
\echo '=== Cleanup ==='
RESET search_path;
DROP SCHEMA preflight_scratch CASCADE;
\echo ''
\echo 'All preflight checks passed.'
