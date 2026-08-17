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
paths require: Go (`gofmt`, `go vet`, `go test`), frontend (`typecheck`, Vite
builds), Android (unit tests and lint), and a Docker build when the image
definition changes. Keep the path filters in that workflow's `changes` job in
sync when adding a new top-level area. The full packaging and publishing
pipeline lives in `.github/workflows/ci.yml` and runs on `main` and tags only.
