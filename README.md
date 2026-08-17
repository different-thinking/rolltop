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
- Google OAuth refresh and access tokens are encrypted at rest with the same key and are never returned by the API.
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
export ROLLTOP_MEMORY_LIMIT="80%"
export ROLLTOP_STARTUP_LOCK_WAIT="2m"
```

Set `ROLLTOP_COOKIE_SECURE=true` when serving over HTTPS.

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

The limit is a ceiling, not a reservation: the process only uses what it needs,
and going over it makes the collector work harder rather than failing an
allocation. Give the container at least a gigabyte for a first sync of a large
mailbox.

`ROLLTOP_STARTUP_INTEGRITY_CHECK` decides when SQLite files are verified during
startup. `auto` (the default) verifies them only after a run that did not shut
down cleanly, `always` verifies on every start, `never` disables the check.
Verification reads every page, so `always` costs startup time proportional to
the size of the mirror. A database found damaged is reported and set aside, not
repaired: startup continues so every other tenant keeps working and the admin
Database page stays reachable to schedule the repair.

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
and replaces the default `openid email https://mail.google.com/` entirely.

These are validated during startup: half a credential, an unusable redirect
URL, or credentials without any redirect URL stop the process rather than
surfacing as a failed consent much later. Connecting also needs a usable
`ROLLTOP_MASTER_KEY`, because the tokens are stored encrypted.

In the Google Cloud console, create a **Web application** OAuth client and
register the same redirect URLs. Signing in over IMAP and SMTP needs the scope
granted but no API enabled; the People and Calendar APIs become relevant only
when those integrations land. Google requires `https` for redirect URLs, except
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
`imap.gmail.com:993` and `smtp.gmail.com:465` and no password is stored; the
same choice is available for the outgoing SMTP server, so a Gmail account needs
no app password in either direction. Existing password accounts can be switched
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

### Logs and crash reports

All application logs go to **stderr**. `docker logs rolltop` shows them, but
anything that pipes or redirects only stdout (`docker logs rolltop | grep …`,
`docker logs rolltop > out.log`) silently drops every log line — use
`docker logs rolltop 2>&1 | grep …` instead.

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

### Corrupt SQLite Databases

`database disk image is malformed` is SQLite reporting that the file itself is
damaged, so every operation for that tenant fails until the file is repaired.
Rolltop names the damaged file and the repair command in its logs, for example:

```text
sync user_id=1 mailboxes=INBOX: store message mailbox "INBOX" UID 48882: user 1
database /data/users/1/rolltop.db is corrupt: database disk image is malformed;
stop rolltop and run "rolltop recover-db --user-id 1 --confirm-offline"
```

Everything below is also available to admins under **Account menu → Database**,
which is the shorter path: it lists every database with its size, WAL size, and
integrity state next to the free space on the data volume, runs the integrity
check and backups in the background while Rolltop keeps serving, and schedules
a repair. Scheduling a repair writes a durable marker and restarts Rolltop,
because a database can only be replaced while nothing holds a handle on it; the
repair then runs during startup and its report appears on the same page. The
commands below do the same work from a shell.

Repair is always an explicit step, and it never modifies the damaged file.
Stop every Rolltop process that mounts the data volume, then check which files
SQLite considers damaged:

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
`/data/users/<id>/rolltop.db.corrupt-<stamp>`, and installs the recovered file
only after verifying it. A recovered database that fails that verification is
discarded and the original is restored, so a failing disk under the destination
cannot cost you the file you still had.
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

### Overlapping Deployments

One data directory belongs to one Rolltop process. That is enforced with an
`flock` on `/data/.rolltop-instance.lock`, taken before anything opens SQLite,
the Bleve indexes, or the blob store, and held until the process exits.

Platforms that deploy without downtime start the replacement container before
stopping the one it replaces, so for a few seconds two processes want the same
directory. The starting process waits for the lock instead of refusing it, for
up to `ROLLTOP_STARTUP_LOCK_WAIT` (default `2m`), which covers the previous
process draining HTTP, closing its plugins and index, and checkpointing SQLite.
The wait is visible in the log and on the startup page:

```text
waiting for the previous rolltop process to release /data (held by pid 1), 5s so far
previous rolltop process released /data after 7s
```

The HTTP listener is up throughout the wait, the same as during migrations: a
TCP check connects, `/api/startup` answers `200` with the current phase, and a
page request gets the startup page. A platform that stops the old container once
the new one looks healthy therefore gets its healthy answer, and the wait ends. Set the value to `0` to
refuse a directory another process still owns, which is what earlier versions
always did. A wait that runs out is reported with the process that holds the
lock and how long it was given.

The maintenance commands that need the directory to themselves — `check-db`,
`recover-db`, and `reset-search` — keep failing immediately instead of waiting,
because the answer there is to stop the server rather than to wait for it.
`backup-db` takes no lock at all and is meant to run while Rolltop serves.

Two notes on what this does and does not protect:

- SQLite itself does allow several processes on one database file; that is what
  its locking and `busy_timeout` are for. What it cannot survive is a
  filesystem where locking is unreliable, which is the case on NFS and SMB
  volumes. Keep `/data` on a local volume. The Bleve indexes are the stricter
  constraint: a second process cannot open them at all.
- The lock only works if `flock` works on the volume. If the log shows two
  processes serving the same directory at once, that is what to check first.

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
6. Every mailbox turn is time-bounded so one folder cannot hold the account-wide pass forever. A turn that runs out of time stops at a message boundary, commits what it mirrored, and is rescheduled immediately: a first mirror of a large folder therefore completes over many turns and is recorded as a series of normal runs rather than a failure. Each paused turn doubles the next one, up to ten minutes, so a backfill spends its time fetching instead of replanning the same folder; the folder returns to the short freshness budget as soon as a turn finishes cleanly. A turn that spends its whole budget without mirroring anything is still reported as an error.
7. A sync turn also has a memory budget. Fetch batches are planned from the sizes the server reports before the bodies are requested, so a batch is bounded in bytes and not only in messages, and a message larger than the budget is fetched on its own; search documents are trimmed to what Bleve can index before they are queued, and the queue commits when either its document count or its payload budget is reached. A folder holding a few very large mails therefore no longer decides how much memory the process needs.
8. Message bodies, attachment names, and searchable text-like attachments are indexed with the current user's `user_id`.
9. SQLite stores compact body previews; full body search lives in Bleve and message display uses the local raw `.eml` or fetches the message from IMAP by UID when the raw blob has aged out.
10. Raw `.eml` blobs are retained for `ROLLTOP_BLOB_RETENTION` only, defaulting to 14 days. Set it to `0` to keep all raw blobs.
11. Attachment bytes are read from the raw `.eml` while indexing and are not stored as separate blobs for new syncs.
12. `/mail`, folder views, `/search`, and `/messages/{id}` only return current-user records.
13. Folder counts show unread messages.
14. Dragging a message onto a folder immediately removes it from the current view, shows a moving toast, and then applies the IMAP move.
15. Snooze is local and conversation-scoped: future snoozes are hidden from normal lists and search, then resurface at the top without moving or deleting remote mail. A genuinely new incremental reply clears the active snooze.

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

All Mail and every folder list can be sorted by date in either direction from the list header. The choice is stored per user in the browser and follows the reader from folder to folder; flipping it returns to the first page because the paging window is rebuilt from the other end. Mail lists support selection with batch read/unread and snooze actions plus `j`, `k`, and `x` keyboard navigation; `/` focuses search and thread shortcuts cover reply, reply-all, forward, and return-to-list. Hovering or keyboard-focusing a row on a pointer device reveals per-row Reply, Archive, Move to trash, Mark read/unread, and Snooze buttons that run the same undoable mutations as the selection toolbar. The conversation header repeats those commands as a small toolbar — Reply, Reply all, Forward, Archive, Move to trash, Mark unread, and Report spam. The three moves are held behind the same undo toast as the row actions and cover the messages of the open conversation that share its folder, so a thread's Sent or Trash copies stay put; Mark unread instead reaches the whole thread, because opening it is what marked the whole thread read. Report spam moves the conversation into the account's Junk folder, which is also what teaches the optional spam filter. Android left/right row swipes are independently configurable for Trash, account-specific Archive folders, recurring snooze times, Mark read, or Mark unread. Compose keeps a bounded, per-user browser recovery copy, supports reusable local templates, and merges saved contacts with recent tenant-scoped correspondents. Recovery never serializes attachment bodies. Thread views show explicitly source-labeled authentication results and conservative sender/link cautions.

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
| `Frontend` | `frontend/`, `plugins/*/frontend/`, `plugins/*/themes/`, `tsconfig.json`, `package*.json`, `vite.*.config.ts` changed | `npm run typecheck`, `npm run build:vite`, plus `npm run build:plugins` when a plugin frontend changed |
| `Android` | `android/` changed | `:app:testDebugUnitTest` and `:app:lintDebug` (both compile the debug variant, so no separate APK assembly) |
| `Docker Image` | `Dockerfile` or `.dockerignore` changed | `docker build` without pushing |
| `PR Checks` | always | Aggregates the results; use this one as the required status check |

Anything under `.github/workflows/` or `.github/scripts/` forces every job to
run. A documentation-only pull request skips all of them and only pays for the
two coordination jobs. Superseded runs are cancelled automatically, and every
job has a hard timeout.

`.github/workflows/ci.yml` runs on pushes to `main`, on `v*` tags, and on
manual dispatch. It is the packaging pipeline: Go tests with coverage, frontend
and plugin builds, the signed Android APK, plugin shared objects, godoc, build
artifacts, and the GHCR image push. None of that runs per pull request.
