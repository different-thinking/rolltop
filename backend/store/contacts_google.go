// File overview: Provenance for contacts mirrored from Google and the per-
// connection People API sync cursor. Ordinary contact writes live in
// contacts.go; everything that ties a contact to a remote account is here so
// there is one place to look when asking who owns a row.

package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

const (
	// ContactSourceLocal marks a contact this installation owns outright: typed
	// in, imported from a vCard, or captured from a message sender.
	ContactSourceLocal = "local"
	// ContactSourceGoogle marks a mirror of a Google contact. Google is the
	// leading system for these, so a sync overwrites them and local edits are
	// pushed back rather than kept apart.
	ContactSourceGoogle = "google"
)

const (
	// GooglePeopleSyncStatusOK marks a connection whose last sync completed.
	GooglePeopleSyncStatusOK = "ok"
	// GooglePeopleSyncStatusError marks a sync that failed. The token is kept
	// so the next attempt can still be incremental.
	GooglePeopleSyncStatusError = "error"
)

// ContactGoogleLink is the remote identity of one mirrored contact.
type ContactGoogleLink struct {
	ConnectionID int64
	// ExternalID is the People API resource name, e.g. people/c123456789.
	ExternalID string
	// ETag is Google's optimistic-concurrency token for the person. Writing
	// back without the current value either fails at Google or overwrites an
	// edit made elsewhere.
	ETag            string
	RemoteUpdatedAt time.Time
}

// GoogleContactRef is the minimal row a sync needs to decide whether a person
// it just received is new, changed, or already up to date. Loading full
// contacts for that decision would mean four detail queries per row.
type GoogleContactRef struct {
	ContactID  int64
	ExternalID string
	ETag       string
}

// GooglePeopleSync is the sync cursor and outcome for one Google connection.
type GooglePeopleSync struct {
	UserID       int64
	ConnectionID int64
	// SyncToken is Google's delta cursor. It is not a credential -- it grants
	// nothing on its own -- so unlike the tokens in google_connections it is
	// stored as it arrives.
	SyncToken     string
	LastSyncAt    time.Time
	LastSuccessAt time.Time
	Status        string
	StatusDetail  string
}

// IsGoogleContact reports whether Google owns this contact.
func (c Contact) IsGoogleContact() bool {
	return c.Source == ContactSourceGoogle && c.GoogleConnectionID > 0
}

// normalizeContactProvenance keeps the four provenance columns consistent with
// each other. A contact claiming to come from Google without a connection to
// come from would be invisible to the sync and unwritable to Google, so the
// combination is resolved here rather than left to every caller.
func normalizeContactProvenance(source string, connectionID int64, externalID, etag string) (string, int64, string, string) {
	source = strings.ToLower(strings.TrimSpace(source))
	externalID = trimLimit(externalID, 200)
	etag = trimLimit(etag, 200)
	if source != ContactSourceGoogle || connectionID <= 0 {
		return ContactSourceLocal, 0, "", ""
	}
	return ContactSourceGoogle, connectionID, externalID, etag
}

// SetContactGoogleLink records or refreshes which Google person a contact
// mirrors. It is separate from UpdateContact on purpose: an ordinary edit
// through the contact API must not be able to reassign or drop the link.
func (s *Store) SetContactGoogleLink(ctx context.Context, userID, contactID int64, link ContactGoogleLink) error {
	if userID <= 0 || contactID <= 0 || link.ConnectionID <= 0 || strings.TrimSpace(link.ExternalID) == "" {
		return ErrNotFound
	}
	source, connectionID, externalID, etag := normalizeContactProvenance(
		ContactSourceGoogle, link.ConnectionID, link.ExternalID, link.ETag)
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx,
		`UPDATE contacts SET source = ?, google_connection_id = ?, external_id = ?, etag = ?, remote_updated_at = ?, updated_at = ?
			WHERE user_id = ? AND id = ?`,
		source, connectionID, externalID, etag, timeUnix(link.RemoteUpdatedAt), nowUnix(), userID, contactID)
	if err != nil {
		return err
	}
	return requireContactRow(res)
}

// ClearContactGoogleLink demotes one mirrored contact to a local one, keeping
// the data. It is what a user who wants to stop syncing a single person needs,
// and what a delete that only succeeded locally must not do.
func (s *Store) ClearContactGoogleLink(ctx context.Context, userID, contactID int64) error {
	if userID <= 0 || contactID <= 0 {
		return ErrNotFound
	}
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx,
		`UPDATE contacts SET source = ?, google_connection_id = 0, external_id = '', etag = '', remote_updated_at = 0, updated_at = ?
			WHERE user_id = ? AND id = ?`,
		ContactSourceLocal, nowUnix(), userID, contactID)
	if err != nil {
		return err
	}
	return requireContactRow(res)
}

// DemoteGoogleContactsForConnection turns every contact of one connection back
// into a local contact and returns how many were affected.
//
// Disconnecting a Google account deletes contacts nowhere: the user asked to
// stop syncing, not to lose their address book, and Rolltop's copy is often the
// only one they can still reach. The rows keep their data and simply stop being
// mirrors.
func (s *Store) DemoteGoogleContactsForConnection(ctx context.Context, userID, connectionID int64) (int64, error) {
	if userID <= 0 || connectionID <= 0 {
		return 0, nil
	}
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx,
		`UPDATE contacts SET source = ?, google_connection_id = 0, external_id = '', etag = '', remote_updated_at = 0, updated_at = ?
			WHERE user_id = ? AND google_connection_id = ?`,
		ContactSourceLocal, nowUnix(), userID, connectionID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetContactByGoogleResourceForUser finds the local mirror of one Google person.
func (s *Store) GetContactByGoogleResourceForUser(ctx context.Context, userID, connectionID int64, externalID string) (Contact, error) {
	externalID = strings.TrimSpace(externalID)
	if userID <= 0 || connectionID <= 0 || externalID == "" {
		return Contact{}, ErrNotFound
	}
	row := s.mustDataDB(ctx, userID).QueryRowContext(ctx,
		contactSelectSQL()+` WHERE user_id = ? AND google_connection_id = ? AND external_id = ?`,
		userID, connectionID, externalID)
	c, err := scanContact(row)
	if err != nil {
		return Contact{}, err
	}
	if err := s.loadContactDetails(ctx, userID, &c); err != nil {
		return Contact{}, err
	}
	return c, nil
}

// ListGoogleContactRefsForConnection returns every mirrored contact of one
// connection keyed by resource name, which is how a full resync tells apart the
// people Google still returns from the ones it has dropped.
func (s *Store) ListGoogleContactRefsForConnection(ctx context.Context, userID, connectionID int64) (map[string]GoogleContactRef, error) {
	out := map[string]GoogleContactRef{}
	if userID <= 0 || connectionID <= 0 {
		return out, nil
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx,
		`SELECT id, external_id, etag FROM contacts WHERE user_id = ? AND google_connection_id = ? AND external_id <> ''`,
		userID, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref GoogleContactRef
		if err := rows.Scan(&ref.ContactID, &ref.ExternalID, &ref.ETag); err != nil {
			return nil, err
		}
		out[ref.ExternalID] = ref
	}
	return out, rows.Err()
}

// CountGoogleContactsForConnection reports how many contacts one connection
// currently mirrors, for the settings page.
func (s *Store) CountGoogleContactsForConnection(ctx context.Context, userID, connectionID int64) (int, error) {
	if userID <= 0 || connectionID <= 0 {
		return 0, nil
	}
	var count int
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM contacts WHERE user_id = ? AND google_connection_id = ?`,
		userID, connectionID).Scan(&count)
	return count, err
}

// GetGooglePeopleSync loads the sync cursor for one connection. A connection
// that has never synced has no row; that is not an error, it is the state that
// makes the next run a full sync, so the zero value is returned instead.
func (s *Store) GetGooglePeopleSync(ctx context.Context, userID, connectionID int64) (GooglePeopleSync, error) {
	state := GooglePeopleSync{UserID: userID, ConnectionID: connectionID}
	if userID <= 0 || connectionID <= 0 {
		return state, ErrNotFound
	}
	var lastSync, lastSuccess int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx,
		`SELECT sync_token, last_sync_at, last_success_at, status, status_detail
			FROM google_people_sync WHERE user_id = ? AND connection_id = ?`,
		userID, connectionID).
		Scan(&state.SyncToken, &lastSync, &lastSuccess, &state.Status, &state.StatusDetail)
	if err == sql.ErrNoRows {
		return state, nil
	}
	if err != nil {
		return GooglePeopleSync{}, err
	}
	state.LastSyncAt = unixTime(lastSync)
	state.LastSuccessAt = unixTime(lastSuccess)
	return state, nil
}

// ListGooglePeopleSync returns the sync state of every connection of one user.
func (s *Store) ListGooglePeopleSync(ctx context.Context, userID int64) ([]GooglePeopleSync, error) {
	if userID <= 0 {
		return nil, nil
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx,
		`SELECT connection_id, sync_token, last_sync_at, last_success_at, status, status_detail
			FROM google_people_sync WHERE user_id = ? ORDER BY connection_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GooglePeopleSync
	for rows.Next() {
		state := GooglePeopleSync{UserID: userID}
		var lastSync, lastSuccess int64
		if err := rows.Scan(&state.ConnectionID, &state.SyncToken, &lastSync, &lastSuccess, &state.Status, &state.StatusDetail); err != nil {
			return nil, err
		}
		state.LastSyncAt = unixTime(lastSync)
		state.LastSuccessAt = unixTime(lastSuccess)
		out = append(out, state)
	}
	return out, rows.Err()
}

// SaveGooglePeopleSync writes the cursor and outcome of one sync attempt.
func (s *Store) SaveGooglePeopleSync(ctx context.Context, state GooglePeopleSync) error {
	if state.UserID <= 0 || state.ConnectionID <= 0 {
		return ErrNotFound
	}
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, state.UserID).ExecContext(ctx,
		`INSERT INTO google_people_sync
				(user_id, connection_id, sync_token, last_sync_at, last_success_at, status, status_detail, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(user_id, connection_id) DO UPDATE SET
				sync_token = excluded.sync_token,
				last_sync_at = excluded.last_sync_at,
				last_success_at = excluded.last_success_at,
				status = excluded.status,
				status_detail = excluded.status_detail,
				updated_at = excluded.updated_at`,
		state.UserID, state.ConnectionID, state.SyncToken, timeUnix(state.LastSyncAt), timeUnix(state.LastSuccessAt),
		state.Status, trimLimit(state.StatusDetail, 500), ts, ts)
	return err
}

// DeleteGooglePeopleSync drops the cursor for a connection that is going away.
func (s *Store) DeleteGooglePeopleSync(ctx context.Context, userID, connectionID int64) error {
	if userID <= 0 || connectionID <= 0 {
		return nil
	}
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx,
		`DELETE FROM google_people_sync WHERE user_id = ? AND connection_id = ?`, userID, connectionID)
	return err
}

func requireContactRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
