package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

func TestListenStartupHTTPReturnsBindFailureImmediately(t *testing.T) {
	bindErr := errors.New("bind failed")
	listener, err := listenStartupHTTPWith(func(network, address string) (net.Listener, error) {
		if network != "tcp" || address != ":8080" {
			t.Fatalf("listen called with %q, %q", network, address)
		}
		return nil, bindErr
	}, ":8080")
	if listener != nil {
		_ = listener.Close()
		t.Fatal("listener unexpectedly returned after bind failure")
	}
	if !errors.Is(err, bindErr) {
		t.Fatalf("bind error = %v, want wrapped bind failure", err)
	}
}

func TestStartupGateServesStartupHTMLForAppRoutes(t *testing.T) {
	gate := &startupGate{state: newStartupState()}
	req := httptest.NewRequest(http.MethodGet, "/mailbox/97/p3", nil)
	res := httptest.NewRecorder()

	gate.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
	if !strings.Contains(res.Body.String(), "rolltop") {
		t.Fatalf("startup body did not contain rolltop branding")
	}
}

func TestStartupGateKeepsAPIUnavailableUntilReady(t *testing.T) {
	gate := &startupGate{state: newStartupState()}
	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	res := httptest.NewRecorder()

	gate.ServeHTTP(res, req)

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func TestStartupHTMLShowsFailureMessage(t *testing.T) {
	state := newStartupState()
	state.fail(errors.New("ROLLTOP_MASTER_KEY is required"))
	rec := httptest.NewRecorder()

	writeStartupHTML(rec, state.snapshotCopy())

	body := rec.Body.String()
	if !strings.Contains(body, "Startup failed") {
		t.Fatalf("startup body did not contain failure phase")
	}
	if !strings.Contains(body, "ROLLTOP_MASTER_KEY is required") {
		t.Fatalf("startup body did not contain startup error")
	}
}

func TestAppRuntimeCloseStopsPluginHost(t *testing.T) {
	closer := &runtimeTestCloser{}
	app := &appRuntime{pluginHost: closer}

	app.close()

	if closer.calls != 1 {
		t.Fatalf("plugin host Close calls = %d, want 1", closer.calls)
	}
}

func TestSearchWriterRestartShutdownReturnsWhenPluginCloseBlocks(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	app := &appRuntime{pluginHost: &blockingRuntimeTestCloser{started: started, release: release}}

	start := time.Now()
	err := runSearchWriterRestartShutdown(25*time.Millisecond, func() error {
		defer close(finished)
		app.close()
		return nil
	})
	if !errors.Is(err, errSearchWriterRestartShutdownTimeout) {
		t.Fatalf("restart shutdown error = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("restart shutdown returned after %s, want bounded wait", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("plugin Close did not start")
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("timed-out cleanup did not finish after plugin Close was released")
	}
}

func TestSearchWriterRestartShutdownReturnsCleanupError(t *testing.T) {
	want := errors.New("cleanup failed")
	err := runSearchWriterRestartShutdown(time.Second, func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("restart shutdown error = %v, want cleanup error", err)
	}
}

type runtimeTestCloser struct {
	calls int
}

func (c *runtimeTestCloser) Close() error {
	c.calls++
	return nil
}

type blockingRuntimeTestCloser struct {
	started chan struct{}
	release chan struct{}
}

func (c *blockingRuntimeTestCloser) Close() error {
	close(c.started)
	<-c.release
	return nil
}

func TestInboxAutoTargetsIncludesEveryAccountInbox(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "idle@example.test", "Idle", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.CreateMailAccount(ctx, store.MailAccount{UserID: user.ID, Email: "first@example.test", Host: "imap.first.test", Port: 993, Username: "first", EncryptedPassword: "secret", UseTLS: true, Mailbox: "INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.CreateMailAccount(ctx, store.MailAccount{UserID: user.ID, Email: "second@example.test", Host: "imap.second.test", Port: 993, Username: "second", EncryptedPassword: "secret", UseTLS: true, Mailbox: "INBOX"})
	if err != nil {
		t.Fatal(err)
	}

	targets, err := inboxAutoTargets(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, target := range targets {
		if target.UserID == user.ID && target.Mailbox.Name == "INBOX" {
			seen[target.Account.ID] = true
		}
	}
	if !seen[first.ID] || !seen[second.ID] || len(seen) != 2 {
		t.Fatalf("targets = %+v, want both account inboxes", targets)
	}
}

// A platform routes traffic on this path and takes the previous instance down
// once it answers 200. Answering before the process can serve is what turns a
// redeploy into downtime, so 200 has to mean ready and nothing less.
func TestHealthCheckIsUnavailableUntilServing(t *testing.T) {
	state := newStartupState()
	gate := &startupGate{state: state}

	probe := func() int {
		res := httptest.NewRecorder()
		gate.ServeHTTP(res, httptest.NewRequest(http.MethodGet, healthCheckPath, nil))
		return res.Code
	}

	if code := probe(); code != http.StatusServiceUnavailable {
		t.Fatalf("status while starting = %d, want %d", code, http.StatusServiceUnavailable)
	}
	// Migrations reporting progress are still not readiness.
	state.update("Database", "running migrations", 3, 9)
	if code := probe(); code != http.StatusServiceUnavailable {
		t.Fatalf("status mid-startup = %d, want %d", code, http.StatusServiceUnavailable)
	}
	// Nor is a handler that exists before startup declared itself finished.
	gate.setHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	if code := probe(); code != http.StatusServiceUnavailable {
		t.Fatalf("status with a handler but no readiness = %d, want %d", code, http.StatusServiceUnavailable)
	}

	state.ready()
	if code := probe(); code != http.StatusOK {
		t.Fatalf("status once ready = %d, want %d", code, http.StatusOK)
	}
}

// The gate keeps the path after startup: delegating it would hand the probe to
// the application router, which knows nothing about it and would answer 404.
func TestHealthCheckIsNotDelegatedToTheApplication(t *testing.T) {
	state := newStartupState()
	state.ready()
	delegated := false
	gate := &startupGate{state: state}
	gate.setHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delegated = true
		w.WriteHeader(http.StatusNotFound)
	}))

	res := httptest.NewRecorder()
	gate.ServeHTTP(res, httptest.NewRequest(http.MethodGet, healthCheckPath, nil))

	if delegated {
		t.Fatal("health check reached the application handler")
	}
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
}

// A startup that failed must never read as ready.
func TestHealthCheckStaysUnavailableAfterAFailedStartup(t *testing.T) {
	state := newStartupState()
	state.fail(errors.New("ROLLTOP_MASTER_KEY is required"))
	gate := &startupGate{state: state}

	res := httptest.NewRecorder()
	gate.ServeHTTP(res, httptest.NewRequest(http.MethodGet, healthCheckPath, nil))

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

// The budgets used to be constants sized for a ten-second grace period. An
// operator who gives the container more room, or a platform that gives it less,
// now moves all of them at once - and the default still has to be what the
// constants were.
func TestShutdownBudgetSplitsTheStopGracePeriod(t *testing.T) {
	budget := newShutdownBudget(10 * time.Second)
	if budget.httpDrain != 3*time.Second {
		t.Fatalf("http drain budget = %s, want 3s", budget.httpDrain)
	}
	if budget.interruptedRuns != 2*time.Second {
		t.Fatalf("interrupted sync run budget = %s, want 2s", budget.interruptedRuns)
	}
	if budget.derivedClose != 3*time.Second {
		t.Fatalf("derived close budget = %s, want 3s", budget.derivedClose)
	}
	// The remainder is what the database close runs in, and it may not be
	// spent by the phases before it.
	if spent := budget.httpDrain + budget.interruptedRuns + budget.derivedClose; spent >= budget.total {
		t.Fatalf("phases claim %s of a %s budget, leaving nothing for the database close", spent, budget.total)
	}

	longer := newShutdownBudget(60 * time.Second)
	if longer.httpDrain != 18*time.Second || longer.derivedClose != 18*time.Second {
		t.Fatalf("a 60s budget produced %+v, want every phase scaled with it", longer)
	}

	// The paths that shut down before the configuration is read pass zero.
	if unset := newShutdownBudget(0); unset != newShutdownBudget(defaultShutdownTimeout) {
		t.Fatalf("unconfigured budget = %+v, want the default budget", unset)
	}
}

// A close that is not part of a signalled shutdown still has to be bounded, so
// a runtime without a tracker falls back to the default share rather than to no
// deadline at all.
func TestAppRuntimeCloseBudgetFollowsTheTracker(t *testing.T) {
	app := &appRuntime{}
	if got, want := app.derivedCloseBudget(), newShutdownBudget(0).derivedClose; got != want {
		t.Fatalf("untracked close budget = %s, want %s", got, want)
	}

	app.shutdown = newShutdownTracker(nil, newShutdownBudget(60*time.Second))
	if got, want := app.derivedCloseBudget(), 18*time.Second; got != want {
		t.Fatalf("tracked close budget = %s, want %s", got, want)
	}
}

func TestSignalNameSpellsThePlatformsWord(t *testing.T) {
	cases := map[os.Signal]string{
		syscall.SIGTERM: "SIGTERM",
		os.Interrupt:    "SIGINT",
		nil:             "no signal",
	}
	for sig, want := range cases {
		if got := signalName(sig); got != want {
			t.Fatalf("signalName(%v) = %q, want %q", sig, got, want)
		}
	}
}

// The signal is what separates "the platform stopped us" from "the kernel killed
// us", so the watcher has to hold on to it rather than only cancelling.
func TestNotifyStopSignalKeepsTheSignalThatStoppedTheProcess(t *testing.T) {
	ctx, stopSignal, stop := notifyStopSignal(context.Background(), syscall.SIGUSR1)
	defer stop()

	if got := stopSignal(); got != nil {
		t.Fatalf("signal reported before one arrived: %v", got)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled by the signal")
	}
	if got := stopSignal(); got != syscall.SIGUSR1 {
		t.Fatalf("recorded signal = %v, want SIGUSR1", got)
	}
}

// A stop signal during startup is the platform doing its job, not a failed
// start. It must not be filed as a fatal error, and a genuine failure that
// happens to coincide with one still must be.
func TestStoppedDuringStartupOnlyCoversTheStopSignal(t *testing.T) {
	stopped, cancel := context.WithCancel(context.Background())
	cancel()

	if !stoppedDuringStartup(stopped, fmt.Errorf("open search index: %w", context.Canceled)) {
		t.Fatal("a cancelled startup after a stop signal was reported as a failure")
	}
	if stoppedDuringStartup(stopped, errors.New("ROLLTOP_MASTER_KEY is required")) {
		t.Fatal("a real startup failure was hidden because a stop signal had arrived")
	}
	if stoppedDuringStartup(context.Background(), context.Canceled) {
		t.Fatal("a cancellation with no stop signal was reported as an orderly stop")
	}
}
