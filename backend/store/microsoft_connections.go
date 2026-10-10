// File overview: Tenant-scoped persistence for connected Microsoft 365 (and
// personal Microsoft) accounts. It follows google_connections.go: the token
// columns hold ciphertext produced by backend/crypto, and this layer never
// sees, derives or logs a plain-text OAuth token.

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const (
	// MicrosoftConnectionStatusOK marks a connection whose refresh token works.
	MicrosoftConnectionStatusOK = "ok"
	// MicrosoftConnectionStatusReauthRequired marks a connection Microsoft
	// stopped honouring: revoked consent, an expired refresh token, a
	// conditional-access policy that now wants an interactive sign-in.
	MicrosoftConnectionStatusReauthRequired = "reauth_required"
)

// ErrInvalidMicrosoftConnection reports a connection row that fails validation.
var ErrInvalidMicrosoftConnection = errors.New("invalid microsoft connection")

// graphScopePrefix is how the identity platform may spell a Graph scope in
// full. Token responses use the short form for Graph, but a consent recorded
// by a different client library can carry the long one, and both are the same
// permission.
const graphScopePrefix = "https://graph.microsoft.com/"

const microsoftConnectionSelectColumns = `id, user_id, microsoft_email, microsoft_subject,
	display_name, encrypted_refresh_token, encrypted_access_token, access_token_expires_at,
	granted_scopes, status, status_detail, created_at, updated_at`

// MicrosoftConnection is one authorized Microsoft account of one Rolltop user.
type MicrosoftConnection struct {
	ID     int64
	UserID int64
	// Email is the account's mail address, or its sign-in name where the
	// directory records no mailbox address.
	Email string
	// Subject is Graph's stable user id, which is what a reconnect matches on:
	// the address of a work account can be renamed underneath it.
	Subject               string
	DisplayName           string
	EncryptedRefreshToken string
	EncryptedAccessToken  string
	AccessTokenExpiresAt  time.Time
	GrantedScopes         []string
	Status                string
	StatusDetail          string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// HasScope reports whether Microsoft granted a scope to this connection. Graph
// scopes compare without their resource prefix and without case, because the
// identity platform reports them either way.
func (c MicrosoftConnection) HasScope(scope string) bool {
	want := normalizeMicrosoftScope(scope)
	if want == "" {
		return false
	}
	for _, granted := range c.GrantedScopes {
		if normalizeMicrosoftScope(granted) == want {
			return true
		}
	}
	return false
}

// NeedsReauth reports whether the connection is unusable until the user signs
// in again.
func (c MicrosoftConnection) NeedsReauth() bool {
	return c.Status == MicrosoftConnectionStatusReauthRequired
}

func normalizeMicrosoftScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if len(scope) >= len(graphScopePrefix) && strings.EqualFold(scope[:len(graphScopePrefix)], graphScopePrefix) {
		scope = scope[len(graphScopePrefix):]
	}
	return strings.ToLower(scope)
}

// MicrosoftConnectionUpsert carries a completed authorization into storage.
type MicrosoftConnectionUpsert struct {
	Email                 string
	Subject               string
	DisplayName           string
	EncryptedRefreshToken string
	EncryptedAccessToken  string
	AccessTokenExpiresAt  time.Time
	GrantedScopes         []string
}

// UpsertMicrosoftConnection stores a freshly authorized connection, reusing the
// row of an account that is reconnected. A reconnect clears reauth_required,
// because consent just succeeded.
func (s *Store) UpsertMicrosoftConnection(ctx context.Context, userID int64, in MicrosoftConnectionUpsert) (MicrosoftConnection, error) {
	email := cleanEmail(in.Email)
	subject := strings.TrimSpace(in.Subject)
	if userID <= 0 || email == "" || subject == "" {
		return MicrosoftConnection{}, ErrInvalidMicrosoftConnection
	}
	if strings.TrimSpace(in.EncryptedRefreshToken) == "" {
		// A connection without a refresh token dies within the hour; refusing
		// to store it means the user sees the failure at the connect button.
		return MicrosoftConnection{}, ErrInvalidMicrosoftConnection
	}
	scopes := strings.Join(normalizeGoogleScopes(in.GrantedScopes), " ")
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO microsoft_connections
			(user_id, microsoft_email, microsoft_subject, display_name, encrypted_refresh_token,
			 encrypted_access_token, access_token_expires_at, granted_scopes,
			 status, status_detail, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(user_id, microsoft_subject) DO UPDATE SET
			microsoft_email = excluded.microsoft_email,
			display_name = excluded.display_name,
			encrypted_refresh_token = excluded.encrypted_refresh_token,
			encrypted_access_token = excluded.encrypted_access_token,
			access_token_expires_at = excluded.access_token_expires_at,
			granted_scopes = excluded.granted_scopes,
			status = excluded.status,
			status_detail = '',
			updated_at = excluded.updated_at`,
		userID, email, subject, trimLimit(strings.TrimSpace(in.DisplayName), 300),
		in.EncryptedRefreshToken, in.EncryptedAccessToken, timeUnix(in.AccessTokenExpiresAt), scopes,
		MicrosoftConnectionStatusOK, ts, ts)
	if err != nil {
		return MicrosoftConnection{}, err
	}
	return s.MicrosoftConnectionBySubject(ctx, userID, subject)
}

// ListMicrosoftConnections returns every Microsoft account the user connected.
func (s *Store) ListMicrosoftConnections(ctx context.Context, userID int64) ([]MicrosoftConnection, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT `+microsoftConnectionSelectColumns+`
		FROM microsoft_connections WHERE user_id = ? ORDER BY microsoft_email ASC, id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	connections := []MicrosoftConnection{}
	for rows.Next() {
		connection, err := scanMicrosoftConnection(rows)
		if err != nil {
			return nil, err
		}
		connections = append(connections, connection)
	}
	return connections, rows.Err()
}

// MicrosoftConnection loads one connection, scoped to its owner so an id
// guessed from another tenant reads as not found.
func (s *Store) MicrosoftConnection(ctx context.Context, userID, connectionID int64) (MicrosoftConnection, error) {
	return scanMicrosoftConnection(s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT `+microsoftConnectionSelectColumns+`
		FROM microsoft_connections WHERE user_id = ? AND id = ?`, userID, connectionID))
}

// MicrosoftConnectionBySubject loads one connection by Graph's user id.
func (s *Store) MicrosoftConnectionBySubject(ctx context.Context, userID int64, subject string) (MicrosoftConnection, error) {
	return scanMicrosoftConnection(s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT `+microsoftConnectionSelectColumns+`
		FROM microsoft_connections WHERE user_id = ? AND microsoft_subject = ?`, userID, strings.TrimSpace(subject)))
}

// UpdateMicrosoftAccessToken persists a refreshed access token. Microsoft
// rotates the refresh token on most refreshes; an empty value keeps the stored
// one rather than erasing it.
func (s *Store) UpdateMicrosoftAccessToken(ctx context.Context, userID, connectionID int64,
	encryptedAccessToken string, expiresAt time.Time, encryptedRefreshToken string) error {
	if userID <= 0 || connectionID <= 0 {
		return ErrInvalidMicrosoftConnection
	}
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE microsoft_connections SET
			encrypted_access_token = ?,
			access_token_expires_at = ?,
			encrypted_refresh_token = CASE WHEN ? = '' THEN encrypted_refresh_token ELSE ? END,
			status = ?,
			status_detail = '',
			updated_at = ?
		WHERE user_id = ? AND id = ?`,
		encryptedAccessToken, timeUnix(expiresAt),
		encryptedRefreshToken, encryptedRefreshToken,
		MicrosoftConnectionStatusOK, nowUnix(), userID, connectionID)
	if err != nil {
		return err
	}
	return requireGoogleConnectionRow(result)
}

// MarkMicrosoftConnectionReauthRequired records that Microsoft rejected the
// refresh token, dropping the stale access token so nobody keeps using it.
func (s *Store) MarkMicrosoftConnectionReauthRequired(ctx context.Context, userID, connectionID int64, detail string) error {
	if userID <= 0 || connectionID <= 0 {
		return ErrInvalidMicrosoftConnection
	}
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE microsoft_connections SET
			status = ?,
			status_detail = ?,
			encrypted_access_token = '',
			access_token_expires_at = 0,
			updated_at = ?
		WHERE user_id = ? AND id = ?`,
		MicrosoftConnectionStatusReauthRequired, trimLimit(strings.TrimSpace(detail), 500),
		nowUnix(), userID, connectionID)
	if err != nil {
		return err
	}
	return requireGoogleConnectionRow(result)
}

// DeleteMicrosoftConnection removes a connection together with the calendars
// mirrored through it, for the reason DeleteGoogleConnection gives: a calendar
// is a pure mirror, and one left behind could never sync or be switched off.
func (s *Store) DeleteMicrosoftConnection(ctx context.Context, userID, connectionID int64) error {
	if userID <= 0 || connectionID <= 0 {
		return ErrInvalidMicrosoftConnection
	}
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
		`DELETE FROM microsoft_connections WHERE user_id = ? AND id = ?`, userID, connectionID)
	if err != nil {
		return err
	}
	if err := requireGoogleConnectionRow(result); err != nil {
		return err
	}
	if err := deleteCalendarsForMicrosoftConnection(ctx, tx, userID, connectionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM microsoft_calendar_sync WHERE user_id = ? AND connection_id = ?`, userID, connectionID); err != nil {
		return err
	}
	return tx.Commit()
}

// MicrosoftCalendarSync is the per-connection outcome of the calendar-list
// sync. Graph has no cursor for the list, so there is none to keep.
type MicrosoftCalendarSync struct {
	UserID        int64
	ConnectionID  int64
	LastSyncAt    time.Time
	LastSuccessAt time.Time
	Status        string
	StatusDetail  string
}

// GetMicrosoftCalendarSync loads the list-sync state of one connection. A
// connection that never synced has no row and reads back as an empty state.
func (s *Store) GetMicrosoftCalendarSync(ctx context.Context, userID, connectionID int64) (MicrosoftCalendarSync, error) {
	state := MicrosoftCalendarSync{UserID: userID, ConnectionID: connectionID}
	if userID <= 0 || connectionID <= 0 {
		return state, ErrNotFound
	}
	var lastSync, lastSuccess int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx,
		`SELECT last_sync_at, last_success_at, status, status_detail
			FROM microsoft_calendar_sync WHERE user_id = ? AND connection_id = ?`,
		userID, connectionID).
		Scan(&lastSync, &lastSuccess, &state.Status, &state.StatusDetail)
	if err == sql.ErrNoRows {
		return state, nil
	}
	if err != nil {
		return MicrosoftCalendarSync{}, err
	}
	state.LastSyncAt = unixTime(lastSync)
	state.LastSuccessAt = unixTime(lastSuccess)
	return state, nil
}

// SaveMicrosoftCalendarSync writes the outcome of one list sync.
func (s *Store) SaveMicrosoftCalendarSync(ctx context.Context, state MicrosoftCalendarSync) error {
	if state.UserID <= 0 || state.ConnectionID <= 0 {
		return ErrNotFound
	}
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, state.UserID).ExecContext(ctx,
		`INSERT INTO microsoft_calendar_sync
				(user_id, connection_id, last_sync_at, last_success_at, status, status_detail, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(user_id, connection_id) DO UPDATE SET
				last_sync_at = excluded.last_sync_at,
				last_success_at = excluded.last_success_at,
				status = excluded.status,
				status_detail = excluded.status_detail,
				updated_at = excluded.updated_at`,
		state.UserID, state.ConnectionID, timeUnix(state.LastSyncAt),
		timeUnix(state.LastSuccessAt), state.Status, trimLimit(state.StatusDetail, 500), ts, ts)
	return err
}

func scanMicrosoftConnection(dest scanDest) (MicrosoftConnection, error) {
	var connection MicrosoftConnection
	var scopes string
	var expiresAt, createdAt, updatedAt int64
	if err := dest.Scan(&connection.ID, &connection.UserID, &connection.Email,
		&connection.Subject, &connection.DisplayName, &connection.EncryptedRefreshToken,
		&connection.EncryptedAccessToken, &expiresAt, &scopes, &connection.Status,
		&connection.StatusDetail, &createdAt, &updatedAt); err != nil {
		return MicrosoftConnection{}, err
	}
	connection.GrantedScopes = splitGoogleScopes(scopes)
	connection.AccessTokenExpiresAt = unixTime(expiresAt)
	connection.CreatedAt = unixTime(createdAt)
	connection.UpdatedAt = unixTime(updatedAt)
	return connection, nil
}
