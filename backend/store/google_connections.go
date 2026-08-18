// File overview: Tenant-scoped persistence for connected Google accounts. Every
// helper here takes the token ciphertext produced by backend/crypto; this layer
// never sees, derives, or logs a plain-text OAuth token.

package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

const (
	// GoogleConnectionStatusOK marks a connection whose refresh token still works.
	GoogleConnectionStatusOK = "ok"
	// GoogleConnectionStatusReauthRequired marks a connection Google has revoked.
	// Everything hanging off it (mail, contacts, calendar) is expected to pause
	// until the user re-authorizes.
	GoogleConnectionStatusReauthRequired = "reauth_required"
)

// ErrInvalidGoogleConnection reports a connection row that fails validation.
var ErrInvalidGoogleConnection = errors.New("invalid google connection")

const googleConnectionSelectColumns = `id, user_id, google_email, google_subject,
	encrypted_refresh_token, encrypted_access_token, access_token_expires_at,
	granted_scopes, status, status_detail, created_at, updated_at`

// GoogleConnection is one authorized Google account belonging to one Rolltop user.
// Token fields hold AES-GCM ciphertext, matching how IMAP passwords are stored.
type GoogleConnection struct {
	ID                    int64
	UserID                int64
	GoogleEmail           string
	GoogleSubject         string
	EncryptedRefreshToken string
	EncryptedAccessToken  string
	AccessTokenExpiresAt  time.Time
	GrantedScopes         []string
	Status                string
	StatusDetail          string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// HasScope reports whether Google granted a specific scope to this connection.
func (c GoogleConnection) HasScope(scope string) bool {
	scope = strings.TrimSpace(scope)
	for _, granted := range c.GrantedScopes {
		if granted == scope {
			return true
		}
	}
	return false
}

// NeedsReauth reports whether the connection is unusable until the user
// re-runs the consent flow.
func (c GoogleConnection) NeedsReauth() bool {
	return c.Status == GoogleConnectionStatusReauthRequired
}

// GoogleConnectionUpsert carries the result of a completed authorization code
// exchange into storage.
type GoogleConnectionUpsert struct {
	GoogleEmail           string
	GoogleSubject         string
	EncryptedRefreshToken string
	EncryptedAccessToken  string
	AccessTokenExpiresAt  time.Time
	GrantedScopes         []string
}

// normalizeGoogleScopes deduplicates and orders scopes so stored values stay
// comparable across reconnects regardless of the order Google returns them in.
func normalizeGoogleScopes(scopes []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || seen[scope] {
			continue
		}
		seen[scope] = true
		out = append(out, scope)
	}
	sort.Strings(out)
	return out
}

func splitGoogleScopes(raw string) []string {
	return normalizeGoogleScopes(strings.Fields(raw))
}

// UpsertGoogleConnection stores a freshly authorized connection, reusing the
// existing row when the same Google address is reconnected. A reconnect always
// clears a previous reauth_required state because consent just succeeded.
func (s *Store) UpsertGoogleConnection(ctx context.Context, userID int64, in GoogleConnectionUpsert) (GoogleConnection, error) {
	email := cleanEmail(in.GoogleEmail)
	subject := strings.TrimSpace(in.GoogleSubject)
	if userID <= 0 || email == "" || subject == "" {
		// Without the subject claim a reconnect cannot be matched to its
		// existing row, so a renamed Google account would silently fork into
		// a duplicate connection.
		return GoogleConnection{}, ErrInvalidGoogleConnection
	}
	if strings.TrimSpace(in.EncryptedRefreshToken) == "" {
		// Without a refresh token the connection dies at the first access-token
		// expiry, so refuse to persist a half-usable row.
		return GoogleConnection{}, ErrInvalidGoogleConnection
	}
	scopes := strings.Join(normalizeGoogleScopes(in.GrantedScopes), " ")
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO google_connections
			(user_id, google_email, google_subject, encrypted_refresh_token,
			 encrypted_access_token, access_token_expires_at, granted_scopes,
			 status, status_detail, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(user_id, google_subject) DO UPDATE SET
			google_email = excluded.google_email,
			encrypted_refresh_token = excluded.encrypted_refresh_token,
			encrypted_access_token = excluded.encrypted_access_token,
			access_token_expires_at = excluded.access_token_expires_at,
			granted_scopes = excluded.granted_scopes,
			status = excluded.status,
			status_detail = '',
			updated_at = excluded.updated_at`,
		userID, email, subject, in.EncryptedRefreshToken,
		in.EncryptedAccessToken, timeUnix(in.AccessTokenExpiresAt), scopes,
		GoogleConnectionStatusOK, ts, ts)
	if err != nil {
		return GoogleConnection{}, err
	}
	return s.GoogleConnectionBySubject(ctx, userID, subject)
}

// ListGoogleConnections returns every Google account the user has connected.
func (s *Store) ListGoogleConnections(ctx context.Context, userID int64) ([]GoogleConnection, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT `+googleConnectionSelectColumns+`
		FROM google_connections WHERE user_id = ? ORDER BY google_email ASC, id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	connections := []GoogleConnection{}
	for rows.Next() {
		connection, err := scanGoogleConnection(rows)
		if err != nil {
			return nil, err
		}
		connections = append(connections, connection)
	}
	return connections, rows.Err()
}

// GoogleConnection loads one connection, scoped to its owner so a guessed id
// from another tenant reads as not found.
func (s *Store) GoogleConnection(ctx context.Context, userID, connectionID int64) (GoogleConnection, error) {
	return scanGoogleConnection(s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT `+googleConnectionSelectColumns+`
		FROM google_connections WHERE user_id = ? AND id = ?`, userID, connectionID))
}

// GoogleConnectionBySubject loads one connection by Google's stable account
// identifier, which is what a reconnect matches on because the account's
// primary address can change underneath us.
func (s *Store) GoogleConnectionBySubject(ctx context.Context, userID int64, subject string) (GoogleConnection, error) {
	return scanGoogleConnection(s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT `+googleConnectionSelectColumns+`
		FROM google_connections WHERE user_id = ? AND google_subject = ?`, userID, strings.TrimSpace(subject)))
}

// UpdateGoogleAccessToken persists a refreshed access token. Google only returns
// a new refresh token when it rotates one, so an empty value leaves the stored
// refresh token untouched rather than erasing it.
func (s *Store) UpdateGoogleAccessToken(ctx context.Context, userID, connectionID int64,
	encryptedAccessToken string, expiresAt time.Time, encryptedRefreshToken string) error {
	if userID <= 0 || connectionID <= 0 {
		return ErrInvalidGoogleConnection
	}
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE google_connections SET
			encrypted_access_token = ?,
			access_token_expires_at = ?,
			encrypted_refresh_token = CASE WHEN ? = '' THEN encrypted_refresh_token ELSE ? END,
			status = ?,
			status_detail = '',
			updated_at = ?
		WHERE user_id = ? AND id = ?`,
		encryptedAccessToken, timeUnix(expiresAt),
		encryptedRefreshToken, encryptedRefreshToken,
		GoogleConnectionStatusOK, nowUnix(), userID, connectionID)
	if err != nil {
		return err
	}
	return requireGoogleConnectionRow(result)
}

// MarkGoogleConnectionReauthRequired records that Google rejected the refresh
// token. The stale access token is dropped so no caller can keep using it.
func (s *Store) MarkGoogleConnectionReauthRequired(ctx context.Context, userID, connectionID int64, detail string) error {
	if userID <= 0 || connectionID <= 0 {
		return ErrInvalidGoogleConnection
	}
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE google_connections SET
			status = ?,
			status_detail = ?,
			encrypted_access_token = '',
			access_token_expires_at = 0,
			updated_at = ?
		WHERE user_id = ? AND id = ?`,
		GoogleConnectionStatusReauthRequired, trimLimit(strings.TrimSpace(detail), 500),
		nowUnix(), userID, connectionID)
	if err != nil {
		return err
	}
	return requireGoogleConnectionRow(result)
}

// DeleteGoogleConnection removes a connection from the tenant's database. The
// caller is responsible for revoking the grant at Google first; deletion still
// proceeds when revocation fails so a user can always detach a broken account.
func (s *Store) DeleteGoogleConnection(ctx context.Context, userID, connectionID int64) error {
	if userID <= 0 || connectionID <= 0 {
		return ErrInvalidGoogleConnection
	}
	// Everything hanging off the connection stops pointing at it in the same
	// transaction that removes it, because nothing else runs on disconnect. A
	// contact left with source 'google' and a connection id that resolves to
	// nothing is worse than either end state: it is filtered out of the local
	// listing and only reachable through a filter for an account that is gone.
	// Contacts keep their data and become local; the sync cursor is meaningless
	// without the grant it was issued under and would otherwise be picked up by
	// a later reconnect that reuses the id.
	//
	// Calendars go the other way and are removed outright. A contact can be
	// typed in here and often exists nowhere else, so demoting it preserves the
	// user's own data; a calendar is a pure mirror with no local editor behind
	// it, and keeping one would leave a row in the sidebar that can never sync,
	// never be edited, and never be switched off again.
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`DELETE FROM google_connections WHERE user_id = ? AND id = ?`, userID, connectionID)
	if err != nil {
		return err
	}
	if err := requireGoogleConnectionRow(result); err != nil {
		return err
	}
	if _, err := demoteGoogleContacts(ctx, tx, userID, connectionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM google_people_sync WHERE user_id = ? AND connection_id = ?`, userID, connectionID); err != nil {
		return err
	}
	if err := deleteCalendarsForConnection(ctx, tx, userID, connectionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM google_calendar_sync WHERE user_id = ? AND connection_id = ?`, userID, connectionID); err != nil {
		return err
	}
	return tx.Commit()
}

// requireGoogleConnectionRow turns a no-op write into the shared not-found
// sentinel so cross-tenant ids cannot be distinguished from missing ones.
func requireGoogleConnectionRow(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanGoogleConnection(dest scanDest) (GoogleConnection, error) {
	var connection GoogleConnection
	var scopes string
	var expiresAt, createdAt, updatedAt int64
	if err := dest.Scan(&connection.ID, &connection.UserID, &connection.GoogleEmail,
		&connection.GoogleSubject, &connection.EncryptedRefreshToken,
		&connection.EncryptedAccessToken, &expiresAt, &scopes, &connection.Status,
		&connection.StatusDetail, &createdAt, &updatedAt); err != nil {
		return GoogleConnection{}, err
	}
	connection.GrantedScopes = splitGoogleScopes(scopes)
	connection.AccessTokenExpiresAt = unixTime(expiresAt)
	connection.CreatedAt = unixTime(createdAt)
	connection.UpdatedAt = unixTime(updatedAt)
	return connection, nil
}
