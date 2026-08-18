// File overview: Removing credentials from text that may carry a PostgreSQL
// connection string. Every path that reports a database error to a user, a log,
// or a test failure runs through here, because pgx and pgconn echo the whole
// DSN from their parse-error paths and pgconn's own redactPW only covers the
// space-free `password=x` keyword spelling.
//
// The rule is to over-redact rather than under-redact: a message that loses a
// little context is a nuisance, a message that keeps half a password is a leak.

package pgdsn

import "regexp"

// Secret spellings that may appear in an error.
//
// The keyword pattern accepts a quoted value, or an unquoted one in which
// libpq's backslash escaping may hide a space (`password=hun\ ter2`); stopping
// at that escaped space used to leave the rest of the password in the message.
// The second pattern covers the environment and file spellings, and matches
// `passfile` inside `PGPASSFILE` — a leading \b never fires there, because the
// preceding "PG" is itself a word character.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)password\s*=\s*('[^']*'|"[^"]*"|(?:\\.|[^\s'"])+)`),
	regexp.MustCompile(`(?i)(?:pg)?pass(?:word|file)\s*=\s*\S+`),
}

// urlUserinfo matches the credentials section of a URL-style DSN, keeping the
// user name so the message still says which role failed.
//
// The password part deliberately runs to the last `@` in the token rather than
// the first: an operator who writes an unencoded `@` or `/` in the password
// produces exactly the malformed DSN pgx quotes back in full, and a
// first-`@` match would redact only the head and print the tail. It stops at
// whitespace and at the quote characters pgx wraps the DSN in, so it cannot run
// past the connection string into the rest of the message.
var urlUserinfo = regexp.MustCompile("(?i)(postgres(?:ql)?://[^:/@\\s]*):[^\\s`'\"]*@")

// Redact removes credential material while keeping the rest of the message
// readable — the role name and the actual diagnostic both survive.
func Redact(message string) string {
	message = urlUserinfo.ReplaceAllString(message, "$1:…@")
	for _, pattern := range secretPatterns {
		message = pattern.ReplaceAllString(message, "password=…")
	}
	return message
}
