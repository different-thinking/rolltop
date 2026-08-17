// File overview: rolltop process entrypoint and startup coordinator. The
// binary starts an HTTP listener first, serves a temporary startup page while
// schema migrations and service initialization run, then swaps in the real web
// handler. After readiness it owns background loops for sync polling, IMAP IDLE,
// blob retention, thread-header backfills, and graceful shutdown cleanup.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/buildinfo"
	"rolltop/backend/config"
	"rolltop/backend/imapclient"
	"rolltop/backend/logging"
	"rolltop/backend/plugins"
	"rolltop/backend/search"
	"rolltop/backend/smtpclient"
	"rolltop/backend/store"
	"rolltop/backend/syncer"
	"rolltop/backend/web"
)

type mailboxWatcher interface {
	WatchMailbox(ctx context.Context, account store.MailAccount, mailbox string, onChange func()) error
}

// errRestartForRecovery marks the deliberate exit that hands a stalled search
// index writer to the restart policy. It is an intended outcome, so crash
// reporting must not file it as a failure of this run.
var errRestartForRecovery = errors.New("restarting for offline recovery")

func isPlannedRestart(err error) bool { return errors.Is(err, errRestartForRecovery) }

func main() {
	var err error
	if len(os.Args) > 1 {
		err = runCommand(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	} else {
		err = run()
	}
	if err != nil {
		log.Fatal(err)
	}
}

// Startup state is intentionally process-local: it exists before the normal
// web server is ready, so the browser and API clients can see migration and
// initialization progress instead of a dead connection.
type startupSnapshot struct {
	Ready     bool   `json:"ready"`
	Failed    bool   `json:"failed"`
	Error     string `json:"error"`
	Phase     string `json:"phase"`
	Detail    string `json:"detail"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	StartedAt string `json:"started_at"`
}

type startupState struct {
	mu       sync.RWMutex
	snapshot startupSnapshot
}

func newStartupState() *startupState {
	return &startupState{snapshot: startupSnapshot{Phase: "Starting", Detail: "Preparing rolltop", Total: 1, StartedAt: time.Now().UTC().Format(time.RFC3339)}}
}

func (s *startupState) update(phase, detail string, done, total int) {
	if total <= 0 {
		total = 1
	}
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	s.mu.Lock()
	s.snapshot.Phase = phase
	s.snapshot.Detail = detail
	s.snapshot.Done = done
	s.snapshot.Total = total
	s.mu.Unlock()
}

func (s *startupState) ready() {
	s.mu.Lock()
	s.snapshot.Ready = true
	s.snapshot.Phase = "Ready"
	s.snapshot.Detail = "rolltop is ready"
	s.snapshot.Done = 1
	s.snapshot.Total = 1
	s.mu.Unlock()
}

func (s *startupState) fail(err error) {
	s.mu.Lock()
	s.snapshot.Failed = true
	s.snapshot.Error = err.Error()
	s.snapshot.Phase = "Startup failed"
	s.snapshot.Detail = err.Error()
	s.mu.Unlock()
}

func (s *startupState) snapshotCopy() startupSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

type startupGate struct {
	state *startupState
	mu    sync.RWMutex
	ready http.Handler
}

func (g *startupGate) setHandler(handler http.Handler) {
	g.mu.Lock()
	g.ready = handler
	g.mu.Unlock()
}

// startupGate is the temporary root handler. It serves startup status until
// startApp has built the real application handler, then delegates all normal
// traffic while keeping /api/startup available for diagnostics.
func (g *startupGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/startup" {
		writeStartupJSON(w, http.StatusOK, g.state.snapshotCopy())
		return
	}
	g.mu.RLock()
	ready := g.ready
	g.mu.RUnlock()
	if ready != nil {
		ready.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeStartupJSON(w, http.StatusServiceUnavailable, g.state.snapshotCopy())
		return
	}
	writeStartupHTML(w, g.state.snapshotCopy())
}

func writeStartupJSON(w http.ResponseWriter, status int, snapshot startupSnapshot) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(snapshot)
}

func writeStartupHTML(w http.ResponseWriter, snapshot startupSnapshot) {
	percent := 0
	if snapshot.Total > 0 {
		percent = snapshot.Done * 100 / snapshot.Total
	}
	if percent < 4 && !snapshot.Failed {
		percent = 4
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="theme-color" media="(prefers-color-scheme: light)" content="#f2f0eb">
<meta name="theme-color" media="(prefers-color-scheme: dark)" content="#10161f">
<title>rolltop starting</title>
<style>
:root{color-scheme:light;font-family:Inter,ui-sans-serif,system-ui,sans-serif;--bg:#f2f0eb;--surface:#fff;--text:#202426;--muted:#66716d;--border:#ded8d1;--track:#e6ded6;--accent:#b45c35;--danger:#b14532;background:var(--bg);color:var(--text)}@media (prefers-color-scheme:dark){:root{color-scheme:dark;--bg:#10161f;--surface:#1a2230;--text:#e2dfd9;--muted:#a49d94;--border:#525f73;--track:#2a3646;--accent:#e28c60;--danger:#f0907c}}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg)}.panel{width:min(520px,calc(100vw - 40px));border:1px solid var(--border);border-radius:10px;background:var(--surface);box-shadow:0 18px 60px rgba(0,0,0,.18);padding:28px}.brand{font-weight:800;font-size:28px;letter-spacing:0}.phase{margin-top:18px;font-size:15px;font-weight:700}.detail{margin-top:6px;color:var(--muted);line-height:1.45}.bar{height:8px;background:var(--track);border-radius:999px;overflow:hidden;margin-top:22px}.fill{height:100%%;width:%d%%;background:var(--accent);transition:width .25s ease}.error{margin-top:18px;color:var(--danger);font-weight:700}</style>
<script>
async function poll(){try{const r=await fetch('/api/startup',{cache:'no-store'});const s=await r.json();if(s.ready){location.reload();return}document.querySelector('.phase').textContent=s.phase||'Starting';document.querySelector('.detail').textContent=s.detail||'';const pct=s.total?Math.max(4,Math.min(100,Math.round((s.done/s.total)*100))):4;document.querySelector('.fill').style.width=pct+'%%';if(s.failed){document.querySelector('.error').textContent=s.error||'Startup failed';return}}catch(e){}setTimeout(poll,700)}setTimeout(poll,700)
</script>
</head>
<body>
<main class="panel">
<div class="brand">rolltop</div>
<div class="phase">%s</div>
<div class="detail">%s</div>
<div class="bar"><div class="fill"></div></div>
<div class="error">%s</div>
</main>
</body>
</html>`, percent, html.EscapeString(snapshot.Phase), html.EscapeString(snapshot.Detail), html.EscapeString(snapshot.Error))
}

func startupListenAddr() string {
	addr := strings.TrimSpace(os.Getenv("ROLLTOP_ADDR"))
	if addr == "" {
		return ":8080"
	}
	return addr
}

func listenStartupHTTP(addr string) (net.Listener, error) {
	return listenStartupHTTPWith(net.Listen, addr)
}

func listenStartupHTTPWith(listen func(network, address string) (net.Listener, error), addr string) (net.Listener, error) {
	listener, err := listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return listener, nil
}

type appRuntime struct {
	pluginHost      io.Closer
	db              *store.Store
	search          *search.Service
	handler         http.Handler
	restartRequired <-chan restartRequest
	// markSearchRecovery durably schedules a rebuild of every tenant search
	// index. It is called when the index close has to be abandoned, because the
	// process then exits with a possibly half-written Bleve batch and nothing
	// else would notice on the next start.
	markSearchRecovery func()
}

// restartRequest is a controlled-restart request from a subsystem that cannot
// finish its work inside the running process: a stalled search index writer, or
// an admin-scheduled database repair that needs the tenant file closed.
type restartRequest struct {
	UserID int64
	Reason string
}

const searchWriterRestartShutdownTimeout = 15 * time.Second

// The whole shutdown has to finish inside the container runtime's grace period
// — ten seconds by default for "docker stop" — because the step that matters
// most, closing SQLite so its WAL is checkpointed, comes last. These budgets
// leave room for that close instead of letting a slow HTTP drain or a stuck
// index writer consume the grace period and turn every restart into a SIGKILL.
const (
	httpDrainTimeout          = 3 * time.Second
	interruptedSyncRunTimeout = 2 * time.Second
	derivedCloseTimeout       = 3 * time.Second
)

var errSearchWriterRestartShutdownTimeout = errors.New("search writer restart cleanup timed out")

func (a *appRuntime) close() {
	a.closeWithin(derivedCloseTimeout)
}

// closeWithin releases the runtime in dependency order but gives the derived
// subsystems only a bounded share of the shutdown budget. Whatever has not
// finished by then is abandoned so the database close still runs: an abandoned
// writer can only fail its own statement, while a skipped database close leaves
// a hot WAL behind on every restart.
func (a *appRuntime) closeWithin(derivedBudget time.Duration) {
	if a == nil {
		return
	}
	deadline := time.Now().Add(derivedBudget)
	if a.pluginHost != nil {
		closeWithBudget("plugin host", time.Until(deadline), a.pluginHost.Close)
	}
	if a.search != nil {
		if !closeWithBudget("search index", time.Until(deadline), a.search.Close) && a.markSearchRecovery != nil {
			// Only the stall handler writes recovery markers otherwise, so
			// without this an abandoned close leaves an index that fails to
			// open on the next start and is never rebuilt.
			a.markSearchRecovery()
		}
	}
	if a.db != nil {
		// Deliberately unbounded: this is the write that decides whether the
		// next start opens a checkpointed database or replays a WAL.
		if err := a.db.Close(); err != nil {
			log.Printf("close databases: %v", err)
		}
	}
}

// closeWithBudget reports whether the subsystem finished closing. A false
// return means the close was skipped or abandoned, and the caller has to assume
// the subsystem's on-disk state is unfinished.
func closeWithBudget(name string, budget time.Duration, closeFn func() error) bool {
	if budget <= 0 {
		log.Printf("skipping %s shutdown: shutdown budget already spent", name)
		return false
	}
	done := make(chan error, 1)
	go func() { done <- closeFn() }()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			log.Printf("close %s: %v", name, err)
		}
		return true
	case <-timer.C:
		log.Printf("%s did not close within %s; closing databases anyway", name, budget)
		return false
	}
}

func runSearchWriterRestartShutdown(timeout time.Duration, shutdown func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- shutdown()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("%w after %s", errSearchWriterRestartShutdownTimeout, timeout)
	}
}

func shutdownServingApp(app *appRuntime, server *http.Server, serverErr <-chan error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpDrainTimeout)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	err := <-serverErr
	// Recorded after the drain: no handler can start a new sync run once the
	// listener is closed, and the write cannot eat the drain budget. Startup
	// repeats this cleanup, so a shutdown too short to finish it loses nothing.
	if app.db != nil {
		markCtx, cancelMark := context.WithTimeout(context.Background(), interruptedSyncRunTimeout)
		defer cancelMark()
		if n, markErr := app.db.MarkRunningSyncRunsInterrupted(markCtx); markErr != nil {
			log.Printf("mark interrupted sync runs during shutdown: %v", markErr)
		} else if n > 0 {
			log.Printf("marked interrupted sync runs during shutdown: %d", n)
		}
	}
	return err
}

// run starts the HTTP listener before backend initialization. That lets slow
// database migrations or index opens show progress in the browser rather than
// making the app look down.
func run() (runErr error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Arm crash reporting before anything can fail. A port conflict or an
	// unusable configuration is exactly the kind of fatal that crash-loops a
	// container, and the container log may not survive the next restart.
	crash := armCrashOutput(config.DataDirFromEnv())
	// Fallback for failures before the instance lock exists. The lock-scoped
	// defer below is registered later, so it runs first and makes this one a
	// no-op on every path that gets that far.
	defer func() { crash.finish(runErr) }()

	startup := newStartupState()
	gate := &startupGate{state: startup}
	server := &http.Server{
		Addr:              startupListenAddr(),
		Handler:           gate,
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := listenStartupHTTP(server.Addr)
	if err != nil {
		return err
	}
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("rolltop starting on %s", listener.Addr())
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			serverErr <- nil
			return
		}
		serverErr <- err
	}()

	cfg, err := config.Load()
	if err != nil {
		startup.fail(err)
		log.Printf("rolltop startup failed: %v", err)
		select {
		case <-ctx.Done():
		case listenErr := <-serverErr:
			return listenErr
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return err
	}
	logging.SetLevel(cfg.LogLevel)
	// Name the resolved storage paths before anything opens them, so a
	// misconfigured deployment (volume mounted somewhere Rolltop does not
	// write) is visible in the first lines of the container log.
	log.Printf("rolltop storage data_dir=%s db=%s index=%s", cfg.DataDir, cfg.DatabasePath, cfg.IndexPath)
	lock, err := acquireInstanceLock(cfg.DataDir)
	if err != nil {
		startup.fail(err)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-serverErr
		return err
	}
	defer lock.Close()
	// Registered after the lock defer so it runs before the lock is released:
	// once another process can acquire the lock, this run's marker and crash
	// log are no longer its own to clean up.
	defer func() { crash.finish(runErr) }()
	crash.beginRun(buildinfo.Version)

	// The marker is claimed after the instance lock, so only the process that
	// owns the data directory can decide what the previous run's exit means.
	uncleanShutdown := claimRunningMarker(cfg.DataDir)
	if uncleanShutdown {
		log.Printf("previous rolltop run did not shut down cleanly")
	}

	appCtx, cancelApp := context.WithCancel(ctx)
	defer cancelApp()
	app, err := startApp(appCtx, cfg, startup, uncleanShutdown)
	if err != nil {
		startup.fail(err)
		log.Printf("rolltop startup failed: %v", err)
		select {
		case <-ctx.Done():
		case listenErr := <-serverErr:
			return listenErr
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return err
	}
	restartShutdownOwnsClose := false
	// The restart path closes the app on a goroutine that outlives its own
	// timeout, so this flag crosses goroutines and needs atomic access.
	var databasesClosed atomic.Bool
	defer func() {
		if !restartShutdownOwnsClose {
			app.close()
			databasesClosed.Store(true)
		}
		// The marker outlives any exit that did not get as far as closing
		// SQLite, so the next start treats that exit as unclean and verifies
		// the files before serving.
		if databasesClosed.Load() {
			releaseRunningMarker(cfg.DataDir)
		}
	}()
	gate.setHandler(app.handler)
	startup.ready()
	log.Printf("rolltop ready on %s", cfg.Addr)

	restart := restartRequest{}
	select {
	case <-ctx.Done():
	case restart = <-app.restartRequired:
		log.Printf("controlled restart requested user_id=%d reason=%q", restart.UserID, restart.Reason)
		cancelApp()
	case err := <-serverErr:
		if err != nil {
			return err
		}
		return nil
	}

	if restart.UserID > 0 {
		restartShutdownOwnsClose = true
		// The reason now names either a stalled index writer or an admin's
		// scheduled repair; either way the sentinel keeps this exit out of the
		// crash report, because it is a planned restart.
		restartErr := fmt.Errorf("%s; %w", restart.Reason, errRestartForRecovery)
		cleanupErr := runSearchWriterRestartShutdown(searchWriterRestartShutdownTimeout, func() error {
			shutdownErr := shutdownServingApp(app, server, serverErr)
			app.close()
			databasesClosed.Store(true)
			return shutdownErr
		})
		if cleanupErr != nil {
			// Deliberately not wrapping errRestartForRecovery: the restart was
			// planned, but a cleanup that did not complete is a real failure and
			// has to be recorded as one.
			log.Printf("search writer restart cleanup: %v", cleanupErr)
			return fmt.Errorf("%s; restart cleanup failed: %w", restart.Reason, cleanupErr)
		}
		return restartErr
	}
	if err := shutdownServingApp(app, server, serverErr); err != nil {
		return err
	}
	return nil
}

// startApp performs the blocking startup work in dependency order: schema,
// user stores, interrupted sync cleanup, search indexes, then web/sync services.
func startApp(ctx context.Context, cfg config.Config, startup *startupState, uncleanShutdown bool) (*appRuntime, error) {
	startup.update("System database", "opening", 0, 1)
	pluginManifests, err := plugins.LoadManifests(cfg.PluginDir)
	if err != nil {
		return nil, err
	}
	backendPlugins := plugins.NewBackendManager(cfg.PluginDir, pluginManifests)
	for _, manifest := range pluginManifests {
		if manifest.Backend == nil || manifest.Backend.Kind != "go-plugin" {
			continue
		}
		if _, _, err := backendPlugins.Plugin(manifest.ID); err != nil {
			log.Printf("backend plugin %s disabled after load failure: %v", manifest.ID, err)
		}
	}
	reporter := func(p store.MigrationProgress) {
		phase := "System database"
		if p.Scope == "user" {
			phase = "User databases"
		}
		detail := strings.TrimSpace(p.Migration + " - " + p.Step)
		startup.update(phase, detail, p.Done, p.Total)
	}
	db, err := store.OpenServerWithPluginManifests(cfg.DatabasePath, cfg.DataDir, pluginManifests, reporter)
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = db.Close()
		}
	}()

	// Repairs scheduled from the admin UI run here, while the tenant databases
	// still have no open handles. Failures are recorded per tenant and do not
	// stop the other tenants from starting.
	startup.update("User databases", "checking scheduled repairs", 0, 1)
	if _, err := runScheduledDatabaseRepairs(ctx, cfg.DataDir, pluginManifests, time.Now(), func(done, total int, detail string) {
		startup.update("User databases", detail, done, total)
	}); err != nil {
		return nil, err
	}

	if startupIntegrityCheckRequired(cfg.StartupIntegrityCheck, uncleanShutdown) {
		startup.update("User databases", "verifying after unclean shutdown", 0, 1)
		damaged, err := verifyUserDatabases(ctx, db, cfg.DataDir, func(done, total int, detail string) {
			startup.update("User databases", detail, done, total)
		})
		if err != nil {
			return nil, err
		}
		if len(damaged) > 0 {
			log.Printf("startup integrity check found %d damaged user database(s); repair them with rolltop recover-db", len(damaged))
		}
	}

	startup.update("User databases", "opening per-user stores", 0, 1)
	if err := db.PrepareUserStores(ctx, reporter); err != nil {
		return nil, err
	}
	for _, warning := range damagedDatabaseWarnings(db.CorruptDatabases()) {
		log.Print(warning)
	}

	startup.update("Sync state", "marking interrupted sync runs", 0, 1)
	if n, err := db.MarkRunningSyncRunsInterrupted(context.Background()); err != nil {
		log.Printf("mark interrupted sync runs: %v", err)
	} else if n > 0 {
		log.Printf("marked interrupted sync runs: %d", n)
	}

	startup.update("Messages", "backfilling thread keys", 0, 1)
	for {
		n, err := db.BackfillThreadKeys(context.Background(), 10000)
		if err != nil {
			log.Printf("backfill thread keys: %v", err)
			break
		}
		if n < 10000 {
			break
		}
	}

	startup.update("Search", "opening indexes", 0, 1)
	searchRoot := filepath.Join(cfg.DataDir, "users")
	searchSvc, err := search.OpenPerUser(searchRoot)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cleanup {
			_ = searchSvc.Close()
		}
	}()
	users, err := db.ServiceableUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users for stalled search recovery: %w", err)
	}
	startup.update("Search", "checking recovery markers", 0, max(1, len(users)))
	if _, err := recoverMarkedSearchIndexes(ctx, db, searchSvc, searchRoot, users, time.Now()); err != nil {
		return nil, err
	}
	restartRequired := make(chan restartRequest, 1)
	requestRestart := func(userID int64, reason string) {
		select {
		case restartRequired <- restartRequest{UserID: userID, Reason: reason}:
		default:
		}
	}
	searchSvc.SetActiveWriterStallHandler(func(userID int64) {
		requestRestart(userID, fmt.Sprintf("search index writer stalled for user %d", userID))
	})

	startup.update("Services", "initializing sync and web services", 0, 1)
	blobStore := blob.New(cfg.DataDir)
	imapFetcher := &imapclient.Fetcher{MasterKey: cfg.MasterKey}
	syncSvc := &syncer.Service{
		Store:         db,
		Blobs:         blobStore,
		Search:        searchSvc,
		Fetcher:       imapFetcher,
		Sender:        &smtpclient.Sender{MasterKey: cfg.MasterKey},
		BlobRetention: cfg.BlobRetention,
		PluginDir:     cfg.PluginDir,
		MasterKey:     cfg.MasterKey,
	}
	syncRunner := syncer.NewRunnerWithContext(ctx, syncSvc)
	webServer, err := web.New(web.Options{
		Store:          db,
		Blobs:          blobStore,
		Search:         searchSvc,
		Syncer:         syncSvc,
		SyncRunner:     syncRunner,
		MasterKey:      cfg.MasterKey,
		DataDir:        cfg.DataDir,
		DatabasePath:   cfg.DatabasePath,
		IndexPath:      cfg.IndexPath,
		PluginDir:      cfg.PluginDir,
		SessionTTL:     cfg.SessionTTL,
		CookieSecure:   cfg.CookieSecure,
		WebhookToken:   cfg.WebhookToken,
		Google:         cfg.Google,
		RequestRestart: requestRestart,
	})
	if err != nil {
		return nil, err
	}
	go backfillThreadHeaders(ctx, db, cfg.DataDir)
	if cfg.SyncInterval > 0 {
		// Do not wait one whole scheduler interval after boot. IDLE only watches
		// INBOX changes from this point forward; the initial account-wide pass is
		// what establishes/reconciles every configured folder's UID checkpoint.
		for _, user := range users {
			if !syncRunner.Start(user.ID) {
				log.Printf("startup sync user_id=%d not started", user.ID)
			}
		}
		go scheduledSync(ctx, db, syncRunner, cfg.SyncInterval)
	}
	for _, user := range users {
		syncRunner.StartAttachmentIndex(user.ID)
	}
	go reconcileStaleSyncRuns(ctx, db, 5*time.Minute)
	if cfg.InboxPollInterval > 0 {
		// IMAP IDLE is the primary low-latency path. A separate minute-by-minute
		// poll used to queue the same INBOX work while the IDLE watcher was healthy,
		// producing duplicate visible INBOX runs. Scheduled account sync remains
		// the fallback if IDLE disconnects.
		go inboxIdle(ctx, db, syncRunner, imapFetcher, cfg.InboxPollInterval)
	}
	if cfg.BlobRetention > 0 {
		go storageRetention(ctx, db, syncSvc, cfg.BlobRetention)
	}

	cleanup = false
	return &appRuntime{
		pluginHost: webServer, db: db, search: searchSvc, handler: webServer.Handler(),
		restartRequired: restartRequired,
		markSearchRecovery: func() {
			// Read the list at call time: a tenant created after startup is not
			// in the startup snapshot, and its index needs the marker just as
			// much. This runs before db.Close().
			current, err := db.ListUsers(context.Background())
			if err != nil {
				log.Printf("list users for search recovery markers: %v", err)
				current = users
			}
			for _, user := range current {
				if markErr := searchSvc.MarkSearchIndexRecoveryRequired(user.ID); markErr != nil {
					log.Printf("mark search recovery user_id=%d after abandoned index close: %v", user.ID, markErr)
				}
			}
		},
	}, nil
}

func reconcileStaleSyncRuns(ctx context.Context, db *store.Store, maxAge time.Duration) {
	if db == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := db.InterruptStaleSyncRuns(ctx, maxAge)
			if err != nil {
				log.Printf("reconcile stale sync runs: %v", err)
			} else if n > 0 {
				log.Printf("interrupted stale sync runs count=%d max_age=%s", n, maxAge)
			}
		}
	}
}

func storageRetention(ctx context.Context, db *store.Store, svc *syncer.Service, retention time.Duration) {
	run := func() {
		total := syncer.RetentionStats{}
		for {
			stats, err := svc.ApplyStorageRetention(ctx, retention, 500)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("storage retention: %v", err)
				}
				return
			}
			total.CompactedMessages += stats.CompactedMessages
			total.PrunedBlobs += stats.PrunedBlobs
			if stats.CompactedMessages < 500 && stats.PrunedBlobs < 500 {
				break
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		if total.CompactedMessages > 0 || total.PrunedBlobs > 0 {
			log.Printf("storage retention compacted_messages=%d pruned_blobs=%d", total.CompactedMessages, total.PrunedBlobs)
			vacuumCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if err := db.Vacuum(vacuumCtx); err != nil {
				log.Printf("storage retention vacuum: %v", err)
			}
		}
	}
	run()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func backfillThreadHeaders(ctx context.Context, db *store.Store, dataDir string) {
	for {
		checked, updated, err := db.BackfillThreadHeadersFromBlobs(ctx, dataDir, 500)
		if err != nil {
			log.Printf("backfill thread headers: %v", err)
			return
		}
		if updated > 0 {
			log.Printf("backfilled thread headers: checked=%d updated=%d", checked, updated)
		}
		if checked < 500 {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func inboxPoll(ctx context.Context, db *store.Store, runner *syncer.Runner, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			targets, err := inboxAutoTargets(ctx, db)
			if err != nil {
				log.Printf("inbox poll list accounts: %v", err)
				continue
			}
			for _, target := range targets {
				if !runner.QueueAccountMailboxes(target.UserID, target.Account.ID, []string{target.Mailbox.Name}) {
					log.Printf("inbox poll user_id=%d account_id=%d not queued: sync runner stopped",
						target.UserID, target.Account.ID)
				}
			}
		}
	}
}

func inboxIdle(ctx context.Context, db *store.Store, runner *syncer.Runner, watcher mailboxWatcher, retryEvery time.Duration) {
	if watcher == nil {
		return
	}
	if retryEvery <= 0 {
		retryEvery = time.Minute
	}
	active := map[string]context.CancelFunc{}
	var mu sync.Mutex
	startMissing := func() {
		targets, err := inboxAutoTargets(ctx, db)
		if err != nil {
			log.Printf("inbox idle list accounts: %v", err)
			return
		}
		wanted := map[string]bool{}
		for _, target := range targets {
			key := inboxIdleTargetKey(target.UserID, target.Account.ID, target.Mailbox.Name)
			wanted[key] = true
			mu.Lock()
			if _, ok := active[key]; ok {
				mu.Unlock()
				continue
			}
			mu.Unlock()
			watchCtx, cancel := context.WithCancel(ctx)
			mu.Lock()
			active[key] = cancel
			mu.Unlock()
			go func(target inboxAutoTarget, key string) {
				defer func() {
					cancel()
					mu.Lock()
					delete(active, key)
					mu.Unlock()
				}()
				log.Printf("inbox idle user_id=%d account_id=%d mailbox=%s: subscribing", target.UserID, target.Account.ID, target.Mailbox.Name)
				for watchCtx.Err() == nil {
					err := watcher.WatchMailbox(watchCtx, target.Account, target.Mailbox.Name, func() {
						log.Printf("inbox idle user_id=%d account_id=%d event: queue inbox sync", target.UserID, target.Account.ID)
						if !runner.QueueAccountMailboxes(target.UserID, target.Account.ID, []string{target.Mailbox.Name}) {
							log.Printf("inbox idle user_id=%d account_id=%d not queued: sync runner stopped",
								target.UserID, target.Account.ID)
						}
					})
					if watchCtx.Err() != nil {
						return
					}
					if err != nil {
						log.Printf("inbox idle user_id=%d account_id=%d mailbox=%s: %v", target.UserID, target.Account.ID, target.Mailbox.Name, err)
					}
					timer := time.NewTimer(retryEvery)
					select {
					case <-watchCtx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}(target, key)
		}
		mu.Lock()
		for key, cancel := range active {
			if wanted[key] {
				continue
			}
			cancel()
			delete(active, key)
		}
		mu.Unlock()
	}
	startMissing()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			for _, cancel := range active {
				cancel()
			}
			mu.Unlock()
			return
		case <-ticker.C:
			startMissing()
		}
	}
}

type inboxAutoTarget struct {
	UserID  int64
	Account store.MailAccount
	Mailbox store.Mailbox
}

func inboxAutoTargets(ctx context.Context, db *store.Store) ([]inboxAutoTarget, error) {
	userIDs, err := db.ListUserIDsWithAccounts(ctx)
	if err != nil {
		return nil, err
	}
	var targets []inboxAutoTarget
	for _, userID := range userIDs {
		// Every failure below is reported through NoteError: this runs once a
		// minute per user, so a tenant whose database is damaged has to be
		// latched on the first pass. Latching names the file and its repair
		// command once and drops the tenant from ListUserIDsWithAccounts, rather
		// than logging the same unactionable driver message every minute until
		// the process is restarted.
		accounts, err := db.ListMailAccountsForUser(ctx, userID)
		if err != nil {
			log.Printf("inbox account list user_id=%d: %v", userID, db.NoteError(userID, err))
			continue
		}
		for _, account := range accounts {
			// The tenant database backs every account, so once it is latched the
			// remaining accounts would only reproduce the same failure and print
			// the same repair command again within this one pass.
			if db.DatabaseCorrupt(userID) {
				break
			}
			mb, err := inboxMailbox(ctx, db, userID, account)
			if err != nil {
				log.Printf("inbox mailbox user_id=%d account_id=%d: %v", userID, account.ID, db.NoteError(userID, err))
				continue
			}
			mode, err := db.EffectiveMailboxSyncMode(ctx, userID, account.ID, mb)
			if err != nil {
				log.Printf("inbox mailbox mode user_id=%d account_id=%d mailbox=%s: %v", userID, account.ID, mb.Name, db.NoteError(userID, err))
				continue
			}
			if mode != "auto" {
				continue
			}
			targets = append(targets, inboxAutoTarget{UserID: userID, Account: account, Mailbox: mb})
		}
	}
	return targets, nil
}

func inboxIdleTargetKey(userID, accountID int64, mailboxName string) string {
	return fmt.Sprintf("%d:%d:%s", userID, accountID, strings.ToLower(strings.TrimSpace(mailboxName)))
}

func inboxMailbox(ctx context.Context, db *store.Store, userID int64, account store.MailAccount) (store.Mailbox, error) {
	boxes, err := db.ListMailboxesForUser(ctx, userID)
	if err == nil {
		for _, box := range boxes {
			if box.AccountID == account.ID && box.Role == "inbox" {
				return box.Mailbox, nil
			}
		}
	}
	return db.GetOrCreateMailbox(ctx, userID, account.ID, "INBOX")
}

func scheduledSync(ctx context.Context, db *store.Store, runner *syncer.Runner, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			userIDs, err := db.ListUserIDsWithAccounts(ctx)
			if err != nil {
				log.Printf("scheduled sync list accounts: %v", err)
				continue
			}
			for _, userID := range userIDs {
				if !runner.Start(userID) {
					log.Printf("scheduled sync user_id=%d skipped: already running", userID)
				}
			}
		}
	}
}
