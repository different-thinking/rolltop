// File overview: Recognizing the server refusal that means the folder does not exist.

package imapclient

import "strings"

// mailboxMissingPhrases are the ways servers say "there is no such folder".
// RFC 5530 gave that answer a machine-readable form -- the NONEXISTENT response
// code -- but go-imap hands a tagged NO to the caller as errors.New(resp.Info),
// so the code is parsed and dropped before this package can read it. The text is
// therefore all there is, and each phrase below is one server's wording:
// Dovecot's "Mailbox doesn't exist", Gmail's "Unknown Mailbox", and the
// spellings other hosts use for the same answer.
//
// Matching text is a guess where a code would be a fact, so it may only decide
// things that stay safe when the guess is wrong: a missed phrase reports the
// folder as a failure, as before, and a false match skips one folder for one
// turn and drops only a local row that never held a message.
var mailboxMissingPhrases = []string{
	"nonexistent",
	"mailbox doesn't exist",
	"mailbox does not exist",
	"unknown mailbox",
	"no such mailbox",
	"mailbox not found",
	"folder doesn't exist",
	"folder does not exist",
	"no such folder",
}

// mailboxMissing reports whether a failed IMAP command was refused because the
// account has no folder by that name.
func mailboxMissing(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, phrase := range mailboxMissingPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}
