// File overview: Local contact changes travelling to Google. Google is the
// leading system for the contacts it owns, so every write goes there first and
// the local row is then made to match what Google actually accepted -- never
// the other way round, which would let the two copies disagree the moment a
// write is rejected.

package googlepeople

import (
	"context"
	"errors"
	"fmt"
	"time"

	"rolltop/backend/store"
)

// writeTimeout bounds one write-back. It runs inside a request the user is
// waiting on, so it is far shorter than a sync.
const writeTimeout = 30 * time.Second

// ErrRemoteChanged reports that the contact was edited at Google since Rolltop
// last read it. The local row has been refreshed to Google's version by the
// time this is returned, so the caller can show what the contact looks like now
// instead of leaving the user with a copy that no longer exists anywhere.
var ErrRemoteChanged = errors.New("this contact was changed in Google")

// ErrRemoteDeleted reports that the contact was removed at Google while it was
// being edited here. The local mirror is gone by the time this is returned;
// there is no version left to show, which is what separates it from
// ErrRemoteChanged.
var ErrRemoteDeleted = errors.New("this contact was deleted in Google")

// CreateRemoteContact adds a contact to a Google account and stores the result
// locally. The local row is created from Google's response rather than from the
// submitted values, so the id, etag and any normalization Google applied are
// what Rolltop ends up holding.
func (s *Syncer) CreateRemoteContact(ctx context.Context, userID, connectionID int64, contact store.Contact) (store.Contact, error) {
	if err := s.ready(); err != nil {
		return store.Contact{}, err
	}
	connection, err := s.Connections.Get(ctx, userID, connectionID)
	if err != nil {
		return store.Contact{}, err
	}
	if !s.scopeGranted(connection) {
		return store.Contact{}, ErrScopeMissing
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	payload := FromContact(contact)
	// A create has nothing to be concurrent with, and sending an etag from an
	// unrelated contact is how a copy-paste bug turns into a rejected write.
	payload.ResourceName = ""
	payload.ETag = ""
	var created Person
	if err := s.withToken(ctx, userID, connectionID, func(token string) error {
		var callErr error
		created, callErr = s.client().CreateContact(ctx, token, payload)
		return callErr
	}); err != nil {
		return store.Contact{}, err
	}

	mirrored := ToContact(created)
	mirrored.GoogleConnectionID = connectionID
	// Google returns only what it stored. Anything it does not model -- a
	// category, a label it dropped -- would otherwise be lost on the way in.
	mirrored = store.MergeContacts(mirrored, contact)
	mirrored.IsMe = contact.IsMe
	mirrored.IsPrimary = contact.IsPrimary
	mirrored.Source = store.ContactSourceGoogle
	mirrored.GoogleConnectionID = connectionID
	mirrored.ExternalID = created.ResourceName
	mirrored.ETag = created.ETag

	saved, err := s.Store.CreateContact(ctx, userID, mirrored)
	if err != nil {
		return store.Contact{}, err
	}
	s.importPhoto(ctx, userID, saved.ID, created)
	return s.Store.GetContactForUser(ctx, userID, saved.ID)
}

// UpdateRemoteContact pushes an edit of a mirrored contact and then applies
// what Google accepted to the local row.
//
// On a conflict the local row is replaced with Google's current version and
// ErrRemoteChanged is returned. Discarding the submitted edit is the direct
// consequence of Google being the leading system: keeping it locally would
// produce a contact that disagrees with the account it claims to come from,
// and the next sync would silently undo it anyway.
func (s *Syncer) UpdateRemoteContact(ctx context.Context, userID int64, existing, edited store.Contact) (store.Contact, error) {
	if err := s.ready(); err != nil {
		return store.Contact{}, err
	}
	if !existing.IsGoogleContact() {
		return store.Contact{}, fmt.Errorf("contact %d is not a Google contact", existing.ID)
	}
	connection, err := s.Connections.Get(ctx, userID, existing.GoogleConnectionID)
	if err != nil {
		return store.Contact{}, err
	}
	if !s.scopeGranted(connection) {
		return store.Contact{}, ErrScopeMissing
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	payload := FromContact(edited)
	payload.ResourceName = existing.ExternalID
	payload.ETag = existing.ETag
	connectionID := existing.GoogleConnectionID

	var updated Person
	err = s.withToken(ctx, userID, connectionID, func(token string) error {
		var callErr error
		updated, callErr = s.client().UpdateContact(ctx, token, payload)
		return callErr
	})
	if errors.Is(err, ErrConflict) {
		survived, refreshErr := s.adoptRemote(ctx, userID, connectionID, existing)
		if refreshErr != nil {
			return store.Contact{}, refreshErr
		}
		if !survived {
			// The conflict was a deletion: somebody removed the contact at
			// Google while it was being edited here. There is no version to
			// hand back, and reading the row that adoptRemote just deleted
			// would surface this as a plain "not found" instead.
			return store.Contact{}, ErrRemoteDeleted
		}
		refreshed, getErr := s.Store.GetContactForUser(ctx, userID, existing.ID)
		if getErr != nil {
			return store.Contact{}, getErr
		}
		return refreshed, ErrRemoteChanged
	}
	if err != nil {
		return store.Contact{}, err
	}
	if err := s.applyUpdatedPerson(ctx, userID, connectionID, existing, edited, updated); err != nil {
		return store.Contact{}, err
	}
	return s.Store.GetContactForUser(ctx, userID, existing.ID)
}

// DeleteRemoteContact removes a contact at Google. Removing the local mirror is
// the caller's job, once this has returned without error.
//
// The order is the point: a delete that only succeeded locally would come back
// on the next sync, and a user who confirmed "delete this everywhere" would
// watch the contact reappear. A Google failure therefore leaves both copies
// intact, and the caller must not go on to delete its own.
func (s *Syncer) DeleteRemoteContact(ctx context.Context, userID int64, contact store.Contact) error {
	if err := s.ready(); err != nil {
		return err
	}
	if !contact.IsGoogleContact() {
		return fmt.Errorf("contact %d is not a Google contact", contact.ID)
	}
	connection, err := s.Connections.Get(ctx, userID, contact.GoogleConnectionID)
	if err != nil {
		return err
	}
	if !s.scopeGranted(connection) {
		return ErrScopeMissing
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return s.withToken(ctx, userID, contact.GoogleConnectionID, func(token string) error {
		return s.client().DeleteContact(ctx, token, contact.ExternalID)
	})
}

// applyUpdatedPerson writes Google's accepted version back over the local row.
// The submitted edit is folded in underneath so anything Google does not model
// survives, and the identity flags stay as Rolltop has them.
func (s *Syncer) applyUpdatedPerson(ctx context.Context, userID, connectionID int64, existing, edited store.Contact, updated Person) error {
	mirrored := ToContact(updated)
	mirrored.GoogleConnectionID = connectionID
	merged := store.MergeContacts(mirrored, edited)
	merged.IsMe = existing.IsMe
	merged.IsPrimary = existing.IsPrimary
	if _, err := s.Store.UpdateContact(ctx, userID, existing.ID, merged); err != nil {
		return err
	}
	return s.Store.SetContactGoogleLink(ctx, userID, existing.ID, store.ContactGoogleLink{
		ConnectionID:    connectionID,
		ExternalID:      firstNonEmptyString(updated.ResourceName, existing.ExternalID),
		ETag:            updated.ETag,
		RemoteUpdatedAt: updated.UpdatedAt(),
	})
}

// adoptRemote replaces the local row with Google's current version of the
// contact and reports whether the contact still exists there. A contact Google
// no longer has is deleted here too: it is gone, and leaving a mirror of it
// behind would resurrect it on the next write.
func (s *Syncer) adoptRemote(ctx context.Context, userID, connectionID int64, existing store.Contact) (bool, error) {
	var person Person
	err := s.withToken(ctx, userID, connectionID, func(token string) error {
		var callErr error
		person, callErr = s.client().GetPerson(ctx, token, existing.ExternalID)
		return callErr
	})
	if errors.Is(err, ErrNotFound) {
		deleteErr := s.Store.DeleteContactForUser(ctx, userID, existing.ID)
		if deleteErr != nil && !store.IsNotFound(deleteErr) {
			return false, deleteErr
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	incoming := ToContact(person)
	incoming.GoogleConnectionID = connectionID
	// Google's version is the whole answer here: the local row is exactly the
	// copy its edit was just rejected against.
	return true, s.overwrite(ctx, userID, existing, incoming, person, replaceLocal)
}

func firstNonEmptyString(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}
