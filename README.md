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
export ROLLTOP_SQLITE_ACCESS="auto"
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

`ROLLTOP_SQLITE_ACCESS` decides how SQLite coordinates access to its files, and
it matters on exactly one kind of volume. WAL keeps its index in a `-shm` file
that every connection maps with `MAP_SHARED`, and it relies on POSIX byte-range
locks. A network or FUSE filesystem — CephFS, NFS, virtiofs, a Docker Desktop
share — guarantees neither. SQLite documents WAL as unsupported there, and the
symptom is not an error: databases corrupt hours later, repeatedly, with nothing
in the log connecting the damage to the storage.

`auto`, the default, reads the filesystem under the data directory and picks a
mode for it:

```text
rolltop sqlite access=exclusive filesystem=fuse (virtiofs or another FUSE mount)
rolltop warning: fuse (virtiofs or another FUSE mount) cannot be trusted with
SQLite's shared WAL index; databases are opened one connection at a time without it
```

- **`shared`** is ordinary WAL, chosen for local filesystems (ext4, xfs, btrfs,
  zfs, tmpfs, overlayfs). Each database keeps several connections so reads do not
  queue behind the sync writer. A filesystem the list does not recognize is
  treated as local and named in the log line, so it can be reported.
- **`exclusive`** is WAL without shared memory: SQLite holds the index in heap
  memory and holds the file lock for as long as the connection lives. That
  removes both mechanisms the volume cannot provide. Rolltop already guarantees
  one process per data directory with its instance lock, so the exclusivity costs
  nothing there — but it does mean **one connection per database**, so reads
  queue behind writes, and no second process can open a database while Rolltop
  serves.

Set the value explicitly to overrule the detection in either direction; an
operator who knows their storage outranks a superblock lookup.

In exclusive mode the admin Database page still checks integrity and writes
backups, because it runs inside the serving process and uses the handle that
already owns each file. The `backup-db` command cannot: it is a separate
process, so it reports that Rolltop is serving and stops rather than failing
per database. Use the admin page, or stop the server first.

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
next open.

Corruption that keeps coming back on the same installation, every few hours and
without an obvious trigger, is almost always the filesystem. WAL needs a
coherent shared memory mapping for its index and working POSIX byte-range locks;
a network or FUSE volume (CephFS, NFS, virtiofs, a Docker Desktop share)
provides neither reliably. Check what the volume actually is:

```sh
grep ' /data ' /proc/mounts
```

Anything other than a local filesystem there is the finding. Rolltop detects
this at startup and switches to `ROLLTOP_SQLITE_ACCESS=exclusive`, which runs
WAL without the shared index; moving the volume onto a block device remains the
better fix, because it costs no concurrency. The other classic causes are a host
that lost power and a data directory copied while the server was running.

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
page request gets the startup page. `/api/health` answers `503` for the whole
wait, because this process cannot serve yet — which is why a deployment that
holds the old instance until the new one is healthy has to be reconfigured to
stop first, as the section above describes. Set the value to `0` to refuse a
directory another process still owns, which is what earlier versions always
did. A wait that runs out is reported with the process that holds the
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

The marker records which messages the abandoned batch owned. On restart, before
opening that tenant's index, Rolltop durably marks exactly those SQLite rows
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
from SQLite and retained local `.eml` blobs, including folders configured for
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
16. Cross-account duplicate copies are detected and hidden. An account that collects mail from the other accounts - Gmail fetching POP3 mail, or a provider-side forward - hands the mirror a second row for a delivery the addressed account already holds. A copy is hidden only when exactly one account in the group was actually addressed, meaning its own address appears in `To` or `Cc`; that account's row stays visible and the copies point at it. Mail addressed to several of the accounts, or to none of them because it arrived by Bcc or through a mailing list, keeps every copy visible, because the mirror cannot tell which delivery is the original. Sent, Drafts, and Trash copies are never hidden, and only a row that shows in All Mail can be the original a copy hides behind, so a Spam-filed message never explains away the copy that is still in an inbox. Hidden copies drop out of lists, thread views, folder counts, search results, and new-mail notifications, and deleting the visible original brings its copy straight back.

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

All Mail and every folder list can be sorted by date in either direction from the list header. The choice is stored per user in the browser and follows the reader from folder to folder; flipping it returns to the first page because the paging window is rebuilt from the other end. Mail lists support selection with batch read/unread and snooze actions plus `j`, `k`, and `x` keyboard navigation; `/` focuses search and thread shortcuts cover reply, reply-all, forward, and return-to-list. Hovering or keyboard-focusing a row on a pointer device reveals per-row Reply, Archive, Move to trash, Mark read/unread, and Snooze buttons that run the same undoable mutations as the selection toolbar. The conversation header repeats those commands as a small toolbar — Reply, Reply all, Forward, Archive, Move to trash, Mark unread, and Report spam. The three moves are held behind the same undo toast as the row actions and cover the messages of the open conversation that share its folder, so a thread's Sent or Trash copies stay put; Mark unread instead reaches the whole thread, because opening it is what marked the whole thread read. Report spam moves the conversation into the account's Junk folder, which is also what teaches the optional spam filter. Android left/right row swipes are independently configurable for Trash, account-specific Archive folders, recurring snooze times, Mark read, or Mark unread. Compose keeps a bounded, per-user browser recovery copy, supports reusable local templates, and merges saved contacts with recent tenant-scoped correspondents. `Ctrl+Enter` (`Cmd+Enter`) sends from anywhere in the composer, and on a reply it is **Send & archive**, which is the two-step version of that -- send, go back, archive -- collapsed into the shortcut. A reply that files its conversation away returns to the list the reader came from, since that conversation is no longer in the view they were left standing in; sending alone keeps them where they are. Recovery never serializes attachment bodies. Thread views show explicitly source-labeled authentication results and conservative sender/link cautions.

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

Two list-header actions work on a whole list rather than on selected rows.
`Archive older` moves everything the current list holds that is dated before a
chosen day into each account's Archive folder. The day itself is kept, and it is
the reader's own calendar day: the browser sends the instant that day begins at
in its timezone. Sent, Drafts, Trash, and Junk are never swept up, whether or not
a whole-account list is set to show them, so filing a received backlog leaves the
user's own mail alone. The cutoff and that exclusion are applied in SQL rather than
after the fact, and a very large backlog is archived in repeated passes that say
how much they covered. `Empty Trash`
appears only on a folder carrying the Trash role and is the one place rolltop
deletes mail on the server instead of moving it: the folder is listed live,
flagged `\Deleted`, and expunged under a proven `UIDVALIDITY`, so mail the
mirror never downloaded goes too, and local rows, blobs, and search documents
are removed only for the UIDs the server confirms are gone.

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
