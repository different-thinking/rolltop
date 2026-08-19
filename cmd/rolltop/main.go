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
	"strings"
	"sync"
	"syscall"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/buildinfo"
	"rolltop/backend/config"
	"rolltop/backend/googleauth"
	"rolltop/backend/googlecalendar"
	"rolltop/backend/googlepeople"
	"rolltop/backend/imapclient"
	"rolltop/backend/logging"
	"rolltop/backend/memlimit"
	"rolltop/backend/pgdsn"
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

// healthCheckPath is the readiness probe a hosting platform routes traffic on.
// It stays owned by the gate rather than the application handler, so it answers
// with the same meaning before and after startup finishes.
const healthCheckPath = "/api/health"

// serveHealth answers the readiness probe. It is 200 only once the real handler
// is installed and startup reports itself finished, which is what lets a
// platform hold the previous instance until this one can actually serve.
//
// It deliberately touches nothing else - no database, no search index, no
// filesystem. This process fails by becoming slow under memory pressure, not by
// dying, and a probe that waits on storage turns a slow instance into a killed
// one: that is how a stalled index writer previously cost a tenant its whole
// search index. Readiness here answers from memory that is already in hand.
func (g *startupGate) serveHealth(w http.ResponseWriter) {
	snapshot := g.state.snapshotCopy()
	g.mu.RLock()
	serving := g.ready != nil
	g.mu.RUnlock()
	status := http.StatusServiceUnavailable
	if serving && snapshot.Ready {
		status = http.StatusOK
	}
	writeStartupJSON(w, status, snapshot)
}

// startupGate is the temporary root handler. It serves startup status until
// startApp has built the real application handler, then delegates all normal
// traffic while keeping /api/startup and the health check available.
func (g *startupGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == healthCheckPath {
		g.serveHealth(w)
		return
	}
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
<meta name="theme-color" media="(prefers-color-scheme: light)" content="#f6f8fc">
<meta name="theme-color" media="(prefers-color-scheme: dark)" content="#131314">
<title>rolltop starting</title>
<style>
:root{color-scheme:light;font-family:Inter,ui-sans-serif,system-ui,sans-serif;--bg:#f6f8fc;--surface:#fff;--text:#1f1f1f;--muted:#5f6368;--border:#dadce0;--track:#e8eaed;--accent:#0b57d0;--danger:#b14532;background:var(--bg);color:var(--text)}@media (prefers-color-scheme:dark){:root{color-scheme:dark;--bg:#131314;--surface:#1e1f20;--text:#e3e3e3;--muted:#a2a9b0;--border:#444746;--track:#2c2e30;--accent:#a8c7fa;--danger:#f0907c}}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg)}.panel{width:min(520px,calc(100vw - 40px));border:1px solid var(--border);border-radius:10px;background:var(--surface);box-shadow:0 18px 60px rgba(0,0,0,.18);padding:28px}.brand{font-weight:800;font-size:28px;letter-spacing:0}.phase{margin-top:18px;font-size:15px;font-weight:700}.detail{margin-top:6px;color:var(--muted);line-height:1.45}.bar{height:8px;background:var(--track);border-radius:999px;overflow:hidden;margin-top:22px}.fill{height:100%%;width:%d%%;background:var(--accent);transition:width .25s ease}.error{margin-top:18px;color:var(--danger);font-weight:700}</style>
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

	// Keep the newest log lines in memory from the first one onwards. A hosted
	// operator reaches the admin database page but not the container log, so
	// this is the only route by which the line behind a 500 reaches them.
	//
	// The recorder comes first because MultiWriter stops at the first writer
	// that fails: with stderr leading, a full or closed pipe would silently
	// take the in-memory tail down with it, exactly when it is needed most.
	log.SetOutput(io.MultiWriter(logging.Recorder(), os.Stderr))

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
	// Install the heap ceiling before any service allocates. A first-time
	// account sync is the heaviest thing this process does, and without a limit
	// the collector lets the heap double past whatever the container allows,
	// which ends the process mid-write instead of ending an allocation.
	appliedMemory := memlimit.Apply(cfg.MemoryLimit)
	log.Printf("rolltop %s", appliedMemory.Description())
	// Name the resolved storage paths before anything opens them, so a
	// misconfigured deployment (volume mounted somewhere Rolltop does not
	// write) is visible in the first lines of the container log.
	log.Printf("rolltop storage data_dir=%s index=%s database=%s", cfg.DataDir, cfg.IndexPath, pgdsn.Describe(cfg.DatabaseURL))
	// Measuring the index means stat-walking every tenant's segment files, on the
	// storage whose latency this warning is about. Nothing depends on the result,
	// so it must not stand between the process and serving traffic - least of all
	// during the recovery restarts this release exists to make cheap.
	if cfg.SearchBackend != config.SearchBackendPostgres {
		go reportIndexMemoryHeadroom(appliedMemory, cfg.SearchRoot())
	}
	// A deployment that starts the replacement before stopping the process it
	// replaces makes both want this directory for a few seconds. Waiting is what
	// keeps the second process out of SQLite and the Bleve indexes without
	// turning a routine rollout into a failed start. The listener is already up,
	// so /api/startup answers throughout the wait and a health check sees a
	// process that is starting rather than one that is gone.
	lock, waited, err := waitForInstanceLock(ctx, cfg.DataDir, cfg.StartupLockWait, func(waited time.Duration, holder string) {
		startup.update("Data directory", "waiting for the previous rolltop process to exit", 0, 1)
		log.Printf("waiting for the previous rolltop process to release %s%s, %s so far",
			cfg.DataDir, holder, waited.Round(time.Second))
	})
	if err != nil {
		// Being told to stop while waiting is not a failed start: this process
		// opened nothing, so it has nothing to report and nothing to repair.
		stoppedWhileWaiting := errors.Is(err, context.Canceled)
		if stoppedWhileWaiting {
			log.Printf("stopped while waiting for the previous rolltop process to release %s", cfg.DataDir)
		} else {
			startup.fail(err)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-serverErr
		if stoppedWhileWaiting {
			return nil
		}
		return err
	}
	if waited > 0 {
		log.Printf("previous rolltop process released %s after %s", cfg.DataDir, waited.Round(time.Second))
	}
	defer lock.Close()
	// Registered after the lock defer so it runs before the lock is released:
	// once another process can acquire the lock, this run's marker and crash
	// log are no longer its own to clean up.
	defer func() { crash.finish(runErr) }()
	crash.beginRun(buildinfo.Version)

	appCtx, cancelApp := context.WithCancel(ctx)
	defer cancelApp()
	app, err := startApp(appCtx, cfg, startup)
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
	defer func() {
		if !restartShutdownOwnsClose {
			app.close()
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
func startApp(ctx context.Context, cfg config.Config, startup *startupState) (*appRuntime, error) {
	startup.update("Database", "connecting", 0, 1)
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
		detail := strings.TrimSpace(p.Migration + " - " + p.Step)
		startup.update("Database", detail, p.Done, p.Total)
	}
	db, err := store.OpenPostgres(ctx, cfg.DatabaseURL, store.PostgresOptions{
		MaxConns:       cfg.DatabaseMaxConns,
		Manifests:      pluginManifests,
		Progress:       reporter,
		DataDir:        cfg.DataDir,
		ConnectTimeout: cfg.DatabaseConnectTimeout,
		// One server per database. The data-directory lock above only catches
		// two containers sharing a volume; this catches two deployments with
		// their own volumes pointed at one DSN, which nothing else would.
		// It waits out a rolling deploy on the same budget the directory lock
		// uses, so an overlapping restart is not a failure.
		ExclusiveInstance: true,
		InstanceLockWait:  cfg.StartupLockWait,
	})
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = db.Close()
		}
	}()

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
	// Marking a tenant's folders is one UPDATE over their mailbox rows. It runs
	// while a sync waits for its index, so it is bounded rather than left to
	// inherit whatever deadline the caller happened to have.
	const corruptIndexReindexTimeout = 30 * time.Second
	searchRoot := cfg.SearchRoot()
	restartRequired := make(chan restartRequest, 1)
	requestRestart := func(userID int64, reason string) {
		select {
		case restartRequired <- restartRequest{UserID: userID, Reason: reason}:
		default:
		}
	}
	var searchSvc *search.Service
	users, err := db.ServiceableUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users for search startup: %w", err)
	}
	if cfg.SearchBackend == config.SearchBackendPostgres {
		// Search rows live in the relational database; none of the Bleve
		// lifecycle below - stall watchdog, recovery markers, quarantine -
		// applies. A tenant that has search-visible messages but no rows yet
		// (first start on this backend, or a rollback interim) gets its
		// folders marked here, and the sync repair path refills them the same
		// way it refills a quarantined Bleve index.
		searchSvc = search.OpenPostgresBackend(db)
		// Fuzzy matching needs pg_trgm and its index; both are optional at the
		// hoster's discretion, so a refusal only costs fuzzy, never the start.
		if err := db.EnsureTrigramSearch(ctx); err != nil {
			log.Printf("postgres search: fuzzy matching unavailable: %v", err)
		} else {
			log.Printf("postgres search: fuzzy matching enabled (pg_trgm)")
		}
		startup.update("Search", "checking postgres search coverage", 0, max(1, len(users)))
		for i, user := range users {
			if err := markPostgresSearchBackfill(ctx, db, user.ID); err != nil {
				return nil, err
			}
			startup.update("Search", "checking postgres search coverage", i+1, max(1, len(users)))
		}
	} else {
		searchSvc, err = search.OpenPerUser(searchRoot)
		if err != nil {
			return nil, err
		}
		// A writer stall is diagnosed from a stack that the container log keeps only
		// the first line of. Put it on the volume, beside crash.log, where a shell
		// in the container can still read it after the restart.
		searchSvc.SetStallDiagnosticsDir(cfg.DataDir)
		// An index that cannot be opened is moved aside and replaced rather than
		// failing every message that would have been indexed into it. The
		// replacement is empty, so every search-visible folder is marked here as
		// coverage nothing has verified - before the quarantine, so a crash in
		// between leaves a folder to rebuild rather than one that claims to be
		// indexed. Refilling it is the explicit rebuild, from the folder settings
		// or the admin database page; nothing does it in the background, because
		// re-reading a whole mailbox is not a thing to start behind the reader.
		searchSvc.SetCorruptIndexHandler(func(userID int64) error {
			markCtx, cancel := context.WithTimeout(context.Background(), corruptIndexReindexTimeout)
			defer cancel()
			marked, err := db.MarkUserSearchIndexRepairRequired(markCtx, userID)
			if err != nil {
				return err
			}
			log.Printf("search index unreadable, folders marked for rebuild user_id=%d folders=%d", userID, marked)
			return nil
		})
		startup.update("Search", "checking recovery markers", 0, max(1, len(users)))
		if _, err := recoverMarkedSearchIndexes(ctx, db, searchSvc, searchRoot, users, time.Now()); err != nil {
			return nil, err
		}
		searchSvc.SetActiveWriterStallHandler(func(userID int64) {
			requestRestart(userID, fmt.Sprintf("search index writer stalled for user %d", userID))
		})
	}
	defer func() {
		if cleanup {
			_ = searchSvc.Close()
		}
	}()

	startup.update("Services", "initializing sync and web services", 0, 1)
	blobStore := blob.New(cfg.DataDir)
	// One manager for the whole process: its refresh deduplication only holds
	// while sync workers, the sender and the web routes share an instance.
	googleAuth := googleauth.NewManager(
		googleauth.New(cfg.Google.ClientID, cfg.Google.ClientSecret, cfg.Google.RedirectURLs, cfg.Google.Scopes),
		db, cfg.MasterKey)
	// The People API offers no push, so contacts are polled. It shares the one
	// manager with mail so a refresh is deduplicated across both.
	googleContacts := googlepeople.NewSyncer(db, blobStore, googleAuth, googleAuth, googleauth.ScopeContacts)
	// The Calendar API offers push through watch channels, which need a publicly
	// reachable HTTPS endpoint. Until that exists calendars are polled the same
	// way, over the same manager.
	googleCalendar := googlecalendar.NewSyncer(db, googleAuth, googleAuth, googleauth.ScopeCalendar)
	imapFetcher := &imapclient.Fetcher{MasterKey: cfg.MasterKey, Tokens: googleAuth}
	syncSvc := &syncer.Service{
		Store:         db,
		Blobs:         blobStore,
		Search:        searchSvc,
		Fetcher:       imapFetcher,
		Sender:        &smtpclient.Sender{MasterKey: cfg.MasterKey, Tokens: googleAuth},
		BlobRetention: cfg.BlobRetention,
		PluginDir:     cfg.PluginDir,
		MasterKey:     cfg.MasterKey,
	}
	syncRunner := syncer.NewRunnerWithContext(ctx, syncSvc)
	webServer, err := web.New(web.Options{
		Store:            db,
		Blobs:            blobStore,
		Search:           searchSvc,
		Syncer:           syncSvc,
		SyncRunner:       syncRunner,
		MasterKey:        cfg.MasterKey,
		DataDir:          cfg.DataDir,
		DatabaseTarget:   pgdsn.Describe(cfg.DatabaseURL),
		DatabaseMaxConns: db.MaxConns(),
		IndexPath:        cfg.IndexPath,
		PluginDir:        cfg.PluginDir,
		SessionTTL:       cfg.SessionTTL,
		CookieSecure:     cfg.CookieSecure,
		WebhookToken:     cfg.WebhookToken,
		Google:           cfg.Google,
		GoogleAuth:       googleAuth,
		GoogleContacts:   googleContacts,
		GoogleCalendar:   googleCalendar,
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
	// Deliberately not tied to blob retention: a quarantined index is a full
	// copy of a tenant's index, and a deployment that turns blob retention off
	// still must not accumulate one per incident.
	go indexQuarantineRetention(ctx, searchRoot)
	if googleAuth.Configured() {
		go googleContactPoll(ctx, db, googleContacts, googleContactPollInterval)
		go googleCalendarPoll(ctx, db, googleCalendar, googleCalendarPollInterval)
	}

	cleanup = false
	return &appRuntime{
		pluginHost: webServer, db: db, search: searchSvc, handler: webServer.Handler(),
		restartRequired: restartRequired,
		markSearchRecovery: func() {
			if searchSvc.PostgresBackend() {
				// Postgres rows are transactional; an abandoned close leaves
				// nothing in doubt and nothing to mark.
				return
			}
			// Only a tenant whose writer is still inside Bleve has anything in
			// doubt. Marking every tenant would queue a full reindex for
			// accounts whose index was published in full, and that reindex is
			// itself the load that makes the next commit slow enough to abandon.
			recoveries := searchSvc.UnfinishedWriterRecoveries()
			if len(recoveries) == 0 {
				// The close itself was abandoned, not a batch: every writer had
				// returned and Bleve hung inside Close. Nothing names a range
				// then, and an index left half-closed still has to be rebuilt or
				// it fails to open on the next start with no marker to notice.
				log.Printf("abandoned search index close left no writer in flight; scheduling a full recovery")
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
				return
			}
			for userID, recovery := range recoveries {
				if markErr := searchSvc.MarkSearchIndexRecoveryRequiredForDocuments(
					userID, recovery.FirstDocumentID, recovery.LastDocumentID); markErr != nil {
					log.Printf("mark search recovery user_id=%d after abandoned index close: %v", userID, markErr)
					continue
				}
				log.Printf("scheduled search recovery user_id=%d after abandoned index close scope=%s",
					userID, recovery.Scope())
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
		accounts, err := db.ListMailAccountsForUser(ctx, userID)
		if err != nil {
			log.Printf("inbox account list user_id=%d: %v", userID, err)
			continue
		}
		for _, account := range accounts {
			mb, err := inboxMailbox(ctx, db, userID, account)
			if err != nil {
				log.Printf("inbox mailbox user_id=%d account_id=%d: %v", userID, account.ID, err)
				continue
			}
			mode, err := db.EffectiveMailboxSyncMode(ctx, userID, account.ID, mb)
			if err != nil {
				log.Printf("inbox mailbox mode user_id=%d account_id=%d mailbox=%s: %v", userID, account.ID, mb.Name, err)
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

// googleContactPollInterval is how often Google contacts are re-read. The
// People API has no push channel, so this is the only thing that notices a
// change made in Google's own UI. Fifteen minutes matches what the integration
// plan settled on: fast enough that an edit is not stale for long, slow enough
// that a delta call per connection is negligible against Google's quota.
const googleContactPollInterval = 15 * time.Minute

// googleContactPoll syncs every user's Google contacts on a timer.
//
// The first pass runs immediately rather than one interval after boot, matching
// what the mail scheduler does: nothing else notices a change made in Google's
// own UI, so waiting would leave every address book stale for a quarter of an
// hour after each restart.
//
// The parameter is named contacts, not syncer: this file imports a package by
// that name, and shadowing it here would make a later reference to it fail to
// compile for a reason that reads like nonsense.
func googleContactPoll(ctx context.Context, db *store.Store, contacts *googlepeople.Syncer, interval time.Duration) {
	if db == nil || contacts == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	syncEveryUsersContacts(ctx, db, contacts)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncEveryUsersContacts(ctx, db, contacts)
		}
	}
}

// syncEveryUsersContacts runs one pass over every serviceable user. It is
// sequential: contact syncs are short, and a burst of parallel People API calls
// across all users would be the one thing capable of hitting the quota.
func syncEveryUsersContacts(ctx context.Context, db *store.Store, contacts *googlepeople.Syncer) {
	users, err := db.ServiceableUsers(ctx)
	if err != nil {
		log.Printf("google contact poll list users: %v", err)
		return
	}
	for _, user := range users {
		if ctx.Err() != nil {
			return
		}
		// SyncUser already skips connections that cannot sync and records each
		// failure against its own connection, so one broken account does not
		// stop the others.
		if _, err := contacts.SyncUser(ctx, user.ID); err != nil {
			log.Printf("google contact poll user_id=%d: %v", user.ID, err)
		}
	}
}

// googleCalendarPollInterval is how often Google calendars are re-read. The
// integration plan settled on five to fifteen minutes; the lower end is chosen
// because an appointment moved an hour from now is worth noticing sooner than a
// renamed contact, and a delta call per calendar is cheap.
const googleCalendarPollInterval = 5 * time.Minute

// googleCalendarPoll syncs every user's Google calendars on a timer.
//
// The first pass runs immediately rather than one interval after boot, for the
// same reason the contact poll does: nothing else notices a change made in
// Google's own UI, so waiting would leave every week stale after a restart.
//
// The parameter is named calendars, not syncer: this file imports a package by
// that name, and shadowing it here would make a later reference to it fail to
// compile for a reason that reads like nonsense.
func googleCalendarPoll(ctx context.Context, db *store.Store, calendars *googlecalendar.Syncer, interval time.Duration) {
	if db == nil || calendars == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	syncEveryUsersCalendars(ctx, db, calendars)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncEveryUsersCalendars(ctx, db, calendars)
		}
	}
}

// syncEveryUsersCalendars runs one pass over every serviceable user. It is
// sequential for the same reason the contact pass is: a burst of parallel
// Calendar API calls across all users is the one thing capable of hitting the
// quota.
func syncEveryUsersCalendars(ctx context.Context, db *store.Store, calendars *googlecalendar.Syncer) {
	users, err := db.ServiceableUsers(ctx)
	if err != nil {
		log.Printf("google calendar poll list users: %v", err)
		return
	}
	for _, user := range users {
		if ctx.Err() != nil {
			return
		}
		// SyncUser already skips connections that cannot sync and records each
		// failure against its own connection, so one broken account does not
		// stop the others.
		if _, err := calendars.SyncUser(ctx, user.ID); err != nil {
			log.Printf("google calendar poll user_id=%d: %v", user.ID, err)
		}
	}
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

// indexQuarantineRetention prunes quarantined indexes at start and daily after.
func indexQuarantineRetention(ctx context.Context, searchRoot string) {
	pruneIndexQuarantines(searchRoot, time.Now())
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneIndexQuarantines(searchRoot, time.Now())
		}
	}
}

// pruneIndexQuarantines reclaims the disk a quarantined index holds. Quarantine
// renames a whole tenant index aside and nothing else ever removes it, so a
// deployment that quarantines repeatedly accumulates a full copy per incident.
// A failure here is logged and not returned: retention is housekeeping, and
// refusing to serve because a directory would not delete helps nobody.
func pruneIndexQuarantines(searchRoot string, now time.Time) {
	if strings.TrimSpace(searchRoot) == "" {
		return
	}
	pruned, err := search.PruneIndexQuarantines(searchRoot,
		search.DefaultIndexQuarantineKeep, search.DefaultIndexQuarantineMaxAge, now)
	if err != nil {
		log.Printf("prune quarantined search indexes: %v", err)
	}
	for _, quarantine := range pruned {
		log.Printf("pruned quarantined search index user_id=%d path=%q age=%s bytes=%d",
			quarantine.UserID, quarantine.Path, quarantine.Age.Round(time.Minute), quarantine.Bytes)
	}
}

// reportIndexMemoryHeadroom warns when the heap ceiling and the search index
// cannot both fit in the container's memory limit.
//
// The heap ceiling is a share of that limit, and the remainder has to cover
// goroutine stacks, SQLite's own allocations, and the Bleve segments Scorch
// reads through mmap. Those segments are page cache, so once the index outgrows
// what is left the kernel evicts the very pages the next commit needs, and every
// read becomes a major fault. On a FUSE-backed volume that turns a commit of a
// few kilobytes into one that runs for minutes, which is indistinguishable from
// a hung writer and is treated as one.
//
// This only ever logs. The deployment may be mid-import with an index that is
// still small, and refusing to start would take away the search it is warning
// about.
func reportIndexMemoryHeadroom(applied memlimit.Applied, searchRoot string) {
	if applied.Detected <= 0 || applied.Bytes <= 0 {
		return
	}
	footprint, err := search.MeasureIndexFootprint(searchRoot)
	if err != nil {
		log.Printf("measure search index footprint: %v", err)
		return
	}
	if footprint.Tenants == 0 {
		return
	}
	// Below this an index cannot be what is displacing the page cache, and a
	// warning about a few megabytes on a fresh deployment is noise that teaches
	// operators to ignore the line that matters later.
	const meaningfulIndexBytes = 64 << 20
	if footprint.Bytes < meaningfulIndexBytes {
		return
	}
	headroom := applied.Detected - applied.Bytes
	if headroom > 0 && footprint.Bytes <= headroom {
		return
	}
	log.Printf("rolltop warning: the search index is %s and the heap ceiling is %s of the %s limit, "+
		"leaving %s for everything the heap does not account for; Bleve reads its segments through mmap, "+
		"so an index larger than that is paged in and out on every commit. "+
		"Give the container more memory, or lower ROLLTOP_MEMORY_LIMIT so the index has room to stay resident.",
		memlimit.FormatBytes(footprint.Bytes), memlimit.FormatBytes(applied.Bytes),
		memlimit.FormatBytes(applied.Detected), memlimit.FormatBytes(max(headroom, 0)))
}

// markPostgresSearchBackfill schedules the fill of message_search for one
// tenant: when search-visible messages outnumber the rows that cover them, the
// tenant's folders are marked exactly as the corrupt-index handler marks them,
// and the sync repair path re-indexes through the same IndexMessages call -
// which on this backend writes rows.
//
// The comparison is a shortfall, not a presence check. The first start on this
// backend has no rows at all, but the case that outlives it is the interim: a
// spell served from Bleve leaves the rows behind by exactly the mail that
// arrived meanwhile, and mail that is present but unindexed is invisible to
// search with nothing to say so. Idempotent across restarts, since a tenant
// whose rows already cover its mail marks nothing.
//
// A surplus of rows is deliberately not acted on: rows for messages in folders
// that have since left search are stale but harmless, because every query
// joins messages and filters by the live folder.
func markPostgresSearchBackfill(ctx context.Context, db *store.Store, userID int64) error {
	indexed, err := db.CountMessageSearchForUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("count postgres search rows user_id=%d: %w", userID, err)
	}
	searchable, err := db.CountSearchEnabledMessagesForUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("count search-enabled messages user_id=%d: %w", userID, err)
	}
	if searchable == 0 || indexed >= searchable {
		return nil
	}
	marked, err := db.MarkUserSearchIndexRepairRequired(ctx, userID)
	if err != nil {
		return fmt.Errorf("mark postgres search backfill user_id=%d: %w", userID, err)
	}
	log.Printf("postgres search backfill scheduled user_id=%d folders=%d messages=%d indexed=%d",
		userID, marked, searchable, indexed)
	return nil
}
