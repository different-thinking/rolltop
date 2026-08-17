package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rolltop/backend/store"
)

func newDatabaseAdminServer(t *testing.T) (*Server, store.User, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	databasePath := filepath.Join(dataDir, "rolltop.db")
	db, err := store.OpenServer(databasePath, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	admin, err := db.CreateUser(ctx, "admin@example.test", "Admin", "hash", true)
	if err != nil {
		t.Fatal(err)
	}
	// Touch the tenant store so its database file exists on disk.
	if _, err := db.UserStore(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	server, err := New(Options{Store: db, DataDir: dataDir, DatabasePath: databasePath, PluginDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return server, admin, dataDir
}

func adminDatabaseRequest(t *testing.T, server *Server, admin store.User, method, target string, body any) *http.Request {
	t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = encoded
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: admin}))
	const csrfBase = "database-admin-csrf"
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	request.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	return request
}

func TestAdminDatabaseOverviewListsInstallationAndTenantFiles(t *testing.T) {
	server, admin, dataDir := newDatabaseAdminServer(t)

	recorder := httptest.NewRecorder()
	server.apiAdminDatabase(recorder, adminDatabaseRequest(t, server, admin, http.MethodGet, "/api/admin/database", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var overview databaseOverview
	if err := json.Unmarshal(recorder.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Databases) != 2 {
		t.Fatalf("databases = %+v, want the installation database and one tenant", overview.Databases)
	}
	if overview.Databases[0].Scope != "system" || overview.Databases[0].Bytes == 0 {
		t.Fatalf("system database entry = %+v", overview.Databases[0])
	}
	tenant := overview.Databases[1]
	if tenant.Scope != "user" || tenant.UserID != admin.ID || tenant.Email != admin.Email {
		t.Fatalf("tenant entry = %+v", tenant)
	}
	if tenant.Path != store.UserDatabaseFilePath(dataDir, admin.ID) {
		t.Fatalf("tenant path = %q", tenant.Path)
	}
	if tenant.Corrupt || tenant.RepairScheduled {
		t.Fatalf("healthy tenant reported as %+v", tenant)
	}
}

func TestAdminDatabaseCheckReportsHealthyFiles(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)

	recorder := httptest.NewRecorder()
	server.apiAdminDatabaseAction(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, "/api/admin/database/check", map[string]any{}), "check")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	job := waitForMaintenanceJob(t, server)
	if job.Kind != maintenanceJobCheck {
		t.Fatalf("job kind = %q", job.Kind)
	}
	if job.Problems != 0 || job.Error != "" {
		t.Fatalf("healthy databases reported problems: %+v", job)
	}
}

func TestAdminDatabaseBackupWritesConsistentCopies(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)

	recorder := httptest.NewRecorder()
	server.apiAdminDatabaseAction(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, "/api/admin/database/backup", map[string]any{}), "backup")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	job := waitForMaintenanceJob(t, server)
	if job.Error != "" {
		t.Fatalf("backup failed: %+v", job)
	}
	backups := server.listBackups()
	if len(backups) != 1 {
		t.Fatalf("backups = %+v, want one", backups)
	}
	copied := filepath.Join(backups[0].Path, "users", "1", "rolltop.db")
	problems, err := store.CheckDatabaseFile(context.Background(), copied)
	if err != nil || len(problems) != 0 {
		t.Fatalf("backup copy %s is not sound: %v %v", copied, problems, err)
	}
}

func TestAdminDatabaseRepairSchedulesRestartAndCanBeCancelled(t *testing.T) {
	server, admin, dataDir := newDatabaseAdminServer(t)
	restarts := make(chan int64, 1)
	server.requestRestart = func(userID int64, _ string) { restarts <- userID }

	// An unconfirmed repair must not schedule anything.
	recorder := httptest.NewRecorder()
	server.apiAdminDatabaseAction(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, "/api/admin/database/repair",
		map[string]any{"user_id": admin.ID}), "repair")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed repair status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if _, found, err := store.UserDatabaseRepairRequest(dataDir, admin.ID); err != nil || found {
		t.Fatalf("unconfirmed repair was scheduled: %v %v", found, err)
	}

	recorder = httptest.NewRecorder()
	server.apiAdminDatabaseAction(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, "/api/admin/database/repair",
		map[string]any{"user_id": admin.ID, "confirm": true}), "repair")
	if recorder.Code != http.StatusOK {
		t.Fatalf("repair status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	request, found, err := store.UserDatabaseRepairRequest(dataDir, admin.ID)
	if err != nil || !found {
		t.Fatalf("repair marker missing: %v %v", found, err)
	}
	if request.RequestedBy != admin.Email {
		t.Fatalf("repair requested by %q", request.RequestedBy)
	}
	select {
	case userID := <-restarts:
		if userID != admin.ID {
			t.Fatalf("restart requested for user %d", userID)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduling a repair did not request a restart")
	}

	recorder = httptest.NewRecorder()
	server.apiAdminDatabaseAction(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, "/api/admin/database/repair/cancel",
		map[string]any{"user_id": admin.ID}), "repair/cancel")
	if recorder.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if _, found, err := store.UserDatabaseRepairRequest(dataDir, admin.ID); err != nil || found {
		t.Fatalf("cancelled repair marker survived: %v %v", found, err)
	}
}

func TestAdminDatabaseRoutesRejectNonAdmins(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)
	plain := admin
	plain.IsAdmin = false

	recorder := httptest.NewRecorder()
	server.apiAdminDatabase(recorder, adminDatabaseRequest(t, server, plain, http.MethodGet, "/api/admin/database", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-admin overview status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	server.apiAdminDatabaseAction(recorder, adminDatabaseRequest(t, server, plain, http.MethodPost, "/api/admin/database/repair",
		map[string]any{"user_id": admin.ID, "confirm": true}), "repair")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-admin repair status = %d", recorder.Code)
	}
}

func TestAdminDatabaseRepairRefusesConcurrentJobSlot(t *testing.T) {
	server, _, _ := newDatabaseAdminServer(t)
	if _, ok := server.maintenance.start(maintenanceJobCheck, 0, time.Now()); !ok {
		t.Fatal("first job did not take the slot")
	}
	if _, err := server.startBackup(0); err == nil {
		t.Fatal("second job started while another was running")
	}
}

func waitForMaintenanceJob(t *testing.T, server *Server) *maintenanceJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		job := server.maintenance.snapshot()
		if job != nil && !job.Running {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("maintenance job did not finish: %+v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDatabaseOverviewReportsScheduledRepairAndLastOutcome(t *testing.T) {
	server, admin, dataDir := newDatabaseAdminServer(t)
	if err := store.ScheduleUserDatabaseRepair(dataDir, admin.ID, "admin@example.test", time.Now()); err != nil {
		t.Fatal(err)
	}
	outcome := store.RepairOutcome{
		UserID:    admin.ID,
		Succeeded: true,
		Report:    store.SalvageReport{RowsCopied: 42, RowsSkipped: 3, Gaps: 1},
	}
	if err := store.WriteUserDatabaseRepairReport(dataDir, admin.ID, outcome); err != nil {
		t.Fatal(err)
	}

	overview, err := server.databaseOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tenant := overview.Databases[1]
	if !tenant.RepairScheduled {
		t.Fatalf("scheduled repair not reported: %+v", tenant)
	}
	if tenant.LastRepair == nil || tenant.LastRepair.Report.RowsCopied != 42 {
		t.Fatalf("last repair outcome = %+v", tenant.LastRepair)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "users", "1")); err != nil {
		t.Fatal(err)
	}
}
