package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var errRolltopAlreadyRunning = errors.New("another rolltop process is using this data directory")

// instanceLockRetryInterval paces the wait for a previous process. Polling
// rather than blocking in flock is deliberate: a blocking lock cannot be
// cancelled, so a container told to stop while it waits would ignore the signal,
// and it could report neither how long it has waited nor who holds the lock.
const instanceLockRetryInterval = 250 * time.Millisecond

// instanceLockReportInterval keeps a wait visible without filling the log with
// one line per retry.
const instanceLockReportInterval = 5 * time.Second

type instanceLock struct {
	file *os.File
}

// acquireInstanceLock prevents online maintenance from racing the HTTP server.
// flock is associated with the open file description, so it also works when
// the data directory is a Docker volume shared by separate containers.
//
// This is the fail-fast form, for the offline maintenance commands: a person
// running a repair needs to be told to stop the server, not left waiting.
func acquireInstanceLock(dataDir string) (*instanceLock, error) {
	lock, held, err := tryInstanceLock(dataDir)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, fmt.Errorf("%w%s; stop the server before running offline maintenance",
			errRolltopAlreadyRunning, describeInstanceLockHolder(dataDir))
	}
	return lock, nil
}

// waitForInstanceLock acquires the data directory for a serving process, giving
// a previous one time to let go. Rolling deployments start the new container
// before stopping the old one, so at that moment two processes want the same
// SQLite files and Bleve indexes; the lock is what keeps the second one out.
// Refusing to start is the wrong answer to a lock that is about to be released
// anyway: the same deployment that started this process is what stops the one
// holding it. That process needs its HTTP drain, its plugin and index close, and
// its SQLite checkpoint, which is seconds rather than minutes.
//
// onWait is called when the wait begins and every few seconds after that, so a
// startup page and the container log can say what is being waited for instead of
// looking stuck. The returned duration is how long the lock was held by someone
// else, which is zero on the ordinary start where nothing overlapped.
func waitForInstanceLock(ctx context.Context, dataDir string, wait time.Duration,
	onWait func(waited time.Duration, holder string),
) (*instanceLock, time.Duration, error) {
	lock, held, err := tryInstanceLock(dataDir)
	if err != nil {
		return nil, 0, err
	}
	if held {
		return lock, 0, nil
	}
	holder := describeInstanceLockHolder(dataDir)
	if wait <= 0 {
		return nil, 0, fmt.Errorf("%w%s", errRolltopAlreadyRunning, holder)
	}
	started := time.Now()
	deadline := started.Add(wait)
	report(onWait, 0, holder)
	nextReport := instanceLockReportInterval
	ticker := time.NewTicker(instanceLockRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, time.Since(started), ctx.Err()
		case <-ticker.C:
		}
		lock, held, err := tryInstanceLock(dataDir)
		waited := time.Since(started)
		if err != nil {
			return nil, waited, err
		}
		if held {
			return lock, waited, nil
		}
		if !time.Now().Before(deadline) {
			return nil, waited, fmt.Errorf("%w%s; waited %s for it to exit",
				errRolltopAlreadyRunning, describeInstanceLockHolder(dataDir), waited.Round(time.Second))
		}
		if waited >= nextReport {
			report(onWait, waited, holder)
			nextReport = waited + instanceLockReportInterval
		}
	}
}

func report(onWait func(time.Duration, string), waited time.Duration, holder string) {
	if onWait != nil {
		onWait(waited, holder)
	}
}

// tryInstanceLock reports held=false when another process owns the directory,
// which is a state to act on rather than an error to report.
func tryInstanceLock(dataDir string) (*instanceLock, bool, error) {
	path, err := instanceLockPath(dataDir)
	if err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open instance lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lock rolltop data directory %s: %w", dataDir, err)
	}
	if err := file.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
		_ = file.Sync()
	}
	return &instanceLock{file: file}, true, nil
}

func instanceLockPath(dataDir string) (string, error) {
	dataDir = filepath.Clean(dataDir)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create data directory for instance lock: %w", err)
	}
	return filepath.Join(dataDir, ".rolltop-instance.lock"), nil
}

// describeInstanceLockHolder names the process that wrote the lock file. It is
// diagnostic only: the PID belongs to whichever namespace that process ran in,
// so it identifies the holder in a log rather than in a signal. An empty result
// is normal for a lock file whose owner has not written itself in yet.
func describeInstanceLockHolder(dataDir string) string {
	path, err := instanceLockPath(dataDir)
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(raw))
	if pid == "" {
		return ""
	}
	return fmt.Sprintf(" (held by pid %s)", pid)
}

func (l *instanceLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
