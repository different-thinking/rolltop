// File overview: Database maintenance the admin UI can drive without a shell.
// Two of the three operations are safe on a live server and run here as
// background jobs: the integrity check only reads, and backups use VACUUM INTO
// from a single read transaction. The third, repairing a damaged database,
// replaces the file and therefore cannot run while the process holds handles on
// it; scheduling it writes a durable marker and asks the process to restart, so
// the repair happens at startup before any tenant database is opened.

package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"rolltop/backend/store"
)

const (
	maintenanceJobLogLines = 200
	backupDirectoryName    = "backups"
	// A backup under this suffix is still being written and is not a backup yet.
	stagingBackupSuffix = ".incomplete"
)

// errMaintenanceJobRunning reports the single job slot is taken.
var errMaintenanceJobRunning = errors.New("another maintenance job is already running")

// maintenanceJobKind names the long-running operations the UI can start.
type maintenanceJobKind string

const (
	maintenanceJobCheck  maintenanceJobKind = "check"
	maintenanceJobBackup maintenanceJobKind = "backup"
	maintenanceJobRepair maintenanceJobKind = "repair"
)

// maintenanceJob is the observable state of one running or finished job. Only
// one job runs at a time: both kinds read every page of every database, and
// running them concurrently would only slow the live server down further.
type maintenanceJob struct {
	ID         int64              `json:"id"`
	Kind       maintenanceJobKind `json:"kind"`
	UserID     int64              `json:"user_id"`
	Running    bool               `json:"running"`
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt time.Time          `json:"finished_at,omitempty"`
	Detail     string             `json:"detail"`
	Log        []string           `json:"log"`
	Error      string             `json:"error,omitempty"`
	Problems   int                `json:"problems"`
}

type maintenanceState struct {
	mu      sync.Mutex
	nextID  int64
	current *maintenanceJob
}

func (m *maintenanceState) snapshot() *maintenanceJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return nil
	}
	copied := *m.current
	// The frontend types this as string[] and reads .length, so an empty log
	// must marshal as [] rather than null.
	copied.Log = append(make([]string, 0, len(m.current.Log)), m.current.Log...)
	return &copied
}

// start reserves the single job slot. It reports false when another job is
// still running.
func (m *maintenanceState) start(kind maintenanceJobKind, userID int64, now time.Time) (*maintenanceJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.Running {
		return nil, false
	}
	m.nextID++
	m.current = &maintenanceJob{
		ID:        m.nextID,
		Kind:      kind,
		UserID:    userID,
		Running:   true,
		StartedAt: now.UTC(),
		Detail:    "starting",
		Log:       []string{},
	}
	copied := *m.current
	return &copied, true
}

// reserveExclusive takes the job slot for work that has no job of its own, such
// as the restart a scheduled repair triggers. It reports the running kind when
// the slot is already taken.
func (m *maintenanceState) reserveExclusive(now time.Time) (maintenanceJobKind, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.Running {
		return m.current.Kind, false
	}
	m.nextID++
	m.current = &maintenanceJob{
		ID:        m.nextID,
		Kind:      maintenanceJobRepair,
		Running:   true,
		StartedAt: now.UTC(),
		Detail:    "scheduling repair",
		Log:       []string{},
	}
	return maintenanceJobRepair, true
}

// releaseExclusive gives the slot back when the reserved work did not start.
func (m *maintenanceState) releaseExclusive() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.Kind == maintenanceJobRepair {
		m.current.Running = false
		m.current.FinishedAt = time.Now().UTC()
		m.current.Detail = "repair could not be scheduled"
	}
}

func (m *maintenanceState) appendLog(id int64, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || m.current.ID != id {
		return
	}
	m.current.Detail = detail
	m.current.Log = append(m.current.Log, detail)
	if len(m.current.Log) > maintenanceJobLogLines {
		m.current.Log = m.current.Log[len(m.current.Log)-maintenanceJobLogLines:]
	}
}

func (m *maintenanceState) finish(id int64, detail string, problems int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || m.current.ID != id {
		return
	}
	m.current.Running = false
	m.current.FinishedAt = time.Now().UTC()
	m.current.Detail = detail
	m.current.Problems = problems
	if err != nil {
		m.current.Error = err.Error()
	}
}

// databaseStatus is one SQLite file as the admin UI shows it.
type databaseStatus struct {
	Scope             string               `json:"scope"`
	UserID            int64                `json:"user_id"`
	Email             string               `json:"email,omitempty"`
	Path              string               `json:"path"`
	Bytes             int64                `json:"bytes"`
	WALBytes          int64                `json:"wal_bytes"`
	Missing           bool                 `json:"missing"`
	Corrupt           bool                 `json:"corrupt"`
	CorruptDetail     string               `json:"corrupt_detail,omitempty"`
	CorruptDetected   time.Time            `json:"corrupt_detected_at,omitempty"`
	RepairScheduled   bool                 `json:"repair_scheduled"`
	RepairRequestedAt time.Time            `json:"repair_requested_at,omitempty"`
	LastRepair        *store.RepairOutcome `json:"last_repair,omitempty"`
}

// backupEntry is one previously written backup directory.
type backupEntry struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// databaseOverview is the whole admin page payload.
type databaseOverview struct {
	DataDir          string           `json:"data_dir"`
	FreeBytes        int64            `json:"free_bytes"`
	TotalBytes       int64            `json:"total_bytes"`
	Databases        []databaseStatus `json:"databases"`
	Backups          []backupEntry    `json:"backups"`
	BackupDir        string           `json:"backup_dir"`
	Job              *maintenanceJob  `json:"job,omitempty"`
	RestartSupported bool             `json:"restart_supported"`
}

func (s *Server) backupDirectory() string {
	return filepath.Join(s.dataDir, backupDirectoryName)
}

// databaseOverview assembles everything the maintenance page shows. Sizes come
// from the filesystem so a database too damaged to open still appears.
func (s *Server) databaseOverview(ctx context.Context) (databaseOverview, error) {
	overview := databaseOverview{
		DataDir:          s.dataDir,
		BackupDir:        s.backupDirectory(),
		Job:              s.maintenance.snapshot(),
		RestartSupported: s.requestRestart != nil,
	}
	overview.FreeBytes, overview.TotalBytes = filesystemCapacityBytes(s.dataDir)

	systemStatus := databaseStatus{Scope: "system", Path: s.databasePath}
	systemStatus.Bytes, systemStatus.WALBytes, systemStatus.Missing = sqliteFileSizes(s.databasePath)
	overview.Databases = append(overview.Databases, systemStatus)

	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return overview, err
	}
	corrupt := make(map[int64]store.DatabaseHealth)
	for _, record := range s.store.CorruptDatabases() {
		corrupt[record.UserID] = record
	}
	for _, user := range users {
		status := databaseStatus{
			Scope:  "user",
			UserID: user.ID,
			Email:  user.Email,
			Path:   store.UserDatabaseFilePath(s.dataDir, user.ID),
		}
		status.Bytes, status.WALBytes, status.Missing = sqliteFileSizes(status.Path)
		if record, damaged := corrupt[user.ID]; damaged {
			status.Corrupt = true
			status.CorruptDetail = record.Detail
			status.CorruptDetected = record.DetectedAt
		}
		if request, found, err := store.UserDatabaseRepairRequest(s.dataDir, user.ID); err == nil && found {
			status.RepairScheduled = true
			status.RepairRequestedAt = request.RequestedAt
		}
		if outcome, found, err := store.UserDatabaseRepairReport(s.dataDir, user.ID); err == nil && found {
			recorded := outcome
			status.LastRepair = &recorded
		}
		overview.Databases = append(overview.Databases, status)
	}
	overview.Backups = s.listBackups()
	return overview, nil
}

// backupSize is one measured backup directory. Sizing walks every file in the
// directory, and the admin page polls, so a finished backup is measured once
// and re-measured only if its directory changes.
type backupSize struct {
	modTime time.Time
	bytes   int64
}

func (s *Server) backupDirectorySize(path string, name string, modTime time.Time) (int64, error) {
	s.backupSizeMu.Lock()
	cached, ok := s.backupSizes[name]
	s.backupSizeMu.Unlock()
	if ok && cached.modTime.Equal(modTime) {
		return cached.bytes, nil
	}
	size, err := pathSize(path)
	if err != nil {
		return 0, err
	}
	s.backupSizeMu.Lock()
	if s.backupSizes == nil {
		s.backupSizes = make(map[string]backupSize)
	}
	s.backupSizes[name] = backupSize{modTime: modTime, bytes: size}
	s.backupSizeMu.Unlock()
	return size, nil
}

func (s *Server) listBackups() []backupEntry {
	root := s.backupDirectory()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	backups := make([]backupEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasSuffix(entry.Name(), stagingBackupSuffix) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		created := time.Time{}
		if info, err := entry.Info(); err == nil {
			created = info.ModTime().UTC()
		}
		size, err := s.backupDirectorySize(path, entry.Name(), created)
		if err != nil {
			continue
		}
		backups = append(backups, backupEntry{Name: entry.Name(), Path: path, Bytes: size, CreatedAt: created})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].Name > backups[j].Name })
	return backups
}

// startIntegrityCheck verifies every database, or one tenant, in the
// background. quick_check only reads, so this is safe while the server serves.
func (s *Server) startIntegrityCheck(scope maintenanceScope, userID int64) (*maintenanceJob, error) {
	// Targets are resolved before the slot is taken so an unknown or missing
	// database answers the request instead of failing inside a job the caller
	// has already been told started.
	targets, err := s.maintenanceTargets(context.Background(), scope, userID)
	if err != nil {
		return nil, err
	}
	job, ok := s.maintenance.start(maintenanceJobCheck, userID, time.Now())
	if !ok {
		return nil, errMaintenanceJobRunning
	}
	go func() {
		ctx := context.Background()
		problems := 0
		for _, target := range targets {
			s.maintenance.appendLog(job.ID, fmt.Sprintf("checking %s", target.label))
			found, err := s.store.CheckDatabase(ctx, target.userID, target.path)
			if err != nil {
				s.maintenance.appendLog(job.ID, fmt.Sprintf("%s: check failed: %v", target.label, err))
				problems++
				continue
			}
			if len(found) == 0 {
				// A full scan that finds nothing overrules a latch set from a
				// single failing statement, so a tenant can come back into
				// service without restarting the process.
				if target.userID > 0 {
					s.store.ClearCorruption(target.userID)
				}
				s.maintenance.appendLog(job.ID, fmt.Sprintf("%s: ok", target.label))
				continue
			}
			problems += len(found)
			// Latching here is what makes the damage visible on the page and
			// stops background work from retrying the tenant.
			if target.userID > 0 {
				s.maintenance.appendLog(job.ID, fmt.Sprintf("%s: %v", target.label, s.store.MarkCorrupt(target.userID, found[0])))
			} else {
				s.maintenance.appendLog(job.ID, fmt.Sprintf("%s: %s", target.label, found[0]))
			}
			for _, problem := range found[1:min(len(found), 10)] {
				s.maintenance.appendLog(job.ID, fmt.Sprintf("%s: %s", target.label, problem))
			}
		}
		s.maintenance.finish(job.ID, fmt.Sprintf("checked %d database(s)", len(targets)), problems, nil)
	}()
	return job, nil
}

// startBackup writes a consistent copy of every database into a timestamped
// directory under the data volume.
func (s *Server) startBackup(scope maintenanceScope, userID int64) (*maintenanceJob, error) {
	targets, err := s.maintenanceTargets(context.Background(), scope, userID)
	if err != nil {
		return nil, err
	}
	job, ok := s.maintenance.start(maintenanceJobBackup, userID, time.Now())
	if !ok {
		return nil, errMaintenanceJobRunning
	}
	destination := filepath.Join(s.backupDirectory(), time.Now().UTC().Format("20060102T150405Z"))
	// Copies are written to a sibling that listBackups ignores and published by
	// rename once every target succeeded. A process that exits mid-VACUUM then
	// leaves a staging directory, not something that looks like a backup.
	staging := destination + stagingBackupSuffix
	go func() {
		ctx := context.Background()
		if err := os.MkdirAll(staging, 0o700); err != nil {
			s.maintenance.finish(job.ID, "failed", 0, err)
			return
		}
		failures := 0
		var total int64
		for _, target := range targets {
			dest := filepath.Join(staging, "rolltop.db")
			if target.userID > 0 {
				dest = filepath.Join(staging, "users", fmt.Sprintf("%d", target.userID), "rolltop.db")
			}
			size, err := s.store.BackupDatabase(ctx, target.userID, target.path, dest)
			if err != nil {
				failures++
				s.maintenance.appendLog(job.ID, fmt.Sprintf("%s: failed: %v", target.label, err))
				continue
			}
			total += size
			s.maintenance.appendLog(job.ID, fmt.Sprintf("%s: %d bytes", target.label, size))
		}
		if failures > 0 {
			// An incomplete set is never published; the staging directory is
			// removed so nothing suggests a usable backup exists.
			_ = os.RemoveAll(staging)
			s.maintenance.finish(job.ID, "finished with failures", failures,
				fmt.Errorf("%d of %d database(s) could not be backed up", failures, len(targets)))
			return
		}
		if err := os.Rename(staging, destination); err != nil {
			_ = os.RemoveAll(staging)
			s.maintenance.finish(job.ID, "failed", 0, fmt.Errorf("publish backup %s: %w", destination, err))
			return
		}
		s.maintenance.appendLog(job.ID, fmt.Sprintf("written to %s", destination))
		s.maintenance.finish(job.ID, fmt.Sprintf("backed up %d database(s), %d bytes", len(targets), total), 0, nil)
	}()
	return job, nil
}

type maintenanceTarget struct {
	label  string
	path   string
	userID int64
}

// maintenanceScope selects which files a job covers. Without an explicit scope
// the installation-database row could only be checked by scanning every tenant
// as well, which on a large mirror is a completely different operation.
type maintenanceScope string

const (
	maintenanceScopeAll    maintenanceScope = ""
	maintenanceScopeSystem maintenanceScope = "system"
	maintenanceScopeUser   maintenanceScope = "user"
)

// maintenanceTargets resolves which files a job covers. Passing a user ID
// limits the job to that tenant; zero covers the installation database and
// every tenant.
func (s *Server) maintenanceTargets(ctx context.Context, scope maintenanceScope, userID int64) ([]maintenanceTarget, error) {
	if scope == maintenanceScopeUser || (scope == maintenanceScopeAll && userID > 0) {
		user, err := s.store.GetUserByID(ctx, userID)
		if err != nil {
			return nil, err
		}
		path := store.UserDatabaseFilePath(s.dataDir, user.ID)
		// Checking or backing up a file that is not there would otherwise let
		// SQLite create an empty one at the live path and report it as intact.
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("user %d database %s: %w", user.ID, path, store.ErrDatabaseFileMissing)
		}
		return []maintenanceTarget{{
			label:  fmt.Sprintf("user %d database (%s)", user.ID, user.Email),
			path:   path,
			userID: user.ID,
		}}, nil
	}
	targets := []maintenanceTarget{{label: "installation database", path: s.databasePath}}
	if scope == maintenanceScopeSystem {
		return targets, nil
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		path := store.UserDatabaseFilePath(s.dataDir, user.ID)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		targets = append(targets, maintenanceTarget{
			label:  fmt.Sprintf("user %d database (%s)", user.ID, user.Email),
			path:   path,
			userID: user.ID,
		})
	}
	return targets, nil
}

// scheduleDatabaseRepair records the repair and asks the process to restart so
// it can run with no handles open. The caller must have confirmed the data loss
// this can involve.
func (s *Server) scheduleDatabaseRepair(ctx context.Context, userID int64, requestedBy string) error {
	if userID <= 0 {
		return fmt.Errorf("user id must be positive")
	}
	if _, err := s.store.GetUserByID(ctx, userID); err != nil {
		return err
	}
	// The restart below kills whatever a running job is in the middle of, and a
	// VACUUM INTO cut short leaves a partial file that later looks like a
	// complete backup. Reserving the slot under one lock closes the window
	// between checking and restarting.
	kind, reserved := s.maintenance.reserveExclusive(time.Now())
	if !reserved {
		return fmt.Errorf("%w: wait for the running %s to finish", errMaintenanceJobRunning, kind)
	}
	if err := store.ScheduleUserDatabaseRepair(s.dataDir, userID, requestedBy, time.Now()); err != nil {
		s.maintenance.releaseExclusive()
		return err
	}
	if s.requestRestart == nil {
		// Nothing will restart this process, so the repair waits for the next
		// start and the slot must not stay reserved until then.
		s.maintenance.releaseExclusive()
		return nil
	}
	s.requestRestart(userID, fmt.Sprintf("database repair requested for user %d", userID))
	return nil
}

func sqliteFileSizes(path string) (size, walSize int64, missing bool) {
	if strings.TrimSpace(path) == "" {
		return 0, 0, true
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, true
	}
	size = info.Size()
	if walInfo, err := os.Stat(path + "-wal"); err == nil {
		walSize = walInfo.Size()
	}
	return size, walSize, false
}

// filesystemCapacityBytes reports free and total bytes on the data volume. A
// volume close to full is one of the few storage conditions that can actually
// damage a SQLite file, so the page shows it next to the databases.
func filesystemCapacityBytes(path string) (free, total int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	return int64(stat.Bavail) * int64(stat.Bsize), int64(stat.Blocks) * int64(stat.Bsize)
}
