// File overview: Tests for connected Google account persistence and tenant isolation.

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testGoogleUpsert(email string) GoogleConnectionUpsert {
	return GoogleConnectionUpsert{
		GoogleEmail:           email,
		GoogleSubject:         "sub-" + email,
		EncryptedRefreshToken: "v1:refresh:" + email,
		EncryptedAccessToken:  "v1:access:" + email,
		AccessTokenExpiresAt:  time.Unix(1_800_000_000, 0).UTC(),
		GrantedScopes:         []string{"https://mail.google.com/", "openid", "email"},
	}
}

func TestGoogleConnectionsAreScopedByUser(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "google@example.test", "Google", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "other-google@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.UpsertGoogleConnection(ctx, user.ID, testGoogleUpsert("shared@gmail.example.test"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.GoogleConnection(ctx, other.ID, connection.ID); !IsNotFound(err) {
		t.Fatalf("cross-tenant read error = %v, want not found", err)
	}
	if _, err := db.GoogleConnectionByEmail(ctx, other.ID, "shared@gmail.example.test"); !IsNotFound(err) {
		t.Fatalf("cross-tenant email read error = %v, want not found", err)
	}
	if err := db.DeleteGoogleConnection(ctx, other.ID, connection.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant delete error = %v, want not found", err)
	}
	if err := db.UpdateGoogleAccessToken(ctx, other.ID, connection.ID, "v1:stolen:token", time.Now(), ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant token update error = %v, want not found", err)
	}
	if err := db.MarkGoogleConnectionReauthRequired(ctx, other.ID, connection.ID, "not yours"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant status update error = %v, want not found", err)
	}

	// None of the rejected cross-tenant writes may have touched the owner's row.
	reloaded, err := db.GoogleConnection(ctx, user.ID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.EncryptedAccessToken != "v1:access:shared@gmail.example.test" {
		t.Fatalf("access token after cross-tenant writes = %q", reloaded.EncryptedAccessToken)
	}
	if reloaded.Status != GoogleConnectionStatusOK {
		t.Fatalf("status after cross-tenant writes = %q, want %q", reloaded.Status, GoogleConnectionStatusOK)
	}

	others, err := db.ListGoogleConnections(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(others) != 0 {
		t.Fatalf("other tenant sees %d connections, want 0", len(others))
	}
}

func TestUpsertGoogleConnectionReusesRowAndClearsReauth(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "reconnect@example.test", "Reconnect", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.UpsertGoogleConnection(ctx, user.ID, testGoogleUpsert("me@gmail.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkGoogleConnectionReauthRequired(ctx, user.ID, first.ID, "invalid_grant"); err != nil {
		t.Fatal(err)
	}
	broken, err := db.GoogleConnection(ctx, user.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !broken.NeedsReauth() {
		t.Fatal("connection did not enter reauth_required")
	}
	if broken.EncryptedAccessToken != "" || !broken.AccessTokenExpiresAt.IsZero() {
		t.Fatalf("stale access token survived reauth marking: %q %v", broken.EncryptedAccessToken, broken.AccessTokenExpiresAt)
	}

	// Reconnecting the same Google address must heal the existing row, not add a second one.
	second, err := db.UpsertGoogleConnection(ctx, user.ID, testGoogleUpsert("me@gmail.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("reconnect created connection id %d, want reuse of %d", second.ID, first.ID)
	}
	if second.Status != GoogleConnectionStatusOK || second.StatusDetail != "" {
		t.Fatalf("reconnect left status %q detail %q", second.Status, second.StatusDetail)
	}
	all, err := db.ListGoogleConnections(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("connections after reconnect = %d, want 1", len(all))
	}
}

func TestUpsertGoogleConnectionNormalizesScopesAndEmail(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "scopes@example.test", "Scopes", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	in := testGoogleUpsert("Mixed.Case@Gmail.Example.Test")
	in.GrantedScopes = []string{"email", " openid ", "email", ""}
	connection, err := db.UpsertGoogleConnection(ctx, user.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if connection.GoogleEmail != "mixed.case@gmail.example.test" {
		t.Fatalf("stored email = %q, want lowercased", connection.GoogleEmail)
	}
	if len(connection.GrantedScopes) != 2 ||
		connection.GrantedScopes[0] != "email" || connection.GrantedScopes[1] != "openid" {
		t.Fatalf("stored scopes = %v, want deduplicated and sorted", connection.GrantedScopes)
	}
	if !connection.HasScope("openid") || connection.HasScope("https://mail.google.com/") {
		t.Fatalf("HasScope disagrees with stored scopes %v", connection.GrantedScopes)
	}
}

func TestUpsertGoogleConnectionRejectsMissingRefreshToken(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "norefresh@example.test", "No Refresh", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	in := testGoogleUpsert("norefresh@gmail.example.test")
	in.EncryptedRefreshToken = "  "
	if _, err := db.UpsertGoogleConnection(ctx, user.ID, in); !errors.Is(err, ErrInvalidGoogleConnection) {
		t.Fatalf("upsert without refresh token error = %v, want ErrInvalidGoogleConnection", err)
	}
	in.GoogleEmail = ""
	in.EncryptedRefreshToken = "v1:refresh:x"
	if _, err := db.UpsertGoogleConnection(ctx, user.ID, in); !errors.Is(err, ErrInvalidGoogleConnection) {
		t.Fatalf("upsert without email error = %v, want ErrInvalidGoogleConnection", err)
	}
}

func TestUpdateGoogleAccessTokenKeepsRefreshTokenWhenNotRotated(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "refresh@example.test", "Refresh", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.UpsertGoogleConnection(ctx, user.ID, testGoogleUpsert("refresh@gmail.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Unix(1_900_000_000, 0).UTC()
	if err := db.UpdateGoogleAccessToken(ctx, user.ID, connection.ID, "v1:access:new", expiry, ""); err != nil {
		t.Fatal(err)
	}
	kept, err := db.GoogleConnection(ctx, user.ID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.EncryptedRefreshToken != connection.EncryptedRefreshToken {
		t.Fatalf("refresh token = %q, want unchanged %q", kept.EncryptedRefreshToken, connection.EncryptedRefreshToken)
	}
	if kept.EncryptedAccessToken != "v1:access:new" || !kept.AccessTokenExpiresAt.Equal(expiry) {
		t.Fatalf("access token = %q expiry = %v", kept.EncryptedAccessToken, kept.AccessTokenExpiresAt)
	}

	// A rotated refresh token replaces the stored one.
	if err := db.UpdateGoogleAccessToken(ctx, user.ID, connection.ID, "v1:access:newer", expiry, "v1:refresh:rotated"); err != nil {
		t.Fatal(err)
	}
	rotated, err := db.GoogleConnection(ctx, user.ID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.EncryptedRefreshToken != "v1:refresh:rotated" {
		t.Fatalf("rotated refresh token = %q", rotated.EncryptedRefreshToken)
	}
}

func TestUpdateGoogleAccessTokenClearsReauthState(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "recover@example.test", "Recover", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.UpsertGoogleConnection(ctx, user.ID, testGoogleUpsert("recover@gmail.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkGoogleConnectionReauthRequired(ctx, user.ID, connection.ID, "invalid_grant"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateGoogleAccessToken(ctx, user.ID, connection.ID, "v1:access:ok", time.Unix(1_900_000_000, 0), ""); err != nil {
		t.Fatal(err)
	}
	healed, err := db.GoogleConnection(ctx, user.ID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if healed.NeedsReauth() || healed.StatusDetail != "" {
		t.Fatalf("successful refresh left status %q detail %q", healed.Status, healed.StatusDetail)
	}
}
