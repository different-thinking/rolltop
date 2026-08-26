# go-imap v2 migration — decision and plan

**Status:** deferred (planned, not scheduled)
**Last reviewed:** 2026-08-26
**Owner of the seam:** `backend/imapclient`

## Where things stand

Rolltop's IMAP client is built on `github.com/emersion/go-imap v1.2.1`
(`go.mod`), together with `github.com/emersion/go-sasl`. The v1 line is in
maintenance mode; active development — including IMAP4rev2 support and a
reworked, context-aware client API — happens on `github.com/emersion/go-imap/v2`.

The dependency is well contained. Every import of the package lives under
`backend/imapclient/` (about two dozen files); nothing in `backend/syncer`,
`backend/web`, or the plugins touches go-imap types directly. The rest of the
app talks to IMAP through this package's own types, so a v2 migration is a
rewrite of one package's internals, not a change that ripples across the
codebase.

## Decision: defer

We stay on v1.2.1 for now. Reasons:

- **It works and the surface is isolated.** v1.2.1 is stable against the servers
  users run, and because the whole dependency sits behind `backend/imapclient`,
  staying on it does not leak an aging API into the rest of the code.
- **The migration is all-or-nothing and touches the riskiest layer.** v2's
  client is a different shape (command objects, streaming responses,
  context-per-command) rather than a drop-in. The port would rewrite the fetch,
  move, expunge, and flag-generation paths — exactly where the sync
  correctness invariants live (UIDVALIDITY proven before every mutating
  operation, vanished-UID handling, crash-safe transfer claims). That is real
  regression risk for no user-visible gain today.
- **No forcing function yet.** Nothing we need is v2-only, and no security fix is
  currently stranded there.

## What would change the decision

Revisit — and schedule the migration — if any of these becomes true:

- A security or correctness fix lands only in v2 (v1 no longer gets it).
- We need an IMAP4rev2-only capability, or a server users rely on drops the
  compatibility v1 depends on.
- A transitive bump (go-sasl / go-message) makes staying on v1 awkward or
  unsupported.

## Migration sketch (when it is scheduled)

Treat it as a self-contained, multi-day chunk on its own branch, with the
existing `backend/imapclient` tests as the guardrail:

1. **Freeze the seam.** Confirm nothing outside `backend/imapclient` imports
   go-imap, so the package's exported types stay the only contract the rest of
   the app sees. Add tests for any seam behavior not already covered before
   touching the internals.
2. **Port command by command**, keeping the package's public API unchanged:
   connect/auth (with the XOAUTH2 path), `SELECT`/status, fetch (`fetcher.go`),
   move/expunge/flag generation, and UID reconciliation. Run the package's
   tests after each.
3. **Preserve the invariants explicitly.** UIDVALIDITY is proven before every
   mutating operation; a UID missing from a FETCH within one SELECT is treated
   as vanished, not fatal; transfer claims survive a crash. These are asserted
   by the current tests — keep them green throughout.
4. **Land behind the full suite**, not incrementally on `main`: the client layer
   is where a subtle regression is hardest to notice and most damaging.
