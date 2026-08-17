// File overview: Durable scheduling of per-user database repairs. A repair
// replaces the SQLite file, which cannot be done safely while the server holds
// handles on it, so the admin UI does not repair directly: it writes a marker
// here and asks the process to restart. The next start consumes the marker
// before any user store is opened, which is the same offline condition
// "rolltop recover-db" requires. The outcome is written back beside the marker
// so the UI can show what a restart actually recovered.

package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	repairMarkerSuffix = ".repair-requested"
	repairReportSuffix = ".repair-report.json"
)

// RepairRequest is the durable record the admin UI writes.
type RepairRequest struct {
	UserID      int64     `json:"user_id"`
	RequestedAt time.Time `json:"requested_at"`
	RequestedBy string    `json:"requested_by"`
}

// RepairOutcome is what the startup repair recorded for the UI to read back.
type RepairOutcome struct {
	UserID         int64         `json:"user_id"`
	StartedAt      time.Time     `json:"started_at"`
	FinishedAt     time.Time     `json:"finished_at"`
	Succeeded      bool          `json:"succeeded"`
	Error          string        `json:"error,omitempty"`
	QuarantinePath string        `json:"quarantine_path,omitempty"`
	Report         SalvageReport `json:"report"`
}

// UserDatabaseFilePath is the tenant SQLite file under a data directory. It is
// a package function because maintenance code runs before any Store exists.
func UserDatabaseFilePath(dataDir string, userID int64) string {
	return filepath.Join(dataDir, "users", fmt.Sprintf("%d", userID), databaseFilename)
}

func repairMarkerPath(dataDir string, userID int64) string {
	return UserDatabaseFilePath(dataDir, userID) + repairMarkerSuffix
}

func repairReportPath(dataDir string, userID int64) string {
	return UserDatabaseFilePath(dataDir, userID) + repairReportSuffix
}

// ScheduleUserDatabaseRepair durably records that this tenant's database must
// be rebuilt on the next start. Writing the marker is the only thing the
// running server does to a database it believes is damaged.
func ScheduleUserDatabaseRepair(dataDir string, userID int64, requestedBy string, now time.Time) error {
	if userID <= 0 {
		return fmt.Errorf("user id must be positive")
	}
	path := UserDatabaseFilePath(dataDir, userID)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("open user %d database %s: %w", userID, path, err)
	}
	return writeDurableJSON(repairMarkerPath(dataDir, userID), RepairRequest{
		UserID:      userID,
		RequestedAt: now.UTC(),
		RequestedBy: requestedBy,
	})
}

// UserDatabaseRepairRequest reports a pending repair for one tenant.
func UserDatabaseRepairRequest(dataDir string, userID int64) (RepairRequest, bool, error) {
	var request RepairRequest
	found, err := readJSONFile(repairMarkerPath(dataDir, userID), &request)
	return request, found, err
}

// ClearUserDatabaseRepair removes a pending repair, either because it ran or
// because an admin cancelled it.
func ClearUserDatabaseRepair(dataDir string, userID int64) error {
	if err := os.Remove(repairMarkerPath(dataDir, userID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// WriteUserDatabaseRepairReport persists what a repair recovered.
func WriteUserDatabaseRepairReport(dataDir string, userID int64, outcome RepairOutcome) error {
	return writeDurableJSON(repairReportPath(dataDir, userID), outcome)
}

// UserDatabaseRepairReport returns the most recent repair outcome, if any.
func UserDatabaseRepairReport(dataDir string, userID int64) (RepairOutcome, bool, error) {
	var outcome RepairOutcome
	found, err := readJSONFile(repairReportPath(dataDir, userID), &outcome)
	return outcome, found, err
}

// writeDurableJSON publishes a small JSON file atomically and fsyncs both the
// file and its directory, so a crash right after the request cannot lose it.
func writeDurableJSON(path string, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		_ = temporary.Close()
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(append(payload, '\n')); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	keepTemporary = false
	return syncDirectory(dir)
}

func readJSONFile(path string, value any) (bool, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return true, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
