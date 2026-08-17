# AGENTS.md

## Project Notes

Rolltop V1 is a Go, SQLite, Bleve, and local-blob email mirror. Keep all user-owned data scoped by `user_id` at every layer: SQLite rows, blob paths, search documents, sync runs, and HTTP reads.

## Rules For Future Agents

- SMTP sending and message moves exist and are supported; extend them rather than
  reintroducing the old prohibition. Remote delete is still deliberately absent.
- Read-state sync is intentionally allowed to update only the IMAP `\Seen` flag.
- Do not accept `user_id` from normal browser routes.
- Admin routes may manage local users, but must not expose other users' mail.
- Do not log app passwords, IMAP passwords, OAuth access or refresh tokens,
  authorization codes, session tokens, or raw message bodies.
- Keep IMAP credentials and OAuth tokens encrypted with `ROLLTOP_MASTER_KEY`.
- Keep tests for tenant isolation current when changing sync, search, message, attachment, blob, or route behavior.
- Keep sync incremental: fetch by UID after each mailbox's last stored UID, stream messages into storage, and update `sync_runs` progress during long runs.
- New attachment bodies should be indexed from raw `.eml` data and then discarded, not saved as separate attachment blobs.

## Checks

Run before handing off:

```sh
npm run build:themes
go test ./...
```

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
