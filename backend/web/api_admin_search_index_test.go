package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"rolltop/backend/search"
)

const searchIndexPath = "/api/admin/search-index"

func attachSearchService(t *testing.T, server *Server) string {
	t.Helper()
	root := t.TempDir()
	svc, err := search.OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	server.search = svc
	return root
}

func decodeSearchIndexReport(t *testing.T, recorder *httptest.ResponseRecorder) searchIndexReport {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var report searchIndexReport
	if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	return report
}

func TestAdminSearchIndexRequiresAdmin(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)
	attachSearchService(t, server)
	user := admin
	user.IsAdmin = false

	recorder := httptest.NewRecorder()
	server.apiAdminSearchIndex(recorder, adminDatabaseRequest(t, server, user, http.MethodGet, searchIndexPath, nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	server.apiAdminSearchIndex(recorder, adminDatabaseRequest(t, server, user, http.MethodPost, searchIndexPath,
		map[string]int64{"user_id": admin.ID}))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-admin rebuild status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// Reading the card must never change the indexes it reports on. An operator
// looking at a damaged index has not asked for it to be thrown away.
func TestAdminSearchIndexReportLeavesIndexesAlone(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)
	root := attachSearchService(t, server)
	indexPath := filepath.Join(root, "1", "bleve")
	if err := os.MkdirAll(indexPath, 0o700); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	server.apiAdminSearchIndex(recorder, adminDatabaseRequest(t, server, admin, http.MethodGet, searchIndexPath, nil))
	report := decodeSearchIndexReport(t, recorder)
	if len(report.Tenants) != 1 || report.Tenants[0].UserID != admin.ID {
		t.Fatalf("tenants = %+v", report.Tenants)
	}
	if !report.Tenants[0].Present {
		t.Fatalf("tenant index reported absent: %+v", report.Tenants[0])
	}
	entries, err := os.ReadDir(filepath.Join(root, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != search.LiveIndexDirName {
		t.Fatalf("reading the report changed the index directory: %v", entries)
	}
}

func TestAdminSearchIndexRebuildQuarantinesAndQueuesReindex(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)
	root := attachSearchService(t, server)
	// A live index directory is what the rebuild has to move aside.
	if err := os.MkdirAll(filepath.Join(root, "1", search.LiveIndexDirName), 0o700); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	server.apiAdminSearchIndex(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, searchIndexPath,
		map[string]int64{"user_id": admin.ID}))
	report := decodeSearchIndexReport(t, recorder)
	if report.Rebuilt != admin.ID {
		t.Fatalf("rebuilt = %d, want %d", report.Rebuilt, admin.ID)
	}
	if _, err := os.Stat(filepath.Join(root, "1", search.LiveIndexDirName)); !os.IsNotExist(err) {
		t.Fatalf("live index survived the rebuild: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("tenant directory = %v, want exactly the quarantine", entries)
	}
	if report.Tenants[0].Present {
		t.Fatalf("rebuilt tenant still reports a live index: %+v", report.Tenants[0])
	}
}

func TestAdminSearchIndexRebuildRejectsBadRequests(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)
	attachSearchService(t, server)

	recorder := httptest.NewRecorder()
	server.apiAdminSearchIndex(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, searchIndexPath,
		map[string]int64{"user_id": 0}))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing user status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	server.apiAdminSearchIndex(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, searchIndexPath,
		map[string]int64{"user_id": 9999}))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown user status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	server.apiAdminSearchIndex(recorder, adminDatabaseRequest(t, server, admin, http.MethodDelete, searchIndexPath, nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE status = %d", recorder.Code)
	}
}
