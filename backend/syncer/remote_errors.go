// File overview: Classification of remote IMAP failures for retry policy.

package syncer

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	"rolltop/backend/googletoken"
)

// RemoteErrorKind sorts a remote failure into the retry policy that fits it.
// Every IMAP error used to be handled identically — logged and retried at full
// frequency — which hammered servers that had just refused a credential.
type RemoteErrorKind int

const (
	RemoteErrorNone RemoteErrorKind = iota
	// RemoteErrorAuth is a refused credential. Retrying it fast risks provider
	// lockouts; it needs the user, not another attempt.
	RemoteErrorAuth
	// RemoteErrorTransient is a network-shaped failure: unreachable host,
	// dropped connection, timeout, stall. Worth retrying with backoff.
	RemoteErrorTransient
	// RemoteErrorOther is everything else — protocol-level refusals, local
	// state problems. Retrying may help; backing off still applies.
	RemoteErrorOther
)

func (k RemoteErrorKind) String() string {
	switch k {
	case RemoteErrorNone:
		return "none"
	case RemoteErrorAuth:
		return "auth"
	case RemoteErrorTransient:
		return "transient"
	default:
		return "other"
	}
}

// ClassifyRemoteError decides the retry policy for one remote failure.
func ClassifyRemoteError(err error) RemoteErrorKind {
	if err == nil {
		return RemoteErrorNone
	}
	var authFailure googletoken.AuthError
	if errors.As(err, &authFailure) {
		return RemoteErrorAuth
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return RemoteErrorTransient
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return RemoteErrorTransient
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.ETIMEDOUT) {
		return RemoteErrorTransient
	}
	return RemoteErrorOther
}
