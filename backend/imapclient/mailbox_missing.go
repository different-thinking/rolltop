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

// Servers that put the folder name between the two halves of the answer --
// "Mailbox [Gmail]/Gesendet doesn't exist" -- say the same thing, and no
// contiguous phrase catches them. Naming what is missing and saying it is not
// there is taken as that answer even when the two are apart.
//
// What may pair up is deliberately narrower than the phrases above: "unknown"
// and "not found" are how servers also report a command, a credential or an
// index file, and those refusals must keep failing loudly. They are recognized
// only where a folder is named in the same breath.
var (
	mailboxMissingSubjects = []string{"mailbox", "folder"}
	mailboxMissingAbsences = []string{"does not exist", "doesn't exist", "no such"}
)

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
	return containsAny(text, mailboxMissingSubjects) && containsAny(text, mailboxMissingAbsences)
}

func containsAny(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}
