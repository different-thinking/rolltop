# rolltop

rolltop is a single-container Go app that mirrors multiple IMAP inboxes per local user into local storage for search, viewing, composing, and mailbox moves. Production mail data stays in the user's own Docker instance. Project site: https://rolltop.app, coming soon. Contact: graham@rolltop.app.

## What It Stores

- SQLite metadata at `/data/rolltop.db`
- Per-user Bleve search indexes at `/data/users/{user_id}/bleve`
- Raw `.eml` and attachment blobs under `/data/users/{user_id}/blobs/...`
- Incremental sync progress in `sync_runs`
- A compiled React + Vite + TypeScript frontend served by the Go process

## Security Model

- Browser routes derive the current user from a server-side session.
- Normal user routes never accept `user_id` from browser input.
- Sessions use opaque random tokens; only SHA-256 token hashes are stored in SQLite.
- Cookies are `HttpOnly` and `SameSite=Lax`.
- POST routes require CSRF tokens.
- App passwords are hashed with Argon2id.
- IMAP passwords are encrypted at rest with `ROLLTOP_MASTER_KEY`.
- Admins can create users, but V1 does not give admins access to other users' mail.
- Message sending uses the configured SMTP server.
- Mailbox moves are explicit user actions and are mirrored to IMAP.
- Read-state sync may update only the IMAP `\Seen` flag when a message is read locally.
- Message authentication badges report bounded SPF, DKIM, and DMARC values found in received headers; Rolltop labels their source and does not claim to verify them independently.

## Configuration

Required:

```sh
test -f .env.rolltop || (
  umask 077
  printf 'ROLLTOP_MASTER_KEY=%s\n' "$(openssl rand -base64 32)" > .env.rolltop
)

set -a
. ./.env.rolltop
set +a
```

Common optional variables:

```sh
export ROLLTOP_ADDR=":8080"
export ROLLTOP_DATA_DIR="/data"
export ROLLTOP_DB_PATH="/data/rolltop.db"
export ROLLTOP_INDEX_PATH="/data/bleve"
export ROLLTOP_SESSION_TTL="720h"
export ROLLTOP_SYNC_INTERVAL="15m"
export ROLLTOP_INBOX_POLL_INTERVAL="1m"
export ROLLTOP_BLOB_RETENTION="336h"
export ROLLTOP_COOKIE_SECURE="false"
export ROLLTOP_WEBHOOK_TOKEN=""
export ROLLTOP_LOG_LEVEL="info"
export ROLLTOP_STARTUP_INTEGRITY_CHECK="auto"
```

Set `ROLLTOP_COOKIE_SECURE=true` when serving over HTTPS.

`ROLLTOP_STARTUP_INTEGRITY_CHECK` decides when SQLite files are verified during
startup. `auto` (the default) verifies them only after a run that did not shut
down cleanly, `always` verifies on every start, `never` disables the check.
Verification reads every page, so `always` costs startup time proportional to
the size of the mirror.

`ROLLTOP_LOG_LEVEL` defaults to `info`, which hides verbose `debug ...` log
lines (plugin loading, one-click unsubscribe traces). Set it to `debug` to
include them.

## Run Locally

```sh
npm install
npm run build
npm run build:plugins
go test ./...
test -f .env.rolltop || (
  umask 077
  printf 'ROLLTOP_MASTER_KEY=%s\n' "$(openssl rand -base64 32)" > .env.rolltop
)
set -a
. ./.env.rolltop
set +a
ROLLTOP_DATA_DIR="./data" go run ./cmd/rolltop
```

Open `http://localhost:8080`. If no users exist, `/setup` creates the first admin.

## Docker

```sh
docker pull ghcr.io/grahamsz/rolltop:latest

test -f .env.rolltop || (
  umask 077
  printf 'ROLLTOP_MASTER_KEY=%s\nROLLTOP_COOKIE_SECURE=false\n' "$(openssl rand -base64 32)" > .env.rolltop
)

docker run -d --name rolltop --restart unless-stopped -p 8080:8080 \
  --env-file .env.rolltop \
  -v rolltop-data:/data \
  ghcr.io/grahamsz/rolltop:latest
```

Keep `.env.rolltop` with the same care as the Docker volume. Changing or losing `ROLLTOP_MASTER_KEY` makes stored IMAP passwords undecryptable.

### Corrupt SQLite Databases

`database disk image is malformed` is SQLite reporting that the file itself is
damaged, so every operation for that tenant fails until the file is repaired.
Rolltop names the damaged file and the repair command in its logs, for example:

```
sync user_id=1 mailboxes=INBOX: store message mailbox "INBOX" UID 48882: user 1
database /data/users/1/rolltop.db is corrupt: database disk image is malformed;
stop rolltop and run "rolltop recover-db --user-id 1 --confirm-offline"
```

Repair is always an explicit offline step, and it never modifies the damaged
file. Stop every Rolltop process that mounts the data volume, then check which
files SQLite considers damaged:

```sh
ROLLTOP_SERVICE=<your-service-name>
docker compose stop "$ROLLTOP_SERVICE"
docker compose run --rm --no-deps "$ROLLTOP_SERVICE" check-db --confirm-offline
```

`check-db` runs SQLite's `quick_check` against the installation database and
every `/data/users/<id>/rolltop.db`, changes nothing, and exits non-zero when a
file is damaged. Repair a damaged tenant database with:

```sh
docker compose run --rm --no-deps "$ROLLTOP_SERVICE" \
  recover-db --user-id 1 --confirm-offline
```

`recover-db` copies every readable row into a freshly migrated database, steps
over the damaged pages, repairs foreign key references the lost rows leave
behind, moves the damaged file aside as
`/data/users/<id>/rolltop.db.corrupt-<stamp>`, and installs the recovered file.
It prints what survived per table. Mail that the IMAP server still holds is
re-downloaded by the next sync; locally created state on lost pages (contacts,
snoozes, identities, pending flag changes) is not recoverable. Because the row
set changed, the command prints the matching `reset-search` invocation when
anything was lost. The quarantined file is kept, so nothing is deleted.

Corruption comes from the storage under the data volume, not from a Rolltop
write path: Rolltop opens SQLite in WAL mode with one writer per tenant and an
exclusive lock on the data directory. A killed process does not corrupt a WAL
database — SQLite discards a partially written frame by its checksum on the
next open. Recurring corruption on the same installation usually means the
volume is on a network or overlay filesystem that does not honor SQLite's
locking, that the host lost power, or that the data directory was copied while
the server was running.

### Shutdown And Restart

Rolltop's shutdown ends with closing SQLite, which checkpoints the WAL into the
database file. Everything before that step is bounded so the close is reached:
the HTTP drain gets 3 seconds, the plugin host and search index together get 3
more, and whatever has not finished by then is abandoned. Give the container
enough grace for that sequence plus the checkpoint:

```yaml
services:
  rolltop:
    stop_grace_period: 60s
    restart: unless-stopped
```

Without it, `docker stop` sends SIGKILL after its default 10 seconds. That does
not corrupt the database, but it leaves a hot WAL that the next start has to
replay, and the next start then treats the run as unclean and verifies the
tenant files before serving (see `ROLLTOP_STARTUP_INTEGRITY_CHECK`).

### Backups

Copying `/data` with `cp`, `rsync`, `docker cp`, or a volume snapshot while
Rolltop runs produces a torn database: the committed state is split between
`rolltop.db` and its WAL. Use `backup-db`, which writes each database with
SQLite's `VACUUM INTO` from a single read transaction and is safe to run while
the server is serving:

```sh
docker compose exec "$ROLLTOP_SERVICE" \
  rolltop backup-db --output /data/backups/$(date -u +%Y%m%dT%H%M%SZ)
```

The copies mirror the data directory layout (`rolltop.db` and
`users/<id>/rolltop.db`) and are already checkpointed, so no WAL sidecar is
needed to read them. Message blobs and the Bleve index are deliberately not
copied: blobs are re-fetchable from IMAP and the index is rebuilt from the
database. Write backups outside the data volume if you want them to survive
losing that volume.

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

On restart, before opening that tenant's index, Rolltop durably marks the user's
search-visible SQLite rows pending with a full WAL checkpoint, renames only
`/data/users/<id>/bleve` to a timestamped quarantine directory, persists that
rename before clearing the recovery marker, and creates a new derived index.
The normal local indexing worker rebuilds it from SQLite and retained local
`.eml` blobs, including folders configured for `manual` or `never` sync. Mail
rows, IMAP state, and blobs are not deleted. If retained raw data has expired,
existing indexing behavior may retrieve it from IMAP.

### Offline Search Index Reset

If automatic recovery cannot run, the offline `reset-search` command remains
the manual fallback. It can also repair a corrupt index that never opened far
enough to trigger the active-write watchdog. The command quarantines that
tenant's Bleve directory and rebuilds it from the local mirror.
The numeric local user ID is the number in `/data/users/<id>` and in log fields
such as `user_id=1`.

This resets only derived search data. It does not restore messages that are
absent from the user's SQLite mirror. Deploy the mailbox-recovery fix and let
the local mailbox row count reach the remote count first; use `reset-search`
only if Bleve repair remains stalled after that. During the later rebuild,
Rolltop may fetch raw messages from the configured IMAP server when retained
local `.eml` data has expired.

Stop every Rolltop process that mounts the data volume, then run:

```sh
docker stop <rolltop-container>
docker run --rm \
  --env-file .env.rolltop \
  -v rolltop-data:/data \
  ghcr.io/grahamsz/rolltop:latest \
  reset-search --user-id 1 --confirm-offline
```

For Docker Compose, use your actual service name; the one-off command inherits
that service's image, environment, and volumes:

```sh
ROLLTOP_SERVICE=<your-service-name>
docker compose stop "$ROLLTOP_SERVICE"
docker compose run --rm --no-deps "$ROLLTOP_SERVICE" \
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
6. Message bodies, attachment names, and searchable text-like attachments are indexed with the current user's `user_id`.
7. SQLite stores compact body previews; full body search lives in Bleve and message display uses the local raw `.eml` or fetches the message from IMAP by UID when the raw blob has aged out.
8. Raw `.eml` blobs are retained for `ROLLTOP_BLOB_RETENTION` only, defaulting to 14 days. Set it to `0` to keep all raw blobs.
9. Attachment bytes are read from the raw `.eml` while indexing and are not stored as separate blobs for new syncs.
10. `/mail`, folder views, `/search`, and `/messages/{id}` only return current-user records.
11. Folder counts show unread messages.
12. Dragging a message onto a folder immediately removes it from the current view, shows a moving toast, and then applies the IMAP move.
13. Snooze is local and conversation-scoped: future snoozes are hidden from normal lists and search, then resurface at the top without moving or deleting remote mail. A genuinely new incremental reply clears the active snooze.

In account settings, `Folder scope` can be:

- `INBOX` for only inbox.
- `INBOX,Sent` for a comma-separated subset.
- `*` for all selectable IMAP folders.

Search supports Gmail-style operators:

- `has:attachment`
- `filename:pdf` or `filename:"report.csv"`
- `is:read`
- `is:unread`

The web app is installable as a limited offline PWA. It caches the shell and a bounded, user-scoped snapshot of the first All Mail page so the most recent mail list can paint immediately while it refreshes. Browser notifications can be enabled from the top bar and use user-scoped VAPID Web Push subscriptions. The Android app uses the same server sender through UnifiedPush, with an embedded Play Services distributor and a 15-minute authenticated poll as fallbacks. Notifications are driven only by durable recent INBOX arrival events after the mailbox has completed its initial sync, so archive/backfill syncs do not create popups.

Mail lists support selection with batch read/unread and snooze actions plus `j`, `k`, and `x` keyboard navigation; `/` focuses search and thread shortcuts cover reply, reply-all, forward, and return-to-list. Android left/right row swipes are independently configurable for Trash, account-specific Archive folders, recurring snooze times, Mark read, or Mark unread. Compose keeps a bounded, per-user browser recovery copy, supports reusable local templates, and merges saved contacts with recent tenant-scoped correspondents. Recovery never serializes attachment bodies. Thread views show explicitly source-labeled authentication results and conservative sender/link cautions.

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
