# IMAP Sync: Detailed Analysis and Remediation Plan

Scope: everything that talks to IMAP servers or schedules that work —
`backend/imapclient`, `backend/syncer` (fetch pipeline, runner, turn budget,
generation recovery, move/copy/trash, flags, reconciliation), the wiring in
`cmd/rolltop/main.go`, the web-API triggers, and the `remote_imap_sync` plugin.

The repository's own history shows this area has been patched reactively many
times ("Stop failing a move for mail the source folder no longer holds", "Keep
emptying a Trash folder going when the connection drops", "Log in once for a
batch of moves instead of per message", "Bound what a sync holds in memory").
The findings below explain *why* problems keep recurring: a handful of
structural decisions produce whole families of symptoms, and most past fixes
addressed one symptom at a time.

---

## Part 1 — Structural root causes

### R1. One TCP+TLS connection and one LOGIN per IMAP operation

`imapclient.Fetcher` has no session concept for ordinary sync. Every public
method dials, authenticates, SELECTs, runs one command, and terminates:

- `MailboxStatus`, `UIDs`, `SeenUIDs*`, `FlaggedUIDs*`, `FetchMailbox*`,
  `FetchUIDs*`, `FetchMessage`, `SetSeen*`, `SetFlagged*`, `AppendMessage*`,
  `SearchMailboxUIDsSince`, `SnapshotMailboxUIDs` — all begin with
  `f.loginWithinContext(ctx, account)` (`backend/imapclient/fetcher.go`,
  `flag_generation.go:30-64`, `remote_sync.go:125-140`).

Consequences, per one *single ordinary folder turn* (`syncAccount`):
STATUS (1 login) + fetch (1) + seen search (1) + flagged search (1) +
reconcile snapshot (1) ≈ **5 logins per folder per turn**. Worse:

- `PushPendingReadState(ctx, userID, 500)` / `PushPendingStarState`
  (`backend/syncer/flags.go:90-101,146-157`) call
  `SetSeenWithUIDValidity`/`SetFlaggedWithUIDValidity` per message — up to
  **500 sequential dial+login+SELECT+STORE cycles in one turn**, even when all
  500 messages live in the same folder.
- The copy path (`backend/syncer/copy.go:114`) performs **3–6 logins per
  copied message** (boundary snapshot, append, confirm fetch, up to two flag
  writes). A 1000-message copy is ~4000 logins.
- The move path already got the session treatment
  (`moveSessionExecutor`, `move.go:550`) — proof the pattern works; it was
  simply never applied anywhere else.

This is the primary driver of "immer wieder Probleme": Gmail, Dovecot,
Courier and most hosters throttle or tempfail rapid re-logins, enforce
per-user connection caps, and may flag the pattern as abuse. Symptoms then
show up randomly anywhere in sync as `LOGIN rate-limited`, `[THROTTLED]`,
`Too many simultaneous connections`, TLS handshake timeouts.

### R2. Flat 60-second command deadline regardless of data volume

`defaultIMAPCommandTimeout = 60s` (`fetcher.go:34`) is applied as
`client.Timeout` (a per-command socket deadline) and, for body fetches, as an
absolute wall-clock bound around the whole batch (`guardedUIDFetch`,
`fetcher.go:842-876`): the connection is *terminated* if the batch has not
completed in 60s.

- Body batches are planned up to `defaultFetchBatchBytes = 16 MiB`
  (`fetcher.go:41`). 16 MiB in 60 s requires ≥ ~2.2 Mbit/s sustained. On a
  slower or lossy link the same batch fails **deterministically forever**,
  because batch planning never adapts after a timeout.
- Full-folder `UID SEARCH 1:*` commands (reconcile snapshot, seen/flagged
  searches, `SnapshotMailboxUIDs` — which issues **two** full searches when a
  sync-start cutoff exists, `reconcile_uids.go:57-91`) must also finish inside
  60s. On 100k-message folders on slow servers this is marginal.
- Nothing distinguishes "server is sending data, just slowly" from "server is
  stuck". A progress-based deadline would.

### R3. No error classification, no backoff, no per-account health

The Google API clients in this codebase have a shared retry/backoff policy
(`backend/googleauth/client.go:217`). The IMAP path has **none**:

- A failing account is retried at full frequency: IDLE reconnect every
  `ROLLTOP_INBOX_POLL_INTERVAL` (default 1 min, `main.go:1163-1250`), the
  scheduled account pass every 15 min, generation recovery every 30 s
  (`arrival_scheduler.go:18`). A wrong password produces a steady stream of
  failed LOGINs — enough to trip provider lockouts.
- Except move/append "outcome unknown" wrappers
  (`syncer/imap_capabilities.go`), every error is opaque. Auth failures,
  DNS failures, TLS failures and tagged NO responses are all handled
  identically: log and retry at full speed.
- `syncUserWithOptions` (`syncer.go:420-429`) **returns on the first failing
  account**, so during an account-wide pass one dead account prevents the
  same-named mailbox job from ever reaching the remaining accounts.
- `PushPendingReadState`/`PushPendingStarState` failures abort the whole sync
  turn *before* any body is fetched (`syncer.go:532-542`) — a single stuck
  flag write blocks mail fetching for that account, every turn.

### R4. Hand-rolled scheduler with ~35 interlocking state maps

`syncer.Runner` (`runner.go:49-108`) serializes work with ~35 maps guarded by
one mutex, plus deferral/replay maps, epochs, gates, and several busy-wait
loops (`waitForForegroundYield` polls at 10 ms, `runner.go:1387-1406`;
`lockAfterSenderStats` can spin, `runner.go:206-208`; the recovery replay
reservation spins at 10 ms, `generation_recovery_gate.go:457-494`). Every new
feature (foreground barrier, sender stats, attachment indexing, generation
recovery, plugin coordination) added another dimension of hand-maintained
invariants. The concrete bugs in Part 2 (F-series) are all instances of this
complexity leaking.

---

## Part 2 — Concrete defects (verified, with locations)

Severity: **H** = causes user-visible data/consistency problems or permanent
stalls; **M** = reliability/performance defect; **L** = latent/cosmetic.

### A. Fetch/communication layer

| # | Sev | Finding |
|---|-----|---------|
| A1 | H | Connection-per-operation churn (R1). |
| A2 | H | Fixed 60 s deadline over 16 MiB batches; no adaptive batch shrink; slow links fail permanently (R2, `guardedUIDFetch`). |
| A3 | M | `FetchMessage`/one-shot paths use `c.Timeout` only; a single message larger than the link can deliver in 60 s can never be hydrated. |
| A4 | M | One IDLE connection per (user, account) INBOX (`main.go:1163`), retried without jitter; mass reconnect after a server restart is a login stampede. |
| A5 | L | `Fetcher.MoveMessage` is a stub that always errors (`fetcher.go:1348-1350`) but still satisfies the `syncer.Fetcher` interface — misleading API. |

### B. Sync pipeline / scheduling

| # | Sev | Finding |
|---|-----|---------|
| B1 | H | First failing account aborts the per-user account loop (`syncer.go:420-429`). |
| B2 | H | Flag-push failure fails the whole turn before fetching (`syncer.go:532-542`); contrast with flag *reads*, which are correctly demoted to log-only (`syncer.go:1139-1144`). |
| B3 | M | `reconcileStaleSyncRuns` uses `maxAge = 5 min` (`main.go:988`) while legitimate turns run 3–10 min and progress rows are only written when messages are handled. `InterruptStaleSyncRuns` then stamps the run `interrupted` with `finished_at`, and `FinishSyncRun`'s guard (`store/sync_runs.go:102`) makes the true outcome unwritable — run rows lie permanently. |
| B4 | M | `POST /api/account/sync` calls `Runner.Start` synchronously; `lockAfterExclusiveWriters` waits on tenant channels bounded only by the *runner* context, not the request context (`api_account.go:1084`, `runner.go:215-235`). Same call inside `scheduledSync`'s per-user loop (`main.go:1430`) — one blocked tenant stalls the scheduler for all tenants. |
| B5 | M | `QueueAccountMailboxes` returns `true` even when everything was deferred or dropped (`runner.go:370-392`); the API reports `queued:true` untruthfully. |
| B6 | M | `POST /api/sync-runs/{id}/cancel` cannot cancel maintenance runs (never registered in `runControls`) but the API forces `Cancellable: true` for running rows (`api_sync.go:241`); the row is stamped interrupted while the worker keeps running. Run-to-completion maintenance additionally uses a raw `WithCancel` and is invisible to the Activity cancel (`runner.go:584-587`). |
| B7 | M | `inboxPoll` (`main.go:1140-1161`) is dead code; the documented poll fallback for broken IDLE does not exist — `ROLLTOP_INBOX_POLL_INTERVAL` silently became the IDLE retry interval. |
| B8 | L | Busy-wait loops: 10 ms polling in `waitForForegroundYield`; unguarded `continue` spins when `done` channels are nil (`runner.go:206-208, 226-228, 1322-1324`). |
| B9 | L | `BeginForegroundOperation` acquire-timeout leaves the barrier held by a detached goroutine while the caller retries (`runner.go:1359-1369`) — plugin retry loops can starve ordinary folder syncs. |

### C. Move / journal

| # | Sev | Finding |
|---|-----|---------|
| C1 | H | `moveMessagesWithReceipts` checks `ctx.Err()` *after* the post-move verification search but *before* building outcomes (`imapclient/move_batch.go:141-143`). On cancellation it returns bare `context.Canceled` — not `MoveOutcomeUnknown` — so `applyMoveOutcome` marks **actually-applied moves as failed** and the destination receipts are discarded. |
| C2 | H | No age-based claim expiry: a transfer row claimed by this process but never settled (panic, killed goroutine) is unreconcilable until restart (`transfer_dispatch.go:25-30`; TTL sweep skips `pending`, `message_arrival_fingerprints.go:177-180`). Every retry says "already awaiting remote reconciliation" forever. Same for copy. |
| C3 | M | A batch cancelled *before dispatch* marks all messages failed instead of unattempted (`move.go:326-329` → `move_batch.go:32-34`). |
| C4 | M | Copy reconciliation hard-fails as "ambiguous" when a byte-identical copy pre-exists in the destination and never retries (`copy.go:204-206, 313`); >100 Message-ID candidates likewise (`transfer_reconciliation.go:159`). Permanent stuck transfers with no override or expiry. |
| C5 | M | `refusedMoveOutcomes` records a UID missing after a refused batch as "successfully moved" even though another client may have expunged/moved it (`move_batch.go:148-156`); local row is deleted unrecoverably. |
| C6 | M | Background move/copy/trash runs use `context.Background()` (`move.go:125`, `copy.go:56`, `empty_trash.go:89`): not cancellable at shutdown, "interrupted" paths dead, hung commands hold the tenant foreground reservation. |
| C7 | M | Copy path lacks everything move gained: no session reuse, no batching, first error kills the run, per-message progress writes (`copy.go:96-108`). |
| C8 | M | Non-UIDPLUS expunge fallback flags `\Deleted` then issues bare `EXPUNGE` (`expunge.go:103,125`) — removes any message another client had flagged `\Deleted` in that folder; and a failure between STORE and EXPUNGE leaves `\Deleted` flags behind. Accepted by comment, but worth an explicit safety check. |
| C9 | L | `prepareMessageMove` loads full bodies (`GetMessageForUser`) for a UID-only operation, only for the plugin preview (`move.go:758,894`). |

### D. Generation recovery

| # | Sev | Finding |
|---|-----|---------|
| D1 | H | Recovery ignores the account sync-start cutoff: prewarm and batch fetch use `snapshot.UIDs`, never `snapshot.Fetchable()` (`generation_rebuild_prewarm.go:52,67,74`; `fetchMailboxGenerationSnapshotBatch`). After a UIDVALIDITY reset the entire pre-cutoff history is re-downloaded, and the marker cannot finalize until it completes. The repair path does this correctly (`syncer.go:1263`) — recovery was forgotten. |
| D2 | H | Self-cancellation: an *ordinary* sync detecting a reset calls `MailboxGenerationRecoveryStarted` → `SignalMailboxGenerationRecovery` → `cancelOrdinaryMailboxWorkLocked`, which cancels **the caller's own context**; the very next statement runs `Search.PurgeMailbox(ctx, …)` on that cancelled context (`mailbox_generation.go:39-50`, `generation_recovery_gate.go:40-54`, `runner_work_diagnostics.go:164-182`). The mailbox-scoped index purge is skipped and — because the next pass sees `reset=false` — **never retried**: stale documents from the old generation survive, and the run is recorded `failed: context canceled`. |
| D3 | H | Recovery turns never get `withSyncTurnBudget` (`arrival_scheduler.go:172`), so the cooperative pause machinery is inert: when the 2-minute deadline fires mid-fetch, the turn takes the failure branch, `finishMailboxTurn` never runs on a detached context, and up to a search batch worth of documents is dropped while their rows are durable — those messages are never re-indexed. If 500 bodies cannot be fetched in 2 min, *every* turn ends this way. |
| D4 | M | `reserveGenerationRecoveryMailbox` cancels ordinary work *before* the guard that may still refuse the reservation (`generation_recovery_gate.go:791-821`) — an INBOX poll is killed for nothing and recovery still waits 30 s. |
| D5 | M | Flat 30 s retry forever, no backoff, no failure classification (`arrival_scheduler.go:160-167`); one permanently unfetchable folder keeps the tenant gated indefinitely (INBOX bypass only). |
| D6 | M | Per-turn cost: full `SnapshotMailboxUIDs` (1–2 whole-folder searches) at turn start **and again** in `refreshNewest` at batch end; ~200 full snapshots to recover a 100k folder at 500/turn (`syncer.go:1055-1057`). |
| D7 | M | `replayStoredGenerationRebuildArrivals` loads unbounded full `MessageRecord`s incl. bodies on every rebuild turn (`generation_rebuild_arrivals.go:18-22`, `store/mailbox_generation_rebuild_state.go:518-537`). |
| D8 | M | No cross-tenant concurrency cap: one recovery goroutine per gated tenant per 30 s pass, unbounded (`arrival_scheduler.go:131-143`). |
| D9 | M | `MessageUIDsForMailbox` used by prewarm is not generation-bound (no `uid_validity` predicate, `store/mailboxes.go:962-965`) — old-generation rows with reused UIDs suppress prewarm fetches. |
| D10 | L | Replay flag stranded on shutdown (`generation_recovery_gate.go:191,196-198,275`); epoch check bypassed on the `!wasActive` path (`:107-112`); silent state loss on restore-fingerprint ties (`mailbox_generation_rebuild_state.go:248-250`); dead code (`waitForGenerationRecoveryReplayMarkerCheck`, `generationRecoveryReplayWorkRunningLocked`). |

### E. remote_imap_sync plugin & lifecycle

| # | Sev | Finding |
|---|-----|---------|
| E1 | M | Plugin `routineManager` runs on `context.Background()` (`plugins/remote_imap_sync/backend/sync.go:57`); shutdown depends on `Server.Close()` within ~3 s of budget. Overrun → close abandoned, `db.Close()` runs while plugin workers still execute queries ("database is closed" mid-transfer), and the abandoned goroutine holds `backendLifecycleMu` forever (`backend_plugins.go:359-392`, `main.go:399-451`). |
| E2 | M | Persisted `next_retry_at` is never consulted; every restart triggers an immediate hot `"startup"` run for every routine, ignoring stored backoff (`sync.go:297,341-348`, `models.go:376-384`). |
| E3 | L | `MutateRoutine` blocks HTTP handlers on `wg.Wait()` (`sync.go:124-140`); `sourceAccount` stuffs routine ID into `MailAccount.ID` (`sync.go:961-967`). |

---

## Part 3 — Remediation plan

Ordered so that each phase is independently shippable and de-risks the next.
Phase 1 is pure bug-fixing (small diffs, no design change). Phase 2 removes
the two biggest structural causes (connections, timeouts, retry policy).
Phase 3 pays down the scheduler. Phase 4 hardens verification.

### Phase 1 — Correctness fixes (small, targeted, high value)

1. **Fix the generation-reset self-cancellation (D2).**
   In `ResetMailboxGenerationIfNeeded`, run `Search.PurgeMailbox` *before*
   signalling recovery, on a context derived with `context.WithoutCancel`
   bounded by its own timeout; or have `SignalMailboxGenerationRecovery`
   skip the caller's own cancellation key. Add a durable "index purge
   pending" bit to the rebuild marker so a skipped purge is retried by the
   recovery worker instead of being lost.
2. **Use `snapshot.Fetchable()` in generation recovery (D1)** everywhere the
   repair path already does; keep the full list only for checkpoint/notify
   decisions (mirroring `syncer.go:1237-1263`). Finalization must be based on
   the fetchable set.
3. **Apply `withSyncTurnBudget` to recovery turns (D3)** so they pause
   cooperatively and commit batches via `finishPausedTurnContext` like
   ordinary turns; treat a deadline-expired recovery turn as paused when it
   made progress.
4. **Move-outcome cancellation windows (C1, C3).** In
   `moveMessagesWithReceipts`, build outcomes from the verification search
   before honouring `ctx.Err()`; wrap post-dispatch cancellations in
   `MoveOutcomeUnknown`. In the syncer, a batch cancelled before dispatch
   must release claims via `FinishMessageTransferDispatch` (unattempted),
   not `MarkMessageTransferFailed`.
5. **Age-based claim expiry (C2).** Allow reconciliation when
   `dispatched_at < now - X` (e.g. 10 min) even for the same owner, or write
   `dispatch_finished_at` from a `defer` that survives panics. Add the same
   escape for copy.
6. **Continue the account loop on failure (B1)**: collect per-account errors,
   sync the remaining accounts, and report a joined error/status.
7. **Demote flag-push failures (B2)** to log-and-continue (matching flag
   reads), so a stuck STORE cannot block body fetching; optionally count and
   surface them in run progress.
8. **Stale-run reconciler (B3):** raise `maxAge` above the maximum turn
   budget (≥ 15 min) *and* remove the `FinishSyncRun` write-lock effect
   (allow a running worker to overwrite an interrupted stamp), or write a
   heartbeat `updated_at` from the turn even when no message was handled
   (planning phases).
9. **Tie long-lived work to the app lifecycle (C6, E1):** derive background
   move/copy/trash contexts and the plugin `routineManager` from the app
   context (keeping the existing `context.WithoutCancel` settle pattern for
   post-dispatch journal writes); make `Server.Close` wait for the manager
   with a bounded, observable join before the DB closes.
10. **Honest API answers (B4, B5, B6):** make `POST /api/account/sync`
    enqueue instead of blocking (return 202 + reason from
    `AccountMailboxBlockReason`); make `QueueAccountMailboxes` report
    queued/deferred/dropped truthfully; only report `Cancellable` for runs
    actually registered in `runControls`, and register maintenance runs.

### Phase 2 — Connections, timeouts, retry policy (the structural fix)

1. **Introduce an account-scoped IMAP session** (`AccountSession`) in
   `imapclient`, following the existing `MoveSession`/`SyncDestinationSession`
   pattern: one authenticated connection, sequential commands, SELECT cache,
   capability cache, context-watched socket. Thread it through one sync turn:
   plan (STATUS), fetch, flag reads, reconcile all reuse the same session;
   drop-and-relogin on error stays the recovery strategy. This alone removes
   ~80 % of logins. Keep the one-shot methods as compatibility wrappers.
2. **Batch flag pushes.** Group pending read/star changes by
   (account, mailbox, uidvalidity), SELECT once, and issue one
   `UID STORE <seqset> ±FLAGS.SILENT` per direction (the store already
   returns the pending sets; only the fetcher API needs a batched variant).
   500 changes become ≤ 4 commands on one connection.
3. **Progress-based command deadlines.** Replace the flat 60 s wall-clock in
   `guardedUIDFetch` with an activity watchdog: extend the deadline while
   bytes/messages keep arriving (e.g. idle limit 60 s, absolute cap
   base + size/minRate). On timeout, **halve the batch byte budget for that
   account** (persisted in-memory per account) so slow links converge instead
   of failing forever.
4. **Error taxonomy + per-account backoff.** Classify in `imapclient`:
   `AuthError` (tagged NO on LOGIN/AUTHENTICATE), `TransientError`
   (dial/TLS/reset/timeout), `ServerError` (tagged NO/BAD elsewhere). Keep an
   in-memory per-account health record in the runner: consecutive failures →
   exponential backoff with jitter (cap ~30 min) applied to scheduled syncs,
   IDLE reconnects, *and* generation-recovery attempts (D5); `AuthError`
   parks the account with a user-visible "needs credentials" status instead
   of hot-looping (the plugin already does exactly this — reuse the idea).
   Surface health in `/api/account/folders/progress`.
5. **Copy parity with move (C7):** reuse `SyncDestinationSession` for the
   destination and an `AccountSession` for the source, batch per mailbox,
   tolerate per-message failures with a consecutive-failure limit, pace
   progress writes.
6. **IDLE hygiene (A4, B7):** add jitter to reconnects; either delete
   `inboxPoll` and its config doc or actually wire it as the fallback when a
   watcher has been failing for N minutes.

### Phase 3 — Scheduler consolidation

1. Extract the reservation/deferral/replay logic from `Runner` into an
   explicit **per-user job queue with a single scheduler goroutine** per
   process: jobs (`sync mailbox`, `maintenance`, `recovery turn`,
   `attachment drain`, `sender stats`) carry priorities and exclusivity
   classes; the current 35 maps become one queue + one running-set. The
   busy-waits (B8), the nil-channel spins, the foreground-barrier
   abandonment (B9) and the gate-before-guard cancellation (D4) all
   disappear structurally rather than being patched individually.
   This can be done incrementally: first attachment/sender-stats, then
   ordinary mailbox jobs, then the recovery gate.
2. **Recovery loop:** bounded cross-tenant concurrency (semaphore, e.g. 4),
   `LIMIT` on the marker scan, per-target backoff state, and reuse of the
   turn-start snapshot for `refreshNewest` (D6) instead of a second full
   listing; bound `replayStoredGenerationRebuildArrivals` with a LIMIT and
   ID-only projection (D7); add the `uid_validity` predicate to
   `MessageUIDsForMailbox` or use the generation-bound variant in prewarm (D9).
3. Registered cancellation for *all* run types (B6) and truthful Activity
   reporting.

### Phase 4 — Verification & guardrails

1. **Integration tests against a real server** (Dovecot in CI, or
   go-imap's memory backend server): slow-link simulation (throttled conn)
   for the adaptive batch logic; cancellation mid-MOVE; UIDVALIDITY flip
   mid-turn; non-UIDPLUS expunge.
2. **Unit tests for each Phase-1 fix** (the repo's fake-fetcher harness is
   good — extend it with cancellation-injection hooks).
3. **Metrics/observability:** per-account counters for logins, commands,
   command latency, timeouts, backoff state; a log line when a batch budget
   is halved. These make the next regression diagnosable from logs alone.

### Suggested sequencing

- Phase 1 items 1–5 are the highest-value/lowest-risk and can land as one PR
  series within days; items 6–10 as a second series.
- Phase 2 item 1 (AccountSession) is the single most impactful change in the
  whole plan and unlocks items 2, 3 and 5; item 4 (backoff) is independent
  and can land in parallel.
- Phase 3 should not start before Phase 2 lands, to avoid refactoring the
  scheduler around call patterns that are about to change.

---

## Part 4 — Implementation status (this branch)

All four phases are implemented on this branch. Deviations from the plan as
written, with reasons:

- **Progress-based deadlines replaced the adaptive batch shrink.** The plan's
  Phase 2 item 3 proposed halving the per-account batch byte budget after a
  timeout. The implemented fix removes the root cause instead: connections are
  wrapped in a byte-level activity tracker (`activityConn`), and
  `guardedUIDFetch` terminates only when no byte arrives for a whole idle
  window. A slow link that keeps delivering data now finishes any batch size,
  so there is nothing left for a shrink policy to converge on. Untracked
  clients (tests) keep an absolute fallback deadline.
- **Session reuse is a per-turn scope, not a long-lived pool**
  (`syncer.AccountSessionScope` + `imapclient.Fetcher.sessionClient`): one
  cached connection per account for the duration of one sync turn / copy run /
  discovery pass, dropped at turn end. That captures ~80 % of the login
  savings without introducing pool lifetime/keepalive management.
- **D4 (gate armed before the reservation guard) is intentionally unchanged.**
  Cancelling the tenant's ordinary work when recovery cannot yet reserve is
  the designed preemption: it is what makes room for the next scan, and the
  cancelled turn's checkpoints are durable.
- **B9 (foreground barrier held after an acquire timeout) is intentionally
  unchanged**: releasing the barrier before the cancelled workers have
  checkpointed can strand their replay state (see the comment at the site).
  The busy-waits around it were softened to capped exponential polls instead.
- **B5 (`QueueAccountMailboxes` return value)**: on inspection every deferral
  branch records durable replay state that a later release replays, so `true`
  ("a future pass is guaranteed") is accurate; only a stopped runner returns
  `false`. No change.
- The scheduler was hardened in place (bounded recovery concurrency, LIMIT on
  the marker scan, backoff-aware attempts, capped polls, cancellation for all
  run types). The full job-queue rewrite from Phase 3 item 1 remains future
  work; the concrete defects it was meant to eliminate are fixed
  individually.
