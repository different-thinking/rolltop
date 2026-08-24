// File overview: The IMAP refusal that means the account has no such folder,
// told apart from a folder that merely could not be read this time.

package syncer

import "errors"

type mailboxGoneError struct {
	err error
}

func (e *mailboxGoneError) Error() string {
	return e.err.Error()
}

func (e *mailboxGoneError) Unwrap() error {
	return e.err
}

// MailboxGone marks a refusal whose reason is that the folder does not exist on
// the account at all, rather than that this attempt to read it failed. The two
// need different answers: an unreadable folder is worth reporting and retrying,
// while a folder the account does not have will be refused identically forever,
// so a run that reports it can never come back green.
func MailboxGone(err error) error {
	if err == nil || IsMailboxGone(err) {
		return err
	}
	return &mailboxGoneError{err: err}
}

// IsMailboxGone reports whether the server refused a folder because it has no
// such folder.
func IsMailboxGone(err error) bool {
	var gone *mailboxGoneError
	return errors.As(err, &gone)
}
