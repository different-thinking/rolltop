package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

func authenticatedRetentionRequest(t *testing.T, server *Server, user store.User, method string, body []byte) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, "/api/profile/retention", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: user}))
	if method != http.MethodGet {
		const csrfBase = "retention-csrf"
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
		request.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func retentionThrough(t *testing.T, server *Server, user store.User, method string, body []byte) (int, apiRetentionSettings) {
	t.Helper()
	response := httptest.NewRecorder()
	server.apiRetention(response, authenticatedRetentionRequest(t, server, user, method, body))
	var payload struct {
		Retention apiRetentionSettings `json:"retention"`
	}
	if response.Code == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			t.Fatalf("decode %s response: %v", method, err)
		}
	}
	return response.Code, payload.Retention
}

func TestRetentionAPIRequiresAuthAndCSRF(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "retention@example.test", "Retention", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{9}, 32)}

	unauthenticated := httptest.NewRecorder()
	server.apiRetention(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/profile/retention", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthenticated.Code)
	}

	body, _ := json.Marshal(apiRetentionSettings{TrashEnabled: true, TrashDays: 30})
	missingCSRF := httptest.NewRequest(http.MethodPost, "/api/profile/retention", bytes.NewReader(body))
	missingCSRF = missingCSRF.WithContext(context.WithValue(missingCSRF.Context(), userContextKey, currentUser{User: user}))
	missingCSRFResponse := httptest.NewRecorder()
	server.apiRetention(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want 403", missingCSRFResponse.Code)
	}
}

func TestRetentionAPIRoundTripsAPolicy(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "retention@example.test", "Retention", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{9}, 32), retentionSchedulerWake: make(chan struct{}, 1)}

	status, defaults := retentionThrough(t, server, user, http.MethodGet, nil)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", status)
	}
	if !defaults.TrashEnabled || defaults.TrashDays != store.DefaultTrashRetentionDays {
		t.Fatalf("default policy = %+v, want the Trash emptying itself after %d days",
			defaults, store.DefaultTrashRetentionDays)
	}
	if len(defaults.Categories) != 0 {
		t.Fatalf("default category rules = %+v, want none", defaults.Categories)
	}

	body, _ := json.Marshal(apiRetentionSettings{
		TrashEnabled: true, TrashDays: 45,
		Categories: []apiCategoryRetention{
			{Category: "newsletters", Mode: "relative", Count: 6, Unit: "months"},
			{Category: "forums", Mode: "fixed", Before: "2024-03-01"},
			{Category: "relevant", Mode: "off"},
		},
	})
	status, saved := retentionThrough(t, server, user, http.MethodPost, body)
	if status != http.StatusOK {
		t.Fatalf("POST status = %d", status)
	}
	if saved.TrashDays != 45 {
		t.Fatalf("saved Trash rule = %d days, want 45", saved.TrashDays)
	}
	if len(saved.Categories) != 2 {
		t.Fatalf("saved category rules = %+v, want the two that are not off", saved.Categories)
	}
	if saved.Categories[0].Category != "newsletters" || saved.Categories[0].Count != 6 || saved.Categories[0].Unit != "months" {
		t.Fatalf("first saved rule = %+v, want newsletters kept 6 months", saved.Categories[0])
	}
	if saved.Categories[1].Category != "forums" || saved.Categories[1].Before != "2024-03-01T00:00:00Z" {
		t.Fatalf("second saved rule = %+v, want forums before 2024-03-01", saved.Categories[1])
	}

	// Saving asks for a pass, because the marks it cleared are what says the
	// policy is due.
	select {
	case <-server.retentionSchedulerWake:
	default:
		t.Fatal("saving a policy did not wake the retention scheduler, so a new rule would wait out an interval it was not part of")
	}

	_, reread := retentionThrough(t, server, user, http.MethodGet, nil)
	if len(reread.Categories) != 2 || reread.TrashDays != 45 {
		t.Fatalf("reread policy = %+v, want what was saved", reread)
	}
}

func TestRetentionAPIRefusesRulesItCannotAct(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "retention@example.test", "Retention", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{9}, 32), retentionSchedulerWake: make(chan struct{}, 1)}

	for _, tc := range []struct {
		name     string
		settings apiRetentionSettings
	}{
		{"fixed rule with no date", apiRetentionSettings{TrashEnabled: true, TrashDays: 30,
			Categories: []apiCategoryRetention{{Category: "newsletters", Mode: "fixed"}}}},
		{"fixed rule with an unreadable date", apiRetentionSettings{TrashEnabled: true, TrashDays: 30,
			Categories: []apiCategoryRetention{{Category: "newsletters", Mode: "fixed", Before: "last Tuesday"}}}},
		{"unknown category", apiRetentionSettings{TrashEnabled: true, TrashDays: 30,
			Categories: []apiCategoryRetention{{Category: "postcards", Mode: "relative", Count: 5, Unit: "days"}}}},
		{"Trash rule with no days", apiRetentionSettings{TrashEnabled: true}},
	} {
		body, _ := json.Marshal(tc.settings)
		status, _ := retentionThrough(t, server, user, http.MethodPost, body)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", tc.name, status)
		}
	}
}
