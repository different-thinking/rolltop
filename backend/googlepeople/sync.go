// File overview: The contact sync loop. Google is the leading system for the
// contacts it owns, so this pulls its state down and makes Rolltop match it:
// new people are created, changed people overwritten, deleted people removed.
// The reverse direction -- local edits travelling to Google -- lives in
// writeback.go.

package googlepeople

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/googletoken"
	"rolltop/backend/store"
)

// syncTimeout bounds one connection's sync. A first run over a large address
// book is paged and can genuinely take minutes; anything past this is stuck.
const syncTimeout = 10 * time.Minute

// ErrScopeMissing reports a connection authorized before contact sync existed.
// Its grant still covers mail, so this is a prompt to re-authorize rather than
// a failure of the connection as a whole.
var ErrScopeMissing = errors.New("this Google account has not granted access to contacts")

// Result summarizes one connection's sync for the caller and the log.
type Result struct {
	ConnectionID int64
	Created      int
	Updated      int
	Deleted      int
	// FullSync reports whether this run had to read every contact, which is
	// what happens on the first sync and after Google expires the cursor.
	FullSync bool
}

// Syncer mirrors Google contacts into one Rolltop installation.
type Syncer struct {
	Store  *store.Store
	Blobs  *blob.Store
	Client *Client
	// Tokens mints access tokens and is the same manager mail uses, so a
	// refresh triggered here is shared with an IMAP worker rather than racing it.
	Tokens googletoken.TokenSource
	// Connections lists a user's Google accounts. It is an interface so the
	// sync does not depend on the whole auth manager.
	Connections ConnectionLister
	// ScopeGranted reports whether a connection may talk to the People API.
	// Injected rather than imported so this package does not depend on the
	// OAuth configuration.
	ScopeGranted func(store.GoogleConnection) bool
}

// ConnectionLister is the slice of the Google auth manager the sync needs.
type ConnectionLister interface {
	List(ctx context.Context, userID int64) ([]store.GoogleConnection, error)
	Get(ctx context.Context, userID, connectionID int64) (store.GoogleConnection, error)
}

// NewSyncer wires a syncer for production use. The process builds one in main
// and the web server builds one when it owns its own auth manager; a struct
// literal at each site would let a field added here reach only one of them.
//
// scope is passed in rather than imported so this package stays independent of
// the OAuth configuration, which is what lets its tests grant or withhold
// contact access without a consent flow.
func NewSyncer(db *store.Store, blobs *blob.Store, tokens googletoken.TokenSource, connections ConnectionLister, scope string) *Syncer {
	return &Syncer{
		Store:       db,
		Blobs:       blobs,
		Client:      NewClient(),
		Tokens:      tokens,
		Connections: connections,
		ScopeGranted: func(connection store.GoogleConnection) bool {
			return connection.HasScope(scope)
		},
	}
}

func (s *Syncer) ready() error {
	if s == nil || s.Store == nil || s.Tokens == nil || s.Connections == nil {
		return errors.New("google contact sync is not configured")
	}
	return nil
}

func (s *Syncer) client() *Client {
	if s.Client != nil {
		return s.Client
	}
	s.Client = NewClient()
	return s.Client
}

func (s *Syncer) scopeGranted(connection store.GoogleConnection) bool {
	if s.ScopeGranted == nil {
		return true
	}
	return s.ScopeGranted(connection)
}

// SyncUser syncs every eligible connection of one user and returns the results
// of those that ran. A connection that cannot sync -- no scope, awaiting
// re-consent -- is skipped rather than failing the others.
func (s *Syncer) SyncUser(ctx context.Context, userID int64) ([]Result, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	connections, err := s.Connections.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	var results []Result
	var firstErr error
	for _, connection := range connections {
		if connection.NeedsReauth() || !s.scopeGranted(connection) {
			continue
		}
		result, err := s.SyncConnection(ctx, userID, connection.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		results = append(results, result)
	}
	return results, firstErr
}

// SyncConnection brings one Google account's contacts into Rolltop. The stored
// sync state is written on every outcome, so a failure is visible in settings
// rather than only in the log.
func (s *Syncer) SyncConnection(ctx context.Context, userID, connectionID int64) (Result, error) {
	if err := s.ready(); err != nil {
		return Result{}, err
	}
	connection, err := s.Connections.Get(ctx, userID, connectionID)
	if err != nil {
		return Result{}, err
	}
	if !s.scopeGranted(connection) {
		return Result{}, ErrScopeMissing
	}
	if connection.NeedsReauth() {
		return Result{}, fmt.Errorf("google connection %d needs re-authorization", connectionID)
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	result, err := s.run(ctx, userID, connectionID)
	s.recordOutcome(ctx, userID, connectionID, result, err)
	return result, err
}

// run performs the sync and returns the cursor to store alongside the counts.
func (s *Syncer) run(ctx context.Context, userID, connectionID int64) (Result, error) {
	state, err := s.Store.GetGooglePeopleSync(ctx, userID, connectionID)
	if err != nil {
		return Result{}, err
	}
	result, nextToken, err := s.pull(ctx, userID, connectionID, state.SyncToken)
	recovered := false
	if errors.Is(err, ErrSyncTokenExpired) && state.SyncToken != "" {
		// Google drops a cursor after about a week, and after a password change
		// or a bulk edit. The only recovery is to read everything again, and
		// doing it here rather than waiting for the next poll keeps a stale
		// mirror from surviving a whole interval.
		log.Printf("google contact sync user_id=%d connection_id=%d sync token expired, falling back to a full sync", userID, connectionID)
		result, nextToken, err = s.pull(ctx, userID, connectionID, "")
		recovered = true
	}
	if err != nil {
		return result, err
	}
	// Google is asked for a cursor on every call, so an empty one here means it
	// answered without the field. Storing that would discard a cursor that is
	// still usable and turn the next poll into a full read for nothing.
	//
	// Not after a recovery, though: there the stored cursor is the very token
	// Google just rejected, and putting it back would make the next poll fail
	// the same way and re-read the whole address book on every interval.
	if nextToken == "" && !recovered {
		nextToken = state.SyncToken
	}
	if err := s.Store.SaveGooglePeopleSync(ctx, store.GooglePeopleSync{
		UserID:        userID,
		ConnectionID:  connectionID,
		SyncToken:     nextToken,
		LastSyncAt:    time.Now().UTC(),
		LastSuccessAt: time.Now().UTC(),
		Status:        store.GooglePeopleSyncStatusOK,
	}); err != nil {
		return result, err
	}
	return result, nil
}

// pull walks every page Google offers and applies it. An empty syncToken means
// a full read; the cursor for the next run comes back as the second value.
func (s *Syncer) pull(ctx context.Context, userID, connectionID int64, syncToken string) (Result, string, error) {
	full := strings.TrimSpace(syncToken) == ""
	result := Result{ConnectionID: connectionID, FullSync: full}

	// Only a full read can conclude that a contact Google did not mention is
	// gone; a delta says nothing about the people it omits.
	var known map[string]store.GoogleContactRef
	if full {
		var err error
		known, err = s.Store.ListGoogleContactRefsForConnection(ctx, userID, connectionID)
		if err != nil {
			return result, "", err
		}
	}

	pageToken := ""
	nextSyncToken := ""
	for {
		var page ConnectionsPage
		err := s.withToken(ctx, userID, connectionID, func(token string) error {
			var callErr error
			page, callErr = s.client().ListConnections(ctx, token, ConnectionsRequest{
				SyncToken: syncToken,
				PageToken: pageToken,
			})
			return callErr
		})
		if err != nil {
			return result, "", err
		}
		for _, person := range page.People {
			resourceName := strings.TrimSpace(person.ResourceName)
			if resourceName == "" {
				continue
			}
			delete(known, resourceName)
			if person.IsDeleted() {
				removed, err := s.removeMirror(ctx, userID, connectionID, resourceName)
				if err != nil {
					return result, "", err
				}
				if removed {
					result.Deleted++
				}
				continue
			}
			outcome, err := s.applyPerson(ctx, userID, connectionID, person)
			if err != nil {
				return result, "", err
			}
			switch outcome {
			case outcomeCreated:
				result.Created++
			case outcomeUpdated:
				result.Updated++
			}
		}
		if page.NextSyncToken != "" {
			nextSyncToken = page.NextSyncToken
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}

	// Whatever the full read never mentioned no longer exists at Google.
	for _, ref := range known {
		if err := s.Store.DeleteContactForUser(ctx, userID, ref.ContactID); err != nil && !store.IsNotFound(err) {
			return result, "", err
		}
		result.Deleted++
	}
	return result, nextSyncToken, nil
}

// applyOutcome is what one person did to the local address book. "Unchanged"
// is a distinct answer rather than a quiet update: a full read re-reports every
// person, so counting those as updates would report an untouched address book
// as fully rewritten.
type applyOutcome int

const (
	outcomeUnchanged applyOutcome = iota
	outcomeCreated
	outcomeUpdated
)

// applyPerson creates or overwrites the local mirror of one Google contact.
func (s *Syncer) applyPerson(ctx context.Context, userID, connectionID int64, person Person) (applyOutcome, error) {
	incoming := ToContact(person)
	incoming.GoogleConnectionID = connectionID

	existing, err := s.Store.GetContactByGoogleResourceForUser(ctx, userID, connectionID, incoming.ExternalID)
	switch {
	case err == nil:
		if existing.ETag != "" && existing.ETag == incoming.ETag {
			return outcomeUnchanged, nil
		}
		// A row that is already this person's mirror is replaced outright.
		// Folding the old copy back in would make a deletion at Google
		// unrepresentable here: the address or phone number the user just
		// removed there would come back on every sync, and the next local edit
		// would push it into their Google account again.
		return outcomeUpdated, s.overwrite(ctx, userID, existing, incoming, person, replaceLocal)
	case store.IsNotFound(err):
	default:
		return outcomeUnchanged, err
	}

	// No mirror yet. A contact already in the address book with the same
	// address is the same person, and creating a second row for them is the
	// duplicate this lookup exists to prevent.
	if local, ok, err := s.findLocalMatch(ctx, userID, incoming); err != nil {
		return outcomeUnchanged, err
	} else if ok {
		// Promotion is the one merge that is right: this row was entered here
		// and Google has never seen the details it carries, so dropping them
		// would delete data Google never had a chance to keep.
		return outcomeUpdated, s.overwrite(ctx, userID, local, incoming, person, keepLocalExtras)
	}

	created, err := s.Store.CreateContact(ctx, userID, incoming)
	if err != nil {
		return outcomeUnchanged, err
	}
	s.importPhoto(ctx, userID, created.ID, person)
	return outcomeCreated, nil
}

// mergePolicy says what happens to the version already stored locally.
type mergePolicy int

const (
	// replaceLocal takes Google's copy as the whole truth. It is what an
	// established mirror needs, so a field removed at Google is removed here.
	replaceLocal mergePolicy = iota
	// keepLocalExtras folds details Google does not carry back in. It applies
	// only the first time a locally entered contact is linked to a Google
	// person, when those details exist nowhere else.
	keepLocalExtras
)

// overwrite makes an existing row match Google. The Me flags are always kept
// from the local row: outgoing identities hang off them and Google has no
// opinion about them at all.
func (s *Syncer) overwrite(ctx context.Context, userID int64, existing, incoming store.Contact, person Person, policy mergePolicy) error {
	merged := incoming
	if policy == keepLocalExtras {
		merged = store.MergeContacts(incoming, existing)
	}
	merged.IsMe = existing.IsMe
	merged.IsPrimary = existing.IsPrimary
	if _, err := s.Store.UpdateContact(ctx, userID, existing.ID, merged); err != nil {
		return err
	}
	if err := s.Store.SetContactGoogleLink(ctx, userID, existing.ID, store.ContactGoogleLink{
		ConnectionID:    incoming.GoogleConnectionID,
		ExternalID:      incoming.ExternalID,
		ETag:            incoming.ETag,
		RemoteUpdatedAt: incoming.RemoteUpdatedAt,
	}); err != nil {
		return err
	}
	s.importPhoto(ctx, userID, existing.ID, person)
	return nil
}

// findLocalMatch looks for an existing contact with one of the incoming
// addresses. Only local contacts qualify: a contact already owned by another
// Google account is a separate mirror and must not be stolen from it.
func (s *Syncer) findLocalMatch(ctx context.Context, userID int64, incoming store.Contact) (store.Contact, bool, error) {
	for _, email := range incoming.Emails {
		candidate, err := s.Store.GetContactByEmailForUser(ctx, userID, email.Email)
		if store.IsNotFound(err) {
			continue
		}
		if err != nil {
			return store.Contact{}, false, err
		}
		if candidate.IsGoogleContact() {
			continue
		}
		return candidate, true, nil
	}
	return store.Contact{}, false, nil
}

// removeMirror deletes the local copy of a contact Google reports as deleted.
func (s *Syncer) removeMirror(ctx context.Context, userID, connectionID int64, resourceName string) (bool, error) {
	existing, err := s.Store.GetContactByGoogleResourceForUser(ctx, userID, connectionID, resourceName)
	if store.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.Store.DeleteContactForUser(ctx, userID, existing.ID); err != nil && !store.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

// importPhoto stores Google's contact picture as the contact icon.
//
// Every failure here is logged and swallowed, including the metadata writes: an
// avatar is not worth abandoning a page of contacts whose actual data has
// already been applied, and a sync that stops on one unreadable photo never
// reaches the people after it. A blob whose metadata write failed is deleted
// again rather than left on disk with nothing pointing at it.
func (s *Syncer) importPhoto(ctx context.Context, userID, contactID int64, person Person) {
	if s.Blobs == nil {
		return
	}
	photoURL := PrimaryPhotoURL(person)
	if photoURL == "" {
		return
	}
	data, err := s.client().FetchPhoto(ctx, photoURL)
	if err != nil {
		log.Printf("google contact photo user_id=%d contact_id=%d: %v", userID, contactID, err)
		return
	}
	if len(data) == 0 {
		return
	}
	contentType := detectPhotoType(data)
	if contentType == "" {
		return
	}
	saved, err := s.Blobs.SaveContactIcon(userID, contactID, "google-contact-photo", data)
	if err != nil {
		log.Printf("google contact photo user_id=%d contact_id=%d: %v", userID, contactID, err)
		return
	}
	record, err := s.Store.CreateBlob(ctx, store.BlobRecord{
		UserID: userID,
		Kind:   "contact_icon",
		Path:   saved.Path,
		SHA256: saved.SHA256,
		Size:   saved.Size,
	})
	if err != nil {
		log.Printf("google contact photo user_id=%d contact_id=%d record blob: %v", userID, contactID, err)
		s.discardPhotoFile(userID, saved.Path)
		return
	}
	if _, err := s.Store.SetContactIcon(ctx, userID, contactID, record.ID, contentType, "google-contact-photo", saved.Size); err != nil {
		if !store.IsNotFound(err) {
			log.Printf("google contact photo user_id=%d contact_id=%d set icon: %v", userID, contactID, err)
		}
		// The blob row exists but nothing references it, which is exactly the
		// state the cleanup queue is for.
		if _, _, queueErr := s.Store.QueueBlobCleanupIfUnreferenced(ctx, userID, record.ID); queueErr != nil {
			log.Printf("google contact photo user_id=%d contact_id=%d queue cleanup: %v", userID, contactID, queueErr)
		}
	}
}

// discardPhotoFile removes a saved photo whose metadata never landed. Without a
// blob row the cleanup queue cannot see it, so this is the only chance to take
// the file back.
func (s *Syncer) discardPhotoFile(userID int64, path string) {
	if s.Blobs == nil || path == "" {
		return
	}
	if err := s.Blobs.DeleteUserBlob(userID, path); err != nil {
		log.Printf("google contact photo user_id=%d discard orphan blob: %v", userID, err)
	}
}

// detectPhotoType accepts only the formats the contact icon route accepts, so
// a synced photo cannot end up as something the browser refuses to render.
func detectPhotoType(data []byte) string {
	switch detected := http.DetectContentType(data); {
	case strings.HasPrefix(detected, "image/jpeg"):
		return "image/jpeg"
	case strings.HasPrefix(detected, "image/png"):
		return "image/png"
	case strings.HasPrefix(detected, "image/gif"):
		return "image/gif"
	case strings.HasPrefix(detected, "image/webp"):
		return "image/webp"
	}
	return ""
}

// withToken runs one API call with a valid access token, retrying once against
// a refreshed one when Google rejects it. It shares the policy IMAP and SMTP
// use, so a token this process still believes in cannot fail a sync outright.
func (s *Syncer) withToken(ctx context.Context, userID, connectionID int64, attempt func(token string) error) error {
	return googletoken.WithFreshToken(ctx, s.Tokens, userID, connectionID, func(token string) error {
		err := attempt(token)
		if errors.Is(err, ErrUnauthorized) {
			return googletoken.AuthError{Err: err}
		}
		return err
	})
}

// recordOutcome persists what happened so the settings page can show it. A
// failed sync keeps its cursor: the next attempt should resume the delta rather
// than re-read the whole address book because of one network hiccup.
func (s *Syncer) recordOutcome(ctx context.Context, userID, connectionID int64, result Result, syncErr error) {
	// The sync's own context may already be cancelled by the failure being
	// recorded, and losing the record is what would leave settings claiming a
	// sync that never finished is still fine.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	state, err := s.Store.GetGooglePeopleSync(writeCtx, userID, connectionID)
	if err != nil {
		log.Printf("google contact sync user_id=%d connection_id=%d read state: %v", userID, connectionID, err)
		return
	}
	state.UserID = userID
	state.ConnectionID = connectionID
	state.LastSyncAt = time.Now().UTC()
	if syncErr == nil {
		state.Status = store.GooglePeopleSyncStatusOK
		state.StatusDetail = ""
		state.LastSuccessAt = state.LastSyncAt
		log.Printf("google contact sync user_id=%d connection_id=%d created=%d updated=%d deleted=%d full=%t",
			userID, connectionID, result.Created, result.Updated, result.Deleted, result.FullSync)
	} else {
		state.Status = store.GooglePeopleSyncStatusError
		state.StatusDetail = summarizeSyncError(syncErr)
		log.Printf("google contact sync user_id=%d connection_id=%d failed: %v", userID, connectionID, syncErr)
	}
	if err := s.Store.SaveGooglePeopleSync(writeCtx, state); err != nil {
		log.Printf("google contact sync user_id=%d connection_id=%d save state: %v", userID, connectionID, err)
	}
}

// summarizeSyncError turns a failure into something a user can act on. The
// underlying text can quote the request, which for a write is the contact's own
// data, so only the classification travels into storage and the UI.
func summarizeSyncError(err error) string {
	switch {
	case errors.Is(err, ErrServiceDisabled):
		return "The People API is switched off for the Google Cloud project this connection's OAuth client belongs to. Enable it there; reconnecting the account does not help."
	case errors.Is(err, ErrScopeMissing), errors.Is(err, ErrScopeInsufficient):
		return "This Google account has not granted access to contacts. Reconnect it to allow contact sync."
	case errors.Is(err, ErrForbidden):
		return "Google refused the request. Reconnect the account to grant access to contacts."
	case errors.Is(err, ErrUnauthorized):
		return "Google rejected the sign-in for this account. Reconnect it."
	case errors.Is(err, ErrSyncTokenExpired):
		return "Google discarded the sync cursor. The next sync reads all contacts again."
	case errors.Is(err, context.DeadlineExceeded):
		return "The sync took too long and was stopped. It resumes on the next run."
	case errors.Is(err, ErrUpstream):
		return "Google could not be reached."
	}
	return "The sync failed. See the server log for details."
}
