# rolltop

> **Fork von [grahamsz/rolltop](https://github.com/grahamsz/rolltop)** — 488 Commits
> seit dem 4. August 2026. Was sich geändert hat, steht direkt hier unter
> [Über diesen Fork](#über-diesen-fork); ausführlich in **[FORK.md](FORK.md)**.
> *This is a fork; the documentation below is the project's own and applies to it.*

rolltop is a Go app that mirrors multiple IMAP inboxes per local user into storage you run, for search, viewing, composing, and mailbox moves. It needs a PostgreSQL database beside it; the compose file below runs both. Production mail data stays in the user's own instance. Project site: https://rolltop.app, coming soon. Contact: graham@rolltop.app.

## Über diesen Fork

Gmail wird zum Jahresende 2026 eingestellt, und damit fällt der Ort weg, an dem
bei mir mehrere Adressen zusammengelaufen sind — samt Kontakten, Kalender und
einer Ablage, die die Post sortiert hat, bevor ich sie gesehen habe. Rolltop war
der beste Ausgangspunkt, den ich dafür gefunden habe. Dieser Fork ergänzt das,
was Gmail über einen Mail-Client hinaus ausgemacht hat, und den Unterbau, den ein
gewachsenes Postfach auf einem Hoster braucht.

Die Unterschiede zum Original in Kürze:

| Bereich | Was hier anders ist |
| --- | --- |
| **Datenbank** | PostgreSQL statt SQLite. Eine Datenbank für die Installation, jede Zeile über `user_id` getrennt — weil SQLite auf einem netzwerkgebundenen Volume irgendwann `database disk image is malformed` sagt. Dazu Preflight, Migrationskonsole, `pg_dump`-Backups und ein Advisory-Lock, der zwei Server auf einer Datenbank verhindert. |
| **Google-Konten** | Verbunden per OAuth statt per App-Passwort, IMAP und SMTP melden sich mit XOAUTH2 an. Kontakte über die People API mit Rückschreiben, Google-Kalender gespiegelt inklusive Wochenansicht und Terminbearbeitung. Gmail-gerechte Voreinstellungen: All Mail/Wichtig/Markiert bleiben draußen, ein Sync-Startdatum begrenzt den ersten Durchlauf. |
| **Ablage** | Die Seitenleiste führt mit **Inbox** (alles über alle Konten, was noch nicht archiviert ist). Darunter fünf Kategorien — Relevant, Newsletters, Forums, Notifications, Invoices&nbsp;&&nbsp;Contracts —, die ersten vier aus den Headern des Absenders entschieden, die letzte aus dem Inhalt (ZUGFeRD, Factur-X, XRechnung, Belegnummer neben Betrag). Kontenübergreifende Dubletten werden erkannt und ausgeblendet. |
| **Regeln** | `mail_filters` weitgehend neu: Editor mit benannten Feldern, Aktionen für gelesen/weiterleiten/verschieben, Ziele relativ zum Konto der Nachricht, Weiterleitung wahlweise nur für neue Post, und ein Audit über 30 Tage, das auch zeigt, worauf die Filter noch warten. |
| **Suche** | Zweites Backend auf PostgreSQL (`tsvector`) neben Bleve, umschaltbar über `ROLLTOP_SEARCH_BACKEND`, mit Fuzzy-Treffern bei Tippfehlern über `pg_trgm`, Ranking auf Relevanz und Sortierung nach Datum in beide Richtungen. |
| **IMAP-Sync** | Verbindungen pro Durchlauf wiederverwendet, jeder Durchgang zeit- **und** speicherbegrenzt, Fetch-Batches nach den vom Server gemeldeten Größen geplant, ein Ordner voll Post mit einem Befehl verschoben. Ein paar sehr große Mails bestimmen nicht mehr, wie viel Speicher der Prozess braucht. |
| **Bedienung** | Gmail-Optik für die Listen, Zeilenaktionen beim Überfahren, Befehlsleiste im Konversationskopf, `Send & archive`, Kategorie-Pille auf der Kopfzeile, `Ältere archivieren`, `Papierkorb leeren`, System-Theme mit Dunkelmodus für HTML-Post. |
| **Betrieb** | `/api/health`, geordnetes Herunterfahren mit Budget pro Phase, Crash-Berichte im Datenvolumen, Warten statt Crash-Schleife beim Start, Admin-Seite für Datenbank und Logs, SMTP-Transkript für fehlgeschlagene Sendungen. |
| **Sicherheit** | Vollständiger Review mit Umsetzung in fünf Phasen ([`docs/code-review-umsetzungsplan.md`](docs/code-review-umsetzungsplan.md)): Login-Drosselung, `ROLLTOP_PUBLIC_URL` als vertrauenswürdige Link-Basis, Webhook-Token nur aus Headern, CSP ohne `script-src 'unsafe-inline'`, validierte OIDC-`id_token`. |
| **CI/Tests** | Zwei Workflows — ein PR-Tor, das nur die betroffenen Bereiche prüft, und ein Merge-Tor mit Paketierung an `v*`-Tags. `go vet` als Tor, Vitest im Frontend, 282 statt 150 Go-Testdateien. |

**Was das Original hat und dieser Fork nicht:** einen echten Offline-Modus mit
eingereihten Sendungen (seit dem 25. August im Original). Hier gibt es weiterhin
nur die begrenzte PWA-Zwischenablage, die beide Zweige vom gemeinsamen Stand
geerbt haben. Ein Teil der Sicherheitshärtung ist außerdem in beiden Zweigen
unabhängig und weitgehend gleichlautend entstanden — das sind keine
Alleinstellungsmerkmale, auch wenn der Diff sie so aussehen lässt.

**Beim Betrieb dieses Forks zu beachten:** `compose.yml` zeigt auf das Image des
Originals (`ghcr.io/grahamsz/rolltop:latest`). Das enthält diesen Code nicht und
läuft auf SQLite, passt also nicht zur PostgreSQL-Konfiguration daneben. Bis hier
ein `v*`-Tag veröffentlicht ist, das Image aus diesem Repository selbst bauen:
`docker build -t rolltop:local .`

Ausführlich, Bereich für Bereich, in **[FORK.md](FORK.md)**. Alles weitere unten
ist die Dokumentation des Projekts selbst und gilt unverändert für diesen Fork.

## What It Stores

- Relational metadata — users, accounts, mailboxes, message headers, sync
  state — in PostgreSQL, one database for the whole installation with every
  row scoped by `user_id`
- Per-user Bleve search indexes at `/data/users/{user_id}/bleve`
- Raw `.eml` and attachment blobs under `/data/users/{user_id}/blobs/...`
- Incremental sync progress in `sync_runs`
- A compiled React + Vite + TypeScript frontend served by the Go process

## Security Model

- Browser routes derive the current user from a server-side session.
- Normal user routes never accept `user_id` from browser input.
- Sessions use opaque random tokens; only SHA-256 token hashes are stored.
- Cookies are `HttpOnly` and `SameSite=Lax`.
- POST routes require CSRF tokens.
- App passwords are hashed with Argon2id.
- IMAP passwords are encrypted at rest with `ROLLTOP_MASTER_KEY`.
- Google OAuth refresh and access tokens are encrypted at rest with the same key and are never returned by the API.
- Admins can create users, but V1 does not give admins access to other users' mail.
- Message sending uses the configured SMTP server.
- Mailbox moves are explicit user actions and are mirrored to IMAP.
- Read-state sync may update only the IMAP `\Seen` flag when a message is read locally.
- Message authentication badges report bounded SPF, DKIM, and DMARC values found in received headers; Rolltop labels their source and does not claim to verify them independently.

## Configuration

Required — a master key and a PostgreSQL connection string:

```sh
test -f .env.rolltop || (
  umask 077
  printf 'ROLLTOP_MASTER_KEY=%s\n' "$(openssl rand -base64 32)" > .env.rolltop
)
# The database must exist and be empty on first start; rolltop creates its
# schema but not the database itself.
export ROLLTOP_DATABASE_URL="postgres://rolltop:password@db:5432/rolltop?sslmode=require"

set -a
. ./.env.rolltop
set +a
```

Common optional variables:

```sh
export ROLLTOP_ADDR=":8080"
export ROLLTOP_DATA_DIR="/data"
export ROLLTOP_INDEX_PATH="/data/bleve"
export ROLLTOP_SEARCH_BACKEND="bleve"
export ROLLTOP_DB_MAX_CONNS="10"
export ROLLTOP_DB_CONNECT_TIMEOUT="2m"
export ROLLTOP_SESSION_TTL="720h"
export ROLLTOP_SYNC_INTERVAL="15m"
export ROLLTOP_INBOX_POLL_INTERVAL="1m"
export ROLLTOP_BLOB_RETENTION="336h"
export ROLLTOP_COOKIE_SECURE="false"
export ROLLTOP_PUBLIC_URL=""
export ROLLTOP_WEBHOOK_TOKEN=""
export ROLLTOP_LOG_LEVEL="info"
export ROLLTOP_MEMORY_LIMIT="80%"
export ROLLTOP_STARTUP_LOCK_WAIT="2m"
export ROLLTOP_BREAK_INSTANCE_LOCK="false"
export ROLLTOP_SHUTDOWN_TIMEOUT="10s"
export ROLLTOP_SQLITE_ACCESS="auto"
```

Session and CSRF cookies are marked `Secure` automatically on any request that
arrives over HTTPS (directly or via `X-Forwarded-Proto: https` from a
terminating proxy), and an HSTS header is sent on those same requests. A
plain-HTTP local run still works, because a `Secure` cookie is silently dropped
over `http://`. Set `ROLLTOP_COOKIE_SECURE=true` to force the flag on regardless
of the detected scheme.

Login and password-reset requests are rate-limited per client IP and target
address with exponential backoff, so repeated password guessing and reset
mail-bombing are throttled without ever locking an address out for good. A
throttled request answers `429` with a `Retry-After` header.

Set `ROLLTOP_PUBLIC_URL` to the external origin the app is reached at (scheme and
host, e.g. `https://mail.example.com`). It is the trusted base for links the app
builds back to itself — the password-reset link in outgoing mail, and the
callback and discovery URLs the OIDC and Mail MCP plugins hand out. When it is
unset, those are built from the request `Host`/`X-Forwarded-Host` header, which a
client can set: an attacker who triggers a reset for a victim can then have the
emailed link point at their own domain and capture the token, and the same
spoofed host can steer an OAuth/OIDC redirect. Setting this closes that, so
configure it in any deployment that sends password-reset mail or enables those
plugins. A trailing path is ignored.

`ROLLTOP_WEBHOOK_TOKEN` guards the `/webhooks/sync` trigger. Present it in the
`X-Rolltop-Webhook-Token` header or an `Authorization: Bearer` header — a token
in the URL query is no longer accepted, because query strings land in access and
proxy logs.

`ROLLTOP_MEMORY_LIMIT` is the soft ceiling the Go runtime is given for its heap.
Without one the collector aims at roughly twice the live heap, so the first sync
of a new account, which is the heaviest work this process does, can grow into the
container's limit and be killed mid-write. The default takes 80% of the
container's memory limit, or of the machine's memory when the container has no
limit; the remainder covers what the heap accounting does not, such as goroutine
stacks and the mapped search index. Set an absolute size (`768MiB`, `2GB`), a
different share (`60%`), or `off` to leave the runtime unbounded. An explicit
`GOMEMLIMIT` wins over this setting. The applied value is named on startup:

```text
rolltop memory limit 1.6GiB (80% of the 2.0GiB cgroup limit)
```

That remainder is not optional. Bleve reads its segments through `mmap`, so a
large search index needs room in the page cache, and the page cache shares the
container's limit with the heap. Once the index no longer fits in what the heap
ceiling leaves over, the kernel evicts the pages the next commit needs and every
read becomes a major fault — on a FUSE-backed volume that turns a commit of a few
kilobytes into one that runs for minutes, which is indistinguishable from a hung
writer and is treated as one. Startup measures the index and says so when the
two cannot both fit:

```text
rolltop warning: the search index is 3.2GiB and the heap ceiling is 1.6GiB of the 2.0GiB limit, leaving 400MiB for everything the heap does not account for; ...
```

The remedy is a container with more memory, or a **lower** `ROLLTOP_MEMORY_LIMIT`
so the index has room to stay resident. A smaller heap ceiling means more
throughput here, not less. `workingset_refault_file` and `pgmajfault` in
`/sys/fs/cgroup/memory.stat` count the thrashing directly, and `max` in
`/sys/fs/cgroup/memory.events` counts how often the limit forced reclaim.

The limit is a ceiling, not a reservation: the process only uses what it needs,
and going over it makes the collector work harder rather than failing an
allocation. Give the container at least a gigabyte for a first sync of a large
mailbox.

`ROLLTOP_SEARCH_BACKEND` selects where full-text search lives. The default
`bleve` keeps the per-tenant mmap'd indexes under `ROLLTOP_INDEX_PATH` — the
setup all of the above is about. `postgres` serves search from a `tsvector`
table in the relational database instead: no mmap contending with the heap, no
index directory on `/data`, and the writer-stall restart machinery is never
armed. On first start with the postgres backend, tenants whose messages are
not yet in the table are marked and re-indexed through the normal repair path
— for a large mailbox that is hours of background indexing, the same as after
an index rebuild. Switching back to `bleve` serves the old on-disk index,
stale for the interim until a repair closes the gap. The migration story and
the differences between the backends are documented in
`docs/search-postgres-plan.md`.

`ROLLTOP_DATABASE_URL` is the PostgreSQL connection string and is required —
there is no local fallback. The database has to exist and be empty on first
start: rolltop creates its own schema, records the schema version, and refuses
to start against a database holding tables it did not create, which is what
stops it from being pointed at somebody else's data.

`ROLLTOP_DB_MAX_CONNS` sizes the connection pool (default `10`, and the admin
Database page shows the number in force). Size it below
the connection limit your database role has, leaving room for a scheduled
`pg_dump` and a `psql` session; two rolltop processes overlap during a rolling
deploy, so the practical peak is twice this number for a few seconds.

`ROLLTOP_DB_CONNECT_TIMEOUT` (default `2m`) is how long startup waits for a
database that is not up yet. The app container and the database start
independently, so a refused first connection is normal rather than fatal;
without the wait it becomes a crash loop whose restarts are the slowest possible
retry.

`ROLLTOP_LOG_LEVEL` defaults to `info`, which hides verbose `debug ...` log
lines (plugin loading, one-click unsubscribe traces). Set it to `debug` to
include them. Failures are written as `error ...` lines and are never hidden,
whatever the level is set to; each names the request that produced it, for
example `error server error GET /api/mail: database is locked`.

### Google accounts

Connecting Google accounts is optional. Leave these unset and the Google
settings section reports the server as unconfigured; nothing else changes.

```sh
export ROLLTOP_GOOGLE_CLIENT_ID="1234567890-example.apps.googleusercontent.com"
export ROLLTOP_GOOGLE_CLIENT_SECRET="GOCSPX-example"
export ROLLTOP_GOOGLE_REDIRECT_URLS="https://mail.example.com/api/google/callback"
export ROLLTOP_GOOGLE_SCOPES=""
```

`ROLLTOP_GOOGLE_REDIRECT_URLS` accepts several entries separated by commas or
whitespace, so one deployment can serve both a production host and
`http://localhost:8080` for development. Every entry must be an absolute
`http` or `https` URL ending in `/api/google/callback`, must be registered
byte for byte in the Google Cloud console, and with more than one configured
the request origin decides which is used. `ROLLTOP_GOOGLE_SCOPES` is optional
and replaces the default
`openid email https://mail.google.com/ https://www.googleapis.com/auth/contacts`
entirely.

These are validated during startup: half a credential, an unusable redirect
URL, or credentials without any redirect URL stop the process rather than
surfacing as a failed consent much later. Connecting also needs a usable
`ROLLTOP_MASTER_KEY`, because the tokens are stored encrypted.

In the Google Cloud console, create a **Web application** OAuth client and
register the same redirect URLs. Signing in over IMAP and SMTP needs the scope
granted but no API enabled; contact sync additionally needs the **People API**
enabled for the project. Google requires `https` for redirect URLs, except
for `http://localhost` and `http://127.0.0.1` — an instance reachable only over
plain HTTP on a LAN address cannot complete a consent round trip.
`redirect_uri_mismatch` on a first attempt is almost always a character
difference between the console entry and `ROLLTOP_GOOGLE_REDIRECT_URLS`.

`https://mail.google.com/` is a restricted scope, and how the consent screen is
published decides how often accounts have to be reconnected:

- **Testing** works without any review, but refresh tokens issued in this state
  expire after seven days, so connections need `Reauthorize` about weekly.
- **Internal**, available only on a Google Workspace domain, has neither
  restriction and is the least troublesome option where it applies.
- **In production** keeps refresh tokens valid indefinitely. An unverified app
  shows an "unverified app" interstitial that the operator confirms once and is
  limited to 100 users. Formal verification of a restricted scope requires a
  recurring third-party security assessment, which self-hosted installs are not
  expected to undergo.

Whichever state applies, an expired or revoked grant is detected and the
connection is marked as needing reauthorization rather than failing silently.

### Gmail mailboxes

Once an account is connected under Settings → Google, an IMAP server can select
it under **Sign-in** instead of asking for a password. The endpoints are set to
`imap.gmail.com:993` and `smtp.gmail.com:587` and no password is stored; the
same choice is available for the outgoing SMTP server, so a Gmail account needs
no app password in either direction. Google also answers on `465`, where TLS
opens before the greeting instead of after `EHLO`, and the outgoing server
offers that port for the network that blocks `587` — which port a provider
blocks against spam is theirs to decide, and the connection test says which one
gets through. Existing password accounts can be switched
over and back, though switching back requires entering a password, since the
OAuth row does not keep one.

Two Gmail-specific defaults matter for how much gets mirrored:

- **All Mail, Important and Starred start excluded from sync.** They are views
  over messages that already live in a real folder, and this mirror stores one
  folder per message, so syncing them would duplicate most of the mailbox.
- **Sync mail since** limits every fetch to messages delivered on or after a
  date, applied as an IMAP `SINCE` so old bodies are never transferred at all.
  Adding a Google account suggests two years back. Leaving it empty mirrors
  everything, which on a long-lived mailbox can run for hours and produce a
  much larger database. The value can be changed later; moving it further back
  backfills through the normal folder repair rather than immediately.

### Google contacts

A connected account whose grant includes contacts is polled every 15 minutes,
and **Sync contacts** under Settings → Google runs it immediately. Google is the
leading system for the contacts it owns:

- Changes made in Google win. A contact edited in both places keeps Google's
  version; Rolltop reports the conflict and shows what Google now holds.
- Edits, new contacts and deletions made in Rolltop are written back. Deleting a
  synced contact **deletes it in Google too**, which the confirmation says.
- A local contact already in the address book with the same address is linked to
  its Google counterpart rather than duplicated, keeping its Me identity and any
  detail Google does not have.
- Two Google contacts may share an address — a household, a shared office
  mailbox, a role account kept on two cards — and both are mirrored. Where one
  contact has to answer for the address anyway (the card and picture beside a
  sender, a reply identity, an import's merge target) it is your own contact
  first, then whoever carries the address as their primary one, then the oldest
  holder. Your own contact is never one Google owns: an address of yours that
  only a synced contact holds gets a card of its own rather than turning theirs
  into your identity.
- Disconnecting an account keeps its contacts and turns them back into local
  ones. Nothing is deleted.

New contacts default to Rolltop only; **Save to** in the contact editor puts one
in a Google account instead. **Reauthorize** is offered on every connection, not
only on one Rolltop can already name a fault on: consent is the only way to add
a scope, replace a revoked grant, or recover an account Google has quietly
stopped answering for. Accounts connected before contact sync existed still lack
the contacts scope and show `Reauthorize this account to include them` — mail
keeps working until they do.

## Run Locally

A PostgreSQL server has to be running first — `compose.dev.yml` starts a
throwaway one that keeps nothing on disk. The test suite and the app want
separate databases on it: the suite creates and drops one per test, so pointing
it at the database you are developing against would drop it.

```sh
npm install
npm run build
npm run build:plugins

docker compose -f compose.dev.yml up -d db
TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable' go test -p 2 ./...

createdb -h 127.0.0.1 -U postgres rolltop
test -f .env.rolltop || (
  umask 077
  printf 'ROLLTOP_MASTER_KEY=%s\n' "$(openssl rand -base64 32)" > .env.rolltop
)
set -a
. ./.env.rolltop
set +a
ROLLTOP_DATA_DIR="./data" \
ROLLTOP_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/rolltop?sslmode=disable' \
  go run ./cmd/rolltop
```

Open `http://localhost:8080`. If no users exist, `/setup` creates the first admin.

`-p 2` bounds how many test packages run at once. The suite is I/O-heavy
against one server, and the default lets enough packages compete that
timing-sensitive tests fail on scheduling rather than on behaviour.

## Docker

Rolltop needs a PostgreSQL database beside it, so the deployment is two
containers. `compose.yml` in this repository runs both; take it and the two
environment files below, and nothing else is needed:

```sh
curl -O https://raw.githubusercontent.com/grahamsz/rolltop/main/compose.yml

# POSTGRES_PASSWORD is read by Compose itself, which only ever looks in your
# shell environment and in a file named exactly `.env`. The alphabet is
# restricted on purpose: this password is substituted into the database
# connection string, and a quote or a backslash there would break it.
test -f .env || (
  umask 077
  printf 'POSTGRES_PASSWORD=%s\n' "$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 40)" > .env
)

# .env.rolltop is the app's own configuration, handed to the rolltop container.
test -f .env.rolltop || (
  umask 077
  printf 'ROLLTOP_MASTER_KEY=%s\nROLLTOP_COOKIE_SECURE=false\n' "$(openssl rand -base64 32)" > .env.rolltop
)

docker compose up -d
```

Keep both files with the same care as the Docker volumes. Changing or losing
`ROLLTOP_MASTER_KEY` makes stored IMAP passwords undecryptable; losing
`POSTGRES_PASSWORD` while the database volume survives locks you out of your
own data.

Running the image on its own still works, but it has to be told where its
database is — `ROLLTOP_DATABASE_URL` is required and the container exits at
once without it, which `--restart unless-stopped` turns into a crash loop:

```sh
docker run -d --name rolltop --restart unless-stopped -p 8080:8080 \
  --env-file .env.rolltop \
  -e ROLLTOP_DATABASE_URL='postgres://rolltop:PASSWORD@db.example:5432/rolltop?sslmode=require' \
  -v rolltop-data:/data \
  ghcr.io/grahamsz/rolltop:latest
```

In the URL form above a password containing `@`, `/`, `?`, or `#` must be
percent-encoded, or the DSN parses as pointing somewhere else entirely. The
keyword form `host=db.example port=5432 user=rolltop password='…'
dbname=rolltop sslmode=require` is accepted too and needs no encoding.

### Logs and crash reports

All application logs go to **stderr**. `docker logs rolltop` shows them, but
anything that pipes or redirects only stdout (`docker logs rolltop | grep …`,
`docker logs rolltop > out.log`) silently drops every log line — use
`docker logs rolltop 2>&1 | grep …` instead.

A start that reports `previous run ended with a crash or fatal error` has read
the bytes appended since the last run and found something other than start
markers there. Growth alone does not count: every start appends its own marker,
so comparing sizes announced a crash whenever the recorded baseline drifted by a
single line, and pointed at a report that did not exist.

Crashes are additionally persisted in the data volume so they survive
container recreation. Unhandled panics and fatal errors are **appended** to
`/data/crash.log`, which Rolltop never truncates — each start adds a
`=== rolltop <version> started at <time> pid=<n> ===` line, so every report is
attributable to a run. The next start reports in the container log that it
found one. Only when the file grows past 1 MiB is it rotated to
`/data/crash.log.prev`; if that rename fails, Rolltop keeps appending rather
than lose what is already there.

If the process is killed without any chance to report (kernel OOM kill,
SIGKILL), no crash report can exist. The next start detects the unclean
shutdown and says so — then check
`docker inspect rolltop --format '{{.State.ExitCode}} {{.State.OOMKilled}}'`
and the kernel log (`dmesg | grep -i oom`).

Crash reporting is armed before the listener binds and before the
configuration is read, so a port conflict or an unusable `ROLLTOP_MASTER_KEY`
also lands in `/data/crash.log`. The deliberate restart for search index
recovery (below) is not a crash and is not recorded as one.

### Outgoing mail that fails

A send that fails answers the composer with one sentence and writes the reason
to the container log, which on a hosted installation nobody can read. **Settings
→ Mail → the outgoing server** therefore shows the SMTP conversation itself:

- **Test connection** opens a connection to that server, upgrades it if TLS is
  configured, and signs in — then hangs up without offering a message, so
  pressing it cannot deliver mail to anybody. The transcript of that exchange
  appears immediately. One test runs at a time per user, with a short pause
  after it: the test decides what address the server dials out to, and answers
  with what the peer said back.
- Below it, the same page lists the recent attempts made by real sends,
  newest first, each expanding into the conversation it produced: the greeting,
  the extensions the server advertised, the TLS upgrade, the reply to `AUTH`,
  and the reply to the message. That is where a rejected password (`535`), a
  refused relay (`554`), a port that accepts a connection and says nothing, or
  a server that does not offer `STARTTLS` names itself.

The recorded traffic never contains a credential or a message: the `AUTH`
exchange is redacted to its mechanism and the payload is recorded as a byte
count. It is a diagnostic tail, not an audit trail — a short, per-user,
in-memory record that is dropped when Rolltop restarts and is never written to
the database. Each user sees only their own attempts; nothing here is an admin
view.

### Database Unavailable

When PostgreSQL is unreachable — restarting, failing over, or behind a broken
network — rolltop keeps serving what it can rather than answering 500 to
everything. A signed-in browser gets the app shell with a "database
unavailable" banner instead of a failed sign-in, so the admin Database page
stays reachable, and background sync logs the failure and retries on its next
turn.

Startup is deliberately patient for the same reason: the app container and the
database come up independently, so a first connection that is refused is waited
out for `ROLLTOP_DB_CONNECT_TIMEOUT` (default two minutes) rather than crash-
looping the container.

What used to live here — `database disk image is malformed`, the repair and
salvage commands, the integrity check — was SQLite-on-a-network-volume and is
gone with it. Storage integrity is the database server's job now, and the
admin Database page reports what it says about itself: version, size, replica
state, connection count, and the round-trip latency the sync loops are budgeted
against.

### Shutdown And Restart

`SIGTERM` and `SIGINT` start an orderly shutdown: the HTTP listener is drained,
sync runs still marked running are stamped interrupted, then the plugin host and
the search index are closed, and the database pool last. A committed transaction
is already durable, so nothing has to be flushed on the way out — the budget is
about the search index, which is the one thing here with a half-written state.

The whole sequence is bounded by `ROLLTOP_SHUTDOWN_TIMEOUT` (default `10s`, the
period `docker stop` gives), and the phases take fixed shares of it: three tenths
for the HTTP drain, a fifth for marking interrupted sync runs, three tenths for
the plugin host and search index together, and the remaining fifth for the
database close. Whatever has not finished by then is abandoned so the steps after
it still run. **Set it to the stop grace period the platform actually gives**,
and give the platform enough of one:

```yaml
services:
  rolltop:
    stop_grace_period: 60s          # Kubernetes: terminationGracePeriodSeconds
    restart: unless-stopped
    environment:
      ROLLTOP_SHUTDOWN_TIMEOUT: 45s # inside the grace period, not equal to it
```

Every step is logged, so an ordinary stop — a restart, a redeploy, an
environment-variable change the platform applies by recreating the container —
reads like this:

```text
received SIGTERM
rolltop shutting down after SIGTERM, budget 45s (ROLLTOP_SHUTDOWN_TIMEOUT)
rolltop shutdown: draining HTTP requests (0s of 45s spent)
rolltop shutdown: closing the search index (1.2s of 45s spent)
rolltop shutdown: closing the database pool (2.8s of 45s spent)
rolltop shutdown complete in 2.9s of 45s
```

The same phases are recorded in `/data/crash-state.json` as they happen, because
the container log is usually gone by the time anyone looks. A process killed
before its shutdown finished is reported as exactly that on the next start,
naming the step it did not get past:

```text
previous run was asked to stop with SIGTERM at 2026-08-19T17:29:12Z and was killed
before its shutdown finished, while closing the search index, reached 4s after the
signal; the container's stop grace period is shorter than the shutdown needs -
raise it (docker: stop_grace_period, Kubernetes: terminationGracePeriodSeconds),
or lower ROLLTOP_SHUTDOWN_TIMEOUT so the shutdown fits inside the period it does get
```

The older message, the one that names a kernel OOM kill or a `SIGKILL`, now only
appears when **no** stop signal was recorded — a process that was killed outright
rather than asked to stop. That is the case where the container exit code and the
kernel log are the right things to check.

An abandoned search index close is not silent either: a durable recovery marker
records what the writer still had in flight — or schedules a full recovery when
the close itself hung — so the next start repairs the index instead of opening an
unfinished one.

### Health Checks

`/api/health` answers `200` once this process owns the data directory and has
finished starting, and `503` until then. The body is the same JSON `/api/startup`
returns, so a probe that is failing also says which phase it is waiting on:

```text
$ curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/api/health
503
$ curl -s http://localhost:8080/api/health
{"ready":false,"phase":"Database","detail":"running migrations","done":3,"total":9,...}
```

Point the hosting platform's health check at that path. Without one a platform
assumes the container is ready the moment it starts, so requests arrive while
migrations, the instance-lock wait, or a search index recovery are still
running. They get the startup page or a `503` — correct answers, but not what
anyone expects from a deploy.

The probe deliberately reads no database, index, or file. Rolltop's failure mode
under memory pressure is slowness rather than death, and a probe that waits on
storage turns a slow instance into a killed one; that is how a stalled index
writer once cost a tenant its entire search index. Readiness here is answered
from state already in memory. Use the `ROLLTOP_MEMORY_LIMIT` guidance above for
the slowness itself — that is not what a health check can fix.

**One data directory belongs to one process, so the deployment has to stop the
old instance before starting the new one.** A platform that instead keeps the
previous instance running until the replacement reports healthy will deadlock
against the instance lock: the new process cannot finish starting until the old
one releases the directory, and the old one is not stopped until the new one is
healthy. For a single-writer application, stopping first is the correct strategy
in any case. Where the platform cannot be configured that way, leave the health
check path unset and accept the startup page during the handover rather than
pointing the probe at something that answers `200` before it can serve.

### Overlapping Deployments

One data directory belongs to one Rolltop process. That is enforced with an
`flock` on `/data/.rolltop-instance.lock`, taken before anything opens the
Bleve indexes or the blob store, and held until the process exits. The database
no longer needs it — PostgreSQL handles concurrent clients — but Bleve still
does: a second process cannot open those indexes at all.

Platforms that deploy without downtime start the replacement container before
stopping the one it replaces, so for a few seconds two processes want the same
directory. The starting process waits for the lock instead of refusing it, for
up to `ROLLTOP_STARTUP_LOCK_WAIT` (default `2m`), which covers the previous
process draining HTTP and closing its plugins and index.
The wait is visible in the log and on the startup page:

```text
waiting for the previous rolltop process to release /data (held by pid 1), 5s so far
previous rolltop process released /data after 7s
```

The HTTP listener is up throughout the wait, the same as during migrations: a
TCP check connects, `/api/startup` answers `200` with the current phase, and a
page request gets the startup page. `/api/health` answers `503` for the whole
wait, because this process cannot serve yet — which is why a deployment that
holds the old instance until the new one is healthy has to be reconfigured to
stop first, as the section above describes. Set the value to `0` to refuse a
directory another process still owns, which is what earlier versions always
did. A wait that runs out is reported with the process that holds the
lock and how long it was given.

The maintenance command that needs the directory to itself — `reset-search` —
keeps failing immediately instead of waiting,
because the answer there is to stop the server rather than to wait for it.

Two notes on what this does and does not protect:

- PostgreSQL allows as many client processes as its connection limit does; that is what
  its locking and `busy_timeout` are for. What it cannot survive is a
  filesystem where locking is unreliable, which is the case on NFS and SMB
  volumes. Keep `/data` on a local volume — the Bleve indexes cannot be opened
  by a second process at all, and their memory-mapped segments are as unsafe on
  a network filesystem as SQLite was.
- The lock only works if `flock` works on the volume. If the log shows two
  processes serving the same directory at once, that is what to check first.

### One Server Per Database

The directory lock above is per volume, so it does not catch the case where two
deployments have their own `/data` and point at one `ROLLTOP_DATABASE_URL`.
That is caught separately, by a PostgreSQL advisory lock a serving process
holds for as long as it is running. Without it both servers come up and neither
says so: each start stamps the other's in-flight sync runs interrupted, and
both runners then fetch every mailbox and push every flag change twice.

A start that cannot take it waits out `ROLLTOP_STARTUP_LOCK_WAIT`, the same as
for the directory, and then refuses with the session that holds it:

```text
postgres: claim this database: another rolltop server is already running against this
database. [...] It is held by backend pid 214 (rolltop-instance-lock) from 10.42.0.9,
connected since 2026-08-23T10:11:02Z, idle for 4s
```

The lock ends with the session that holds it, so an ordinary stop, a crash and a
`SIGKILL` all release it. The case that needs more than that is a container
killed without its connection being closed — an out-of-memory kill, an evicted
pod, a lost node. Nothing tells PostgreSQL the client is gone, so the backend
stays `idle` holding the lock, and with the operating system's default TCP
keepalives it stays that way for over two hours. Every start in that window
refuses, naming a server that no longer exists.

Two things keep that from being an outage. The lock session sets its own
keepalives, so PostgreSQL notices the dead peer in about a minute rather than in
two hours; and the process holding the lock pings it every 15 seconds, so a
starting server can tell a running holder from an abandoned one. A holder that
carries the `rolltop-instance-lock` name and has not pinged for 75 seconds is a
process that no longer exists, and the starting server ends that session and
takes the lock:

```text
instance lock: taking it from pid 214, a rolltop lock session silent for 2m14s
(its process is gone; a running one pings every 15s)
```

That recovery is deliberately narrow. A session that does **not** carry that
name — an older Rolltop, or anything else that took the same key — might be a
running server whose liveness cannot be read this way, so it is reported rather
than ended. If you have established that nothing else is running, set
`ROLLTOP_BREAK_INSTANCE_LOCK=true` for one start to take the lock from it, and
unset it again afterwards.

The override will not take a lock from a session that is still answering — one
that carries the marker and pinged within the last 75 seconds is a running
server, and it is refused with that said in the log. That is what keeps an
override left set in the environment from handing a healthy server's database
to the next deployment that overlaps it. It is still not a setting to leave on:
against anything the guard cannot read it does exactly what it says.

Advisory locks are scoped to a database, so several Rolltop deployments can
share one PostgreSQL cluster as long as each has its own database. They do not
see each other's locks.

### Backups

Back up the database with `pg_dump`, not by copying a volume:

```sh
pg_dump --format=custom --no-owner "$ROLLTOP_DATABASE_URL" \
  > "rolltop-$(date -u +%Y%m%dT%H%M%SZ).dump"
```

Run it on a schedule and keep the output somewhere other than the machine the
database runs on. A managed provider's own backups are disaster recovery for
the provider's failure; they are usually neither browsable nor restorable by
you, so they do not replace this.

Restore with `pg_restore` into an **empty** database. Rolltop refuses to start
against a database that holds tables it did not create, which is what stops a
half-restored target from being served.

Message blobs and the Bleve index under `/data` are deliberately not part of
this: blobs are re-fetchable from IMAP and the index is rebuilt from the
database. Snapshot `/data` if you would rather not re-download them, but it is
not required to recover.

### Automatic Search Index Recovery

Bleve writes pass through a shared priority- and byte-aware coordinator. It
admits at most one write per user, keeps bounded global concurrency, lets direct
rebuild/purge work pass queued attachment enrichment, and ages background work
so it cannot starve. Message projections are also split into bounded byte-sized
batches before reaching Bleve.
If an active Bleve write remains stuck for two minutes, Rolltop writes a durable
tenant recovery marker and starts a controlled shutdown. Cleanup gets a bounded
15-second grace period so another stuck subsystem cannot prevent process exit. A
Docker restart policy such as `--restart unless-stopped` (or Compose
`restart: unless-stopped`) is required for hands-off recovery.

The marker records which messages the abandoned batch owned. On restart, before
opening that tenant's index, Rolltop durably marks exactly those rows
pending with a full WAL checkpoint, clears the marker, and leaves the index in
place:

```text
repaired stalled search index user_id=1 pending_messages=6 first_document_id=162614 last_document_id=162620 index_retained=true
```

A commit that outran the watchdog is a report about how long a write took, not
about the index it was writing to — Bleve publishes each snapshot atomically, so
what survives is consistent and only the batch in flight is in doubt. Rebuilding
a multi-gigabyte index for that costs hours of reindexing, and that reindexing is
itself the load that makes the next commit slow enough to abandon.

Documents in that range whose messages were deleted while the stalled batch held
the writer gate are removed from the index during the repair, since their own
delete never reached Bleve. That sweep is what makes the repair complete, so a
range too wide to sweep is rebuilt instead of repaired: clearing the marker after
a partial repair would leave such a message searchable with nothing left to
notice.

A stall the marker cannot attribute to a message range falls back to the full
rebuild, as does one whose index is missing or no longer opens: Rolltop then renames `/data/users/<id>/bleve` to a timestamped
quarantine directory, persists that rename before clearing the recovery marker,
and creates a new derived index. The normal local indexing worker rebuilds it
from the database and retained local `.eml` blobs, including folders configured for
`manual` or `never` sync. Mail rows, IMAP state, and blobs are not deleted. If
retained raw data has expired, existing indexing behavior may retrieve it from
IMAP.

Quarantine directories hold a full copy of the index they replaced, so retention
prunes them alongside blobs: the newest one per tenant is kept for inspection,
older ones go immediately, and everything older than 48 hours goes regardless.
Each removal is named in the log.

Stall diagnostics are also appended to `/data/search-stall.log`, which is
rotated to `.prev` past 1 MiB and never truncated. The stack recorded there
names the blocked Bleve frame and is what distinguishes a writer waiting on
storage from one waiting on Bleve itself. It is written to the data volume
because a container log pipeline keeps only the first line of a multi-line
entry, and because a shell inside the container cannot read the process log at
all.

### Offline Search Index Reset

If automatic recovery cannot run, the offline `reset-search` command remains
the manual fallback. It can also repair a corrupt index that never opened far
enough to trigger the active-write watchdog. The command quarantines that
tenant's Bleve directory and rebuilds it from the local mirror.
The numeric local user ID is the number in `/data/users/<id>` and in log fields
such as `user_id=1`.

This resets only derived search data. It does not restore messages that are
absent from the user's local mirror. Deploy the mailbox-recovery fix and let
the local mailbox row count reach the remote count first; use `reset-search`
only if Bleve repair remains stalled after that. During the later rebuild,
Rolltop may fetch raw messages from the configured IMAP server when retained
local `.eml` data has expired.

Stop every Rolltop process that mounts the data volume, then run:

```sh
docker stop <rolltop-container>
docker run --rm \
  --env-file .env.rolltop \
  -e ROLLTOP_DATABASE_URL="$ROLLTOP_DATABASE_URL" \
  -v rolltop-data:/data \
  ghcr.io/grahamsz/rolltop:latest \
  reset-search --user-id 1 --confirm-offline
```

The command marks message rows pending before it quarantines the index, so it
needs the database and refuses to start without `ROLLTOP_DATABASE_URL`.

With the bundled `compose.yml` the one-off run inherits the image, environment,
and volumes of the `rolltop` service, including that variable, so nothing has
to be repeated. `--no-deps` keeps it from starting the database container, which
means the database has to be reachable already — with the bundled file it is,
because only the app is stopped:

```sh
docker compose stop rolltop
docker compose run --rm --no-deps rolltop \
  reset-search --user-id 1 --confirm-offline
```

The command never contacts IMAP or deletes message/blob data. It atomically
renames `/data/users/<id>/bleve` to a timestamped quarantine directory and
prints the exact restore paths. Start Rolltop normally to queue the rebuild.
The data-volume lock makes the command fail if a current Rolltop server still
holds that volume, but `--confirm-offline` remains mandatory for an explicit
operator check.

## V1 Flow

1. First admin creates the initial account at `/setup`.
2. Admin creates additional local users at `/admin/users`.
3. Each user logs in and configures their own IMAP account at `/settings/account`.
4. The user clicks `Sync now`, chooses per-folder `auto`, `manual`, or `never`, or scheduled sync runs on `ROLLTOP_SYNC_INTERVAL`.
5. Sync runs are planned per mailbox, with INBOX prioritized before background folders. Each mailbox task estimates pending work from IMAP `STATUS`, streams messages in UID batches, and updates current folder, UID, seen, total, stored, and skipped counts.
6. Every mailbox turn is time-bounded so one folder cannot hold the account-wide pass forever. A turn that runs out of time stops at a message boundary, commits what it mirrored, and is rescheduled immediately: a first mirror of a large folder therefore completes over many turns and is recorded as a series of normal runs rather than a failure. Each paused turn doubles the next one, up to ten minutes, so a backfill spends its time fetching instead of replanning the same folder; the folder returns to the short freshness budget as soon as a turn finishes cleanly. A turn that spends its whole budget without mirroring anything is still reported as an error.
7. A sync turn also has a memory budget. Fetch batches are planned from the sizes the server reports before the bodies are requested, so a batch is bounded in bytes and not only in messages, and a message larger than the budget is fetched on its own; search documents are trimmed to what Bleve can index before they are queued, and the queue commits when either its document count or its payload budget is reached. A folder holding a few very large mails therefore no longer decides how much memory the process needs.
8. Message bodies, attachment names, and searchable text-like attachments are indexed with the current user's `user_id`.
9. The database stores compact body previews; full body search lives in Bleve and message display uses the local raw `.eml` or fetches the message from IMAP by UID when the raw blob has aged out.
10. Raw `.eml` blobs are retained for `ROLLTOP_BLOB_RETENTION` only, defaulting to 14 days. Set it to `0` to keep all raw blobs.
11. Attachment bytes are read from the raw `.eml` while indexing and are not stored as separate blobs for new syncs.
12. `/mail`, folder views, `/search`, and `/messages/{id}` only return current-user records.
13. Folder counts show unread messages.
14. Dragging a message onto a folder immediately removes it from the current view, shows a moving toast, and then applies the IMAP move.
15. Snooze is local and conversation-scoped: future snoozes are hidden from normal lists and search, then resurface at the top without moving or deleting remote mail. A genuinely new incremental reply clears the active snooze.
16. Cross-account duplicate copies are detected and hidden. An account that collects mail from the other accounts - Gmail fetching POP3 mail, or a provider-side forward - hands the mirror a second row for a delivery the addressed account already holds. A copy is hidden only when exactly one account in the group was actually addressed, meaning its own address appears in `To` or `Cc`; that account's row stays visible and the copies point at it. Mail addressed to several of the accounts, or to none of them because it arrived by Bcc or through a mailing list, keeps every copy visible, because the mirror cannot tell which delivery is the original. Sent, Drafts, and Trash copies are never hidden, and only a row that shows in All Mail can be the original a copy hides behind, so a Spam-filed message never explains away the copy that is still in an inbox. Hidden copies drop out of lists, thread views, folder counts, search results, and new-mail notifications, and deleting the visible original brings its copy straight back.

In account settings, `Folder scope` can be:

- `INBOX` for only inbox.
- `INBOX,Sent` for a comma-separated subset.
- `*` for all selectable IMAP folders.

Search results are drawn best match first and can be re-ordered by date in either direction from the results header. The choice is stored per user in the browser, separately from the mail list's own direction, and changing it returns to the first page because the result window is rebuilt from a different end. A date order replaces the ranking rather than tie-breaking it: relevance, the sender-history and contact boosts, and the recency nudge all stop deciding which results the page holds, so the mail is walked strictly by when it was written. That also applies to mail returning from a snooze, which a best-match list heads with the reminder it came back on and a date-ordered one places - and dates - by when it was written, like everything else in the list.

Search supports Gmail-style operators:

- `has:attachment`
- `filename:pdf` or `filename:"report.csv"`
- `is:read`
- `is:unread`

The web app is installable as a limited offline PWA. It caches the shell and a bounded, user-scoped snapshot of the first All Mail page so the most recent mail list can paint immediately while it refreshes. Browser notifications can be enabled from the top bar and use user-scoped VAPID Web Push subscriptions. The Android app uses the same server sender through UnifiedPush, with an embedded Play Services distributor and a 15-minute authenticated poll as fallbacks. Notifications are driven only by durable recent INBOX arrival events after the mailbox has completed its initial sync, so archive/backfill syncs do not create popups.

All Mail and every folder list can be sorted by date in either direction from the list header. The choice is stored per user in the browser and follows the reader from folder to folder; flipping it returns to the first page because the paging window is rebuilt from the other end. Mail lists support selection with batch read/unread and snooze actions plus `j`, `k`, and `x` keyboard navigation; `/` focuses search and thread shortcuts cover reply, reply-all, forward, and return-to-list. Hovering or keyboard-focusing a row on a pointer device reveals per-row Reply, Archive, Move to trash, Mark read/unread, and Snooze buttons that run the same undoable mutations as the selection toolbar. Move to trash, Archive, and Report spam file the whole conversation, so a thread holding copies in more than one account - mail addressed to several of them, or arriving by Bcc or through a mailing list, which duplicate detection leaves visible in each - is split and each copy moves into the Trash, Archive, or Junk folder of the account holding it. A folder belongs to one account, so that is one move per account rather than one move, and the accounts do not have to answer the same way: whichever account's move failed gives its rows back while the rest stay filed. A copy that is already in its own account's destination is left alone rather than standing in the way of the copies that still have somewhere to go, and only a conversation with nothing left to move answers that it is already there. The conversation header repeats those commands as a small toolbar — Reply, Reply all, Forward, Archive, Move to trash, Mark unread, and Report spam. The three moves are held behind the same undo toast as the row actions and cover the messages of the open conversation that share its folder, so a thread's Sent or Trash copies stay put; Mark unread instead reaches the whole thread, because opening it is what marked the whole thread read. Report spam moves the conversation into the account's Junk folder, which is also what teaches the optional spam filter. Android left/right row swipes are independently configurable for Trash, account-specific Archive folders, recurring snooze times, Mark read, or Mark unread. Compose keeps a bounded, per-user browser recovery copy, supports reusable local templates, and merges saved contacts with recent tenant-scoped correspondents. `Ctrl+Enter` (`Cmd+Enter`) sends from anywhere in the composer, and on a reply it is **Send & archive**, which is the two-step version of that -- send, go back, archive -- collapsed into the shortcut. A reply that files its conversation away returns to the list the reader came from, since that conversation is no longer in the view they were left standing in; sending alone keeps them where they are. Recovery never serializes attachment bodies. Thread views show explicitly source-labeled authentication results and conservative sender/link cautions.

Settings shows the hidden copies per account under `Incoming mail`, with `Scan for duplicates` to re-run detection over mail that was mirrored before detection existed, and `Move copies to Trash` to move every hidden copy into the Trash folder of the account holding it. The cleanup never deletes remotely and never touches the addressed account's mail: the copies land in the aggregating account's own Trash, so anything moved by mistake can be moved back. Stopping the copies at the source is still worth doing - a Gmail filter that skips the inbox puts the fetched mail in a label folder instead, and a folder set to `never` is not mirrored at all.

`Syncs & tasks` in the account menu, beside `Settings` and `Database`, lists
everything running in the background for the signed-in user: the folder syncs,
the workers behind them, and the Google syncs that keep their own schedule. It
carries the count of runs in flight, so a stuck sync is visible without opening
it. That is a question about the whole installation rather than about the folder
a sync happens to be touching, which is why it is not a row between the
mailboxes.

The sidebar leads with `Inbox`: All Mail minus each account's chosen Archive
folder, so it is everything that has not been filed away yet, across every
account. All Mail sits below it and shows the rest. Both are whole-account
lists rather than the `INBOX` folder of one account, which keeps its own entry
under Folders. The older `/mail/unarchived` address still resolves to `Inbox`.

Below them sit the categories, one list each and one category per message:
`Relevant`, `Newsletters`, `Forums`, `Notifications`, and `Invoices &
Contracts`. The first four are decided from the list and automation headers the
sender set, which is why the answer is the same one a reader can check for
themselves. `Invoices & Contracts` is the exception, because no header
distinguishes an invoice from a delivery notice: it is decided from the
message's own subject, body, and attachment names - a structured e-invoice
(ZUGFeRD, Factur-X, XRechnung), a file named after what it is, a document
number beside an amount, or, for mail that is only a robot talking, the word in
the subject. It is only ever taken out of `Notifications` and `Newsletters`.
Mail from a person, and a discussion list a person can answer, stay where they
are however much they read like paperwork - misfiling somebody's mail into a
paperwork list costs more than leaving one invoice among the notifications.

Dropping mail on a category, or the thread view's `File sender under ...`,
files that sender there for good, which is the correction for anything the
rules read wrongly. When those rules change, mail that is already filed is
re-read a batch at a time in the background (`store.CategoryVersion`); it keeps
the category it has until the new answer replaces it, so no list goes blank
while the pass runs. That pass only ever improves an answer: mail whose raw
message has aged out of blob retention is not re-read at all, because there is
nothing left to read it from, and neither is a message the bounded scan could
not get through - both keep what the headers, and the parse that had the whole
message, already decided.

Sent, Drafts, Trash and Junk are out of those lists by default, because they
answer "what is on my plate" and the user's own writing is not on it - a Sent
folder inside them puts every reply back in front of its author in All Mail, in
Inbox, and in the category its headers earned it, all at once. Each folder's
`All Mail` switch under folder settings decides this, so any of them can be put
back; only Junk is dropped by role whatever the switch says, because Report spam
promises the message is gone from these lists. Existing accounts are migrated
once, and a Sent folder switched back on stays on. The `Sent` and `Drafts` views
are built from the folder role instead and are unaffected, as is each folder's
own entry under Folders, so nothing becomes unreachable. A conversation row
stands for the newest message the list it sits in holds, not for the newest
message the thread holds anywhere, so a thread you answered keeps the date and
preview of the message you were answering and stays where that date puts it; the
reply is still in the thread when the conversation is opened, and still counts
towards the row's message count and its batch actions.

Three list-header actions work on a whole list rather than on selected rows.
`Archive older` moves everything the current list holds that is dated before a
chosen cutoff into each account's Archive folder. `Delete older` is its
counterpart and moves the same selection into each account's Trash instead; it
reaches every folder the list draws from rather than the Inbox alone, so
clearing a year of Newsletters reaches the newsletters filed away in folders of
their own. Both cutoffs can be named either way round — "older than 30 days",
which moves with the calendar, or one fixed day — and the day itself is kept, in
the reader's own calendar: the browser sends the instant that day begins at in
its timezone. Sent, Drafts, Trash, and Junk are never swept up by `Archive
older`, whether or not a whole-account list is set to show them, so filing a
received backlog leaves the user's own mail alone. The cutoff and that exclusion
are applied in SQL rather than after the fact, and a very large backlog is
handled in repeated passes that say how much they covered. `Empty Trash`
appears only on a folder carrying the Trash role and is the one place rolltop
deletes mail on the server instead of moving it: the folder is listed live,
flagged `\Deleted`, and expunged under a proven `UIDVALIDITY`, so mail the
mirror never downloaded goes too, and local rows, blobs, and search documents
are removed only for the UIDs the server confirms are gone.

### Retention

Deleting always means moving to the Trash; nothing leaves a mail server until a
Trash folder is emptied. Retention automates both halves of that, under
Settings, Preferences, Retention.

Each category — Relevant, Newsletters, Forums, Notifications, and the rest —
can name its own cutoff, again either relative or a fixed day, and mail past it
is moved into its own account's Trash exactly as pressing Delete would. A
category rule reaches the category across every folder it is filed in, archived
mail included, because a rule about Newsletters is a statement about newsletters
rather than about one list; Sent, Drafts, Trash, and Junk stay out, because they
are outside the whole-account scope those lists are built from. A category with
no rule deletes nothing, which is the state every category starts in.

The Trash is then emptied on a schedule, on every account, for everything it has
held longer than the retention length — 30 days by default, and switchable off.
That step is permanent and reaches the mail server, through the same expunge
`Empty Trash` uses: the folder is listed live and its `UIDVALIDITY` proved
before anything is deleted. Three things bound what it takes. The clock is when
a message *arrived* in the Trash rather than when it was sent, so mail thrown
away today keeps its full stay however old the mail itself is — which is what
makes the two halves compose rather than delete a year-old newsletter the moment
a category rule throws it away. It only ever names mail rolltop has mirrored,
since a message this install has never seen has no measurable stay. And it needs
a server that supports `UID EXPUNGE` (RFC 4315, advertised as `UIDPLUS`), which
most do: without it IMAP offers only the expunge that removes everything in the
folder flagged deleted, so a partial purge would take mail nobody asked about,
and rolltop refuses and says so rather than widening itself. Emptying a Trash
folder by hand still takes all of it, on any server.

Upgrading an existing install does not switch the automatic emptying on. The
migration records it as off for every account that already exists, because
nobody chose it; accounts created afterwards get the default above. Either way
it is one checkbox on the Retention page.

A saved policy runs shortly afterwards and then on a several-hour interval, per
user, yielding to whatever the reader is doing at the time.

rolltop uses IMAP `IDLE` for INBOX wakeups when the server supports it and keeps the scheduled INBOX poll as a fallback. Remote deletes and moves are reconciled after folder syncs by comparing local UIDs with the server's current UID set.

## License And Contributions

rolltop is intended to be distributed under the AGPL-3.0-or-later. By contributing code, documentation, assets, or other original work to this repository, you agree to license that contribution under AGPL-3.0-or-later unless you have a separate written agreement with the project owner.

## Development Checks

```sh
npm run build
npm run build:plugins
go test ./...
docker build -t rolltop:dev .
```

## Continuous Integration

CI is split into two workflows so that pull requests only pay for the checks
their changes can actually break.

`.github/workflows/pr.yml` runs on every pull request. A first job diffs the
pull request against its base and turns the changed paths into per-area flags;
each following job is gated on those flags:

| Job | Runs when | What it does |
| --- | --- | --- |
| `Go` | `*.go`, `go.mod`, `go.sum`, spam model or training data changed | `gofmt` check, `go vet ./...`, `go test ./...`, `-buildmode=plugin` link check for changed plugin backends, checked-in spam model verification |
| `Frontend` | `frontend/`, `plugins/*/frontend/`, `plugins/*/themes/`, `tsconfig.json`, `package*.json`, `vite.config.ts`, `vitest.config.ts`, `vite.plugins.config.ts` changed | `npm run typecheck`, `npm run test` (Vitest), `npm run build:vite`, plus `npm run build:plugins` when a plugin frontend changed |
| `Android` | `android/` changed | `:app:testDebugUnitTest` and `:app:lintDebug` (both compile the debug variant, so no separate APK assembly) |
| `Docker Image` | `Dockerfile` or `.dockerignore` changed | `docker build` without pushing |
| `PR Checks` | always | Aggregates the results; use this one as the required status check |

Anything under `.github/workflows/` or `.github/scripts/` forces every job to
run. A documentation-only pull request skips all of them and only pays for the
two coordination jobs. Superseded runs are cancelled automatically, and every
job has a hard timeout.

`.github/workflows/ci.yml` runs on pushes to `main`, on `v*` tags, and on
manual dispatch, and has three jobs. `verify` is the post-merge gate: on a normal
reviewed merge it trusts the checks the pull request already ran and does almost
nothing, but it re-runs the full Go suite when someone pushes straight to
`main`, when a merge touches a path the pull-request filter does not cover, or
when it cannot diff the push. That skip is only sound when pull requests are
required to be up to date with `main` before merging (branch protection or a
merge queue); see AGENTS.md. `release` is the packaging pipeline: frontend and
plugin builds, the signed Android APK, plugin shared objects, godoc, build
artifacts, and the GHCR image push — no coverage instrumentation, and none of it
per pull request. It is gated to `v*` tags and manual dispatch.

### Deploying to Hostim

`deploy` is the third job in `ci.yml`. It runs after `verify`, only for
`main` — a push there, or a manual dispatch of the workflow on that branch —
and asks Hostim to rebuild the hosted instance through the
[`hostimdev/action@v2`](https://github.com/hostimdev/action) action. The app is
a Git deployment: Hostim checks the repository out and builds this
repository's `Dockerfile` itself, which is why the job requests a `rebuild`
rather than pointing the app at an image. Nothing published by `release` is
involved; that image exists for self-hosters and for tagged versions.

The action waits for the build and the rollout, so the job goes green only once
the new version is actually serving, and red when the build fails or the app
never becomes healthy. A failed deployment is therefore a failed workflow run
and notifies like any other. Its wait is bounded at 1800 seconds, generous
because a cold Kaniko build of this image is slow; see the note at the top of
the `Dockerfile` for the builder flags that make it less so.

What the job does not decide is *which commit* ends up serving. A rebuild
carries no ref: Hostim checks `main` out itself, so the build is of the tip at
that moment, which a merge landing during the run has already moved. Running
after `verify` means no deploy is started behind a red post-merge gate — it is
not a guarantee that the commit named in the run is the one that went live, and
the step summary says "requested for" rather than "deployed" for that reason.
Pinning a deployment to a commit would mean an `image:` deploy of a per-commit
tag, and therefore an image published per merge, which this repository
deliberately does not do.

Three repository **secrets** drive it, under **Settings → Secrets and variables
→ Actions → Secrets**:

| Name | Value |
| --- | --- |
| `HOSTIM_API_TOKEN` | An API token from the Hostim dashboard, **Account Settings → API Tokens** |
| `HOSTIM_PROJECT` | The project ID, e.g. `hpr-123456` |
| `HOSTIM_APP` | The app name within that project |

All three are secrets, and none of them is a repository *variable*. That tab is
the one thing to get right: a value under **Variables** is invisible to this
workflow, and the job will tell you so rather than deploy.

Because they are secrets, the job cannot be gated on them — a job condition can
read `vars` and cannot read `secrets` — so it runs on every push to `main` and
decides in its first step:

- **None of the three set** — the step says so and the job ends green. A fork
  inherits a working pipeline instead of a deployment failure it could not fix.
- **All three set** — the rebuild is requested and waited on.
- **Some but not all** — the run fails with a message naming what is missing. If
  a missing value is present as a repository variable of the same name, the
  message says that too, because from the outside that is indistinguishable from
  a fork. A half-configured repository must not go quietly green while its
  deploys stop.

The step summary names no app and no project. Both are secrets, and a summary is
not the place to test whether redaction holds.

Deploys are serialised rather than cancelled: the workflow's concurrency group
sets `cancel-in-progress: false` throughout. Cancelling a run that is waiting on
Hostim stops the waiting and nothing else — the requested build keeps going,
rolls out unwatched, and the next run's rebuild lands on top of it. So a `main`
run waits for its predecessor instead. Bursts do not pile up: GitHub keeps at
most one run pending per group, so a rush of merges collapses to the one in
flight plus the newest. A fork pays a little for this, queueing `main` runs it
would rather cancel, because the credentials that would tell the two apart are
secrets and `concurrency` cannot read those either.
