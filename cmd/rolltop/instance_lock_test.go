// File overview: Data directory ownership during overlapping starts.

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A rolling deployment starts the replacement before stopping the process it
// replaces. The new process has to wait for the directory rather than refuse it,
// or every rollout is a failed start.
func TestWaitForInstanceLockAcquiresAfterThePreviousProcessExits(t *testing.T) {
	dataDir := t.TempDir()
	previous, err := acquireInstanceLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := previous.Close(); err != nil {
			t.Errorf("release previous instance lock: %v", err)
		}
		close(released)
	}()

	waits := 0
	started := time.Now()
	lock, waited, err := waitForInstanceLock(context.Background(), dataDir, 10*time.Second,
		func(time.Duration, string) { waits++ })
	if err != nil {
		t.Fatalf("waitForInstanceLock() error = %v", err)
	}
	defer lock.Close()
	<-released
	if waits == 0 {
		t.Fatal("the wait was never reported")
	}
	if waited <= 0 || waited > time.Since(started) {
		t.Fatalf("reported wait = %s after %s", waited, time.Since(started))
	}
	// The lock file names the process that owns the directory now.
	pid, err := os.ReadFile(filepath.Join(dataDir, ".rolltop-instance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(pid)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file names pid %q, want %d", strings.TrimSpace(string(pid)), os.Getpid())
	}
}

func TestWaitForInstanceLockReportsAnUnreleasedDirectory(t *testing.T) {
	dataDir := t.TempDir()
	previous, err := acquireInstanceLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()

	lock, waited, err := waitForInstanceLock(context.Background(), dataDir, 400*time.Millisecond, nil)
	if lock != nil {
		_ = lock.Close()
		t.Fatal("instance lock was acquired while another process held it")
	}
	if !errors.Is(err, errRolltopAlreadyRunning) {
		t.Fatalf("wait error = %v, want %v", err, errRolltopAlreadyRunning)
	}
	// The message has to say who holds the directory and how long this start
	// gave them, because that is the whole diagnosis of a failed rollout.
	if !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) || !strings.Contains(err.Error(), "waited") {
		t.Fatalf("wait error = %v, want the holder and the elapsed wait", err)
	}
	if waited < 400*time.Millisecond {
		t.Fatalf("reported wait = %s, want at least the configured wait", waited)
	}
}

// A zero wait keeps the old behavior for operators who would rather see a start
// fail immediately than have two containers overlap at all.
func TestWaitForInstanceLockWithoutWaitFailsImmediately(t *testing.T) {
	dataDir := t.TempDir()
	previous, err := acquireInstanceLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()

	started := time.Now()
	lock, _, err := waitForInstanceLock(context.Background(), dataDir, 0, func(time.Duration, string) {
		t.Error("a zero wait reported a wait")
	})
	if lock != nil {
		_ = lock.Close()
		t.Fatal("instance lock was acquired while another process held it")
	}
	if !errors.Is(err, errRolltopAlreadyRunning) {
		t.Fatalf("wait error = %v, want %v", err, errRolltopAlreadyRunning)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("zero wait took %s", elapsed)
	}
}

// A container stopped while it waits has to stop, which is why the wait polls
// instead of blocking inside flock.
func TestWaitForInstanceLockStopsWhenTheProcessIsSignalled(t *testing.T) {
	dataDir := t.TempDir()
	previous, err := acquireInstanceLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	lock, _, err := waitForInstanceLock(ctx, dataDir, time.Hour, nil)
	if lock != nil {
		_ = lock.Close()
		t.Fatal("instance lock was acquired while another process held it")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want a cancelled wait", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("cancelled wait took %s", elapsed)
	}
}

// The offline maintenance commands keep refusing outright: a person running a
// repair has to be told to stop the server, not left waiting for it.
func TestAcquireInstanceLockStillRefusesWhileTheServerRuns(t *testing.T) {
	dataDir := t.TempDir()
	server, err := acquireInstanceLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	started := time.Now()
	maintenance, err := acquireInstanceLock(dataDir)
	if maintenance != nil {
		_ = maintenance.Close()
		t.Fatal("offline maintenance acquired a directory the server owns")
	}
	if !errors.Is(err, errRolltopAlreadyRunning) || !strings.Contains(err.Error(), "stop the server") {
		t.Fatalf("maintenance lock error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("maintenance lock waited %s", elapsed)
	}
}
