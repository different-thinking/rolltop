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

// secretValue matches a conninfo value in any of the three shapes libpq
// accepts. A backslash escapes any character in all of them, so each branch
// consumes `\x` as one unit: `password=hun\ ter2` does not end at the space,
// and `password='hun\' ter2'` does not end at the first quote. Both left the
// tail of the password in the message before the escape alternatives existed.
const secretValue = `('(?:\\.|[^'\\])*'|"(?:\\.|[^"\\])*"|(?:\\.|[^\s'"])+)`

// secretPattern matches any key whose name begins with "pass", optionally
// prefixed with "pg".
//
// The family is matched rather than enumerated on purpose. libpq itself uses
// `password` and `passfile`, the environment adds `PGPASSWORD` and
// `PGPASSFILE`, and pgx accepts unknown keywords without complaint — so an
// operator who writes `pass=` or `passwd=` gets a DSN that still carries a
// secret and still reaches this function through the paths that echo a
// rejected connection string. Enumerating spellings means the next one leaks.
//
// The leading \b sits before the optional "pg", which is what makes
// `PGPASSFILE` match: anchoring after the prefix cannot work, because "PG" is
// itself a word character. It also keeps unrelated words like `bypass=` out.
var secretPattern = regexp.MustCompile(`(?i)\b(?:pg)?pass[\w-]*\s*=\s*` + secretValue)

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
	return secretPattern.ReplaceAllString(message, "password=…")
}
