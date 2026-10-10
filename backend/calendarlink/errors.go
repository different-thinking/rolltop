// File overview: The outcomes every calendar provider reports in common.
// Google and Microsoft each keep their own errors -- worded for their own
// service and carrying their own detail -- and each of those unwraps to one of
// these, so the linked-copy logic and the web layer can decide what happened
// without knowing which provider said it.

package calendarlink

import "errors"

var (
	// ErrRemoteChanged reports that the event changed at the provider since it
	// was last read. The local row has been refreshed to the provider's
	// version by the time it is returned.
	ErrRemoteChanged = errors.New("this event was changed at the calendar provider")
	// ErrRemoteDeleted reports that the event was removed at the provider
	// while it was being edited here. The local mirror is gone with it.
	ErrRemoteDeleted = errors.New("this event was deleted at the calendar provider")
	// ErrReadOnlyCalendar reports a write to a calendar the account may only
	// read.
	ErrReadOnlyCalendar = errors.New("this calendar is shared read-only")
	// ErrNotAnInvitation reports an answer to an event the account was not
	// invited to.
	ErrNotAnInvitation = errors.New("this event has no invitation to answer")
	// ErrConflict reports a write the provider refused on a stale version.
	ErrConflict = errors.New("the event changed since it was last read")
	// ErrUnauthorized reports that the provider rejected the access token.
	ErrUnauthorized = errors.New("the calendar provider rejected the access token")
	// ErrForbidden reports a request the grant does not cover.
	ErrForbidden = errors.New("the calendar provider denied the request")
	// ErrScopeMissing reports a connection whose grant does not include
	// calendars at all.
	ErrScopeMissing = errors.New("this account has not granted access to calendars")
	// ErrNotFound reports an event or calendar the provider no longer has.
	ErrNotFound = errors.New("calendar resource not found")
	// ErrUpstream marks a failure of the call itself, as opposed to a local
	// one.
	ErrUpstream = errors.New("the calendar provider request failed")
	// ErrNoOnlineMeeting reports a request for an online meeting in a calendar
	// whose provider offers none there.
	ErrNoOnlineMeeting = errors.New("this calendar cannot hold online meetings")
	// ErrProviderUnavailable reports a calendar whose provider is not wired on
	// this server -- its OAuth client is not configured.
	ErrProviderUnavailable = errors.New("calendar sync for this provider is not available on this server")
)

// providerError keeps a provider's own wording while unwrapping to one of the
// shared outcomes above.
type providerError struct {
	message string
	base    error
}

func (e *providerError) Error() string { return e.message }
func (e *providerError) Unwrap() error { return e.base }

// Sentinel builds a provider's own error value for one of the shared outcomes.
// It is a distinct value -- errors.Is against the provider's sentinel still
// tells the providers apart -- whose chain reaches base.
func Sentinel(message string, base error) error {
	return &providerError{message: message, base: base}
}
