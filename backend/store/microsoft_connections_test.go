// File overview: Tests for connected Microsoft accounts, the calendars mirrored
// through them, and the separation between those and Google's.

package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func testMicrosoftUpsert(email string) MicrosoftConnectionUpsert {
	return MicrosoftConnectionUpsert{
		Email:                 email,
		Subject:               "oid-" + email,
		DisplayName:           "Reader",
		EncryptedRefreshToken: "v1:refresh:" + email,
		EncryptedAccessToken:  "v1:access:" + email,
		AccessTokenExpiresAt:  time.Unix(1_800_000_000, 0).UTC(),
		GrantedScopes:         []string{"Calendars.ReadWrite", "User.Read", "openid", "offline_access"},
	}
}

func TestMicrosoftConnectionsAreScopedByUser(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "m365@example.test")
	other := mustUser(t, db, ctx, "other-m365@example.test")
	connection, err := db.UpsertMicrosoftConnection(ctx, user, testMicrosoftUpsert("reader@contoso.example"))
	if err != nil {
		t.Fatal(err)
	}
	if !connection.HasScope("https://graph.microsoft.com/calendars.readwrite") {
		t.Fatalf("scopes %v should cover the long spelling of Calendars.ReadWrite", connection.GrantedScopes)
	}

	if _, err := db.MicrosoftConnection(ctx, other, connection.ID); !IsNotFound(err) {
		t.Fatalf("cross-tenant read error = %v, want not found", err)
	}
	if _, err := db.MicrosoftConnectionBySubject(ctx, other, connection.Subject); !IsNotFound(err) {
		t.Fatalf("cross-tenant subject read error = %v, want not found", err)
	}
	if err := db.DeleteMicrosoftConnection(ctx, other, connection.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant delete error = %v, want not found", err)
	}
	if err := db.UpdateMicrosoftAccessToken(ctx, other, connection.ID, "v1:stolen", time.Now(), ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant token update error = %v, want not found", err)
	}
	if err := db.MarkMicrosoftConnectionReauthRequired(ctx, other, connection.ID, "not yours"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant status update error = %v, want not found", err)
	}
	reloaded, err := db.MicrosoftConnection(ctx, user, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.EncryptedAccessToken != "v1:access:reader@contoso.example" || reloaded.NeedsReauth() {
		t.Fatalf("owner's row was touched by cross-tenant writes: %+v", reloaded)
	}
	others, err := db.ListMicrosoftConnections(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if len(others) != 0 {
		t.Fatalf("other tenant sees %d connections, want 0", len(others))
	}

	// A refresh keeps the stored refresh token when none was rotated in, and
	// a reconnect clears the reauthorization flag.
	if err := db.MarkMicrosoftConnectionReauthRequired(ctx, user, connection.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMicrosoftAccessToken(ctx, user, connection.ID, "v1:new-access", time.Now().Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	reloaded, err = db.MicrosoftConnection(ctx, user, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.EncryptedRefreshToken != "v1:refresh:reader@contoso.example" || reloaded.NeedsReauth() {
		t.Fatalf("after refresh: %+v", reloaded)
	}
}

// A Google calendar and a Microsoft calendar can carry the same remote id and
// the same connection number; they stay two rows, and disconnecting one
// provider's account takes only that provider's calendars with it.
func TestMicrosoftCalendarsAreSeparateFromGoogle(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "both@example.test")
	connection, err := db.UpsertMicrosoftConnection(ctx, user, testMicrosoftUpsert("reader@contoso.example"))
	if err != nil {
		t.Fatal(err)
	}
	google := mustCalendar(t, db, ctx, user, connection.ID, "shared-id")
	microsoft, err := db.UpsertMicrosoftCalendar(ctx, user, MicrosoftCalendarUpsert{
		ConnectionID:           connection.ID,
		RemoteCalendarID:       "shared-id",
		Summary:                "Calendar",
		AccessRole:             CalendarAccessRoleOwner,
		IsPrimary:              true,
		OnlineMeetingProviders: []string{"teamsForBusiness", " ", "teamsForBusiness"},
		Selected:               true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if microsoft.ID == google.ID {
		t.Fatal("the Microsoft calendar overwrote the Google one")
	}
	if !microsoft.IsMicrosoft() || microsoft.ConnectionID() != connection.ID || microsoft.GoogleConnectionID != 0 {
		t.Fatalf("microsoft calendar = %+v", microsoft)
	}
	if len(microsoft.OnlineMeetingProviders) != 1 || microsoft.OnlineMeetingProviders[0] != "teamsForBusiness" {
		t.Fatalf("online meeting providers = %v", microsoft.OnlineMeetingProviders)
	}
	if google.IsMicrosoft() || google.Provider != CalendarProviderGoogle {
		t.Fatalf("google calendar = %+v", google)
	}

	// The list sync renames the calendar; Rolltop's own switch survives.
	if err := db.SetCalendarSelected(ctx, user, microsoft.ID, false); err != nil {
		t.Fatal(err)
	}
	renamed, err := db.UpsertMicrosoftCalendar(ctx, user, MicrosoftCalendarUpsert{
		ConnectionID: connection.ID, RemoteCalendarID: "shared-id", Summary: "Renamed",
		AccessRole: CalendarAccessRoleOwner, Selected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != microsoft.ID || renamed.Summary != "Renamed" || renamed.Selected {
		t.Fatalf("renamed = %+v", renamed)
	}

	googleList, err := db.ListCalendarsForConnection(ctx, user, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(googleList) != 1 || googleList[0].ID != google.ID {
		t.Fatalf("google list = %+v", googleList)
	}
	microsoftList, err := db.ListCalendarsForMicrosoftConnection(ctx, user, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(microsoftList) != 1 || microsoftList[0].ID != microsoft.ID {
		t.Fatalf("microsoft list = %+v", microsoftList)
	}

	event := mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: microsoft.ID, ExternalID: "evt", Summary: "Standup",
		StartAt: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), EndAt: time.Date(2026, 10, 12, 9, 15, 0, 0, time.UTC),
		OnlineMeeting: true, OnlineMeetingProvider: "teamsForBusiness", OnlineMeetingURL: "https://teams.example/join",
	})
	if !event.OnlineMeeting || event.OnlineMeetingURL != "https://teams.example/join" || event.OnlineMeetingProvider != "teamsForBusiness" {
		t.Fatalf("online meeting fields did not round trip: %+v", event)
	}

	if err := db.DeleteMicrosoftConnection(ctx, user, connection.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Calendar(ctx, user, microsoft.ID); !IsNotFound(err) {
		t.Fatalf("microsoft calendar after disconnect: %v", err)
	}
	if _, err := db.CalendarEvent(ctx, user, event.ID); !IsNotFound(err) {
		t.Fatalf("microsoft event after disconnect: %v", err)
	}
	if _, err := db.Calendar(ctx, user, google.ID); err != nil {
		t.Fatalf("google calendar went with the microsoft disconnect: %v", err)
	}
}

// The window reconciliation of a Microsoft calendar may only see rows that
// start inside the window it read, and only its own calendar's.
func TestCalendarEventRefsStartingIn(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "window@example.test")
	other := mustUser(t, db, ctx, "window-other@example.test")
	calendar := mustCalendar(t, db, ctx, user, 1, "primary")
	foreign := mustCalendar(t, db, ctx, other, 1, "primary")
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	inside := mustEvent(t, db, ctx, user, CalendarEvent{CalendarID: calendar.ID, ExternalID: "inside", StartAt: base.Add(48 * time.Hour), EndAt: base.Add(49 * time.Hour)})
	mustEvent(t, db, ctx, user, CalendarEvent{CalendarID: calendar.ID, ExternalID: "before", StartAt: base.Add(-time.Hour), EndAt: base.Add(time.Hour)})
	mustEvent(t, db, ctx, user, CalendarEvent{CalendarID: calendar.ID, ExternalID: "after", StartAt: base.Add(240 * time.Hour), EndAt: base.Add(241 * time.Hour)})
	mustEvent(t, db, ctx, other, CalendarEvent{CalendarID: foreign.ID, ExternalID: "foreign", StartAt: base.Add(48 * time.Hour), EndAt: base.Add(49 * time.Hour)})

	refs, err := db.ListCalendarEventRefsStartingIn(ctx, user, calendar.ID, base, base.Add(240*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs["inside"].EventID != inside.ID {
		t.Fatalf("refs = %+v", refs)
	}
	if refs, err := db.ListCalendarEventRefsStartingIn(ctx, other, calendar.ID, base, base.Add(240*time.Hour)); err != nil || len(refs) != 0 {
		t.Fatalf("cross-tenant refs = %+v, %v", refs, err)
	}

	removed, err := db.DeleteCalendarEventsEndingBefore(ctx, user, calendar.ID, base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d rows that ended before the window, want 1", removed)
	}
	if removed, err := db.DeleteCalendarEventsEndingBefore(ctx, other, calendar.ID, base.Add(1000*time.Hour)); err != nil || removed != 0 {
		t.Fatalf("cross-tenant delete removed %d, %v", removed, err)
	}
}
