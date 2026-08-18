// File overview: Removing credentials from text that may carry a PostgreSQL
// connection string. Every path that reports a database error to a user, a log,
// or a test failure runs through here, because pgx and pgconn echo the DSN from
// several of their own error paths and pgconn's redactPW only covers the
// space-free `password=x` spelling.

package pgdsn

import "regexp"

// Secret spellings that may appear in an error: the libpq keyword form with any
// spacing, quoted or bare, and the userinfo section of a URL DSN.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)password\s*=\s*'[^']*'`),
	regexp.MustCompile(`(?i)password\s*=\s*"[^"]*"`),
	regexp.MustCompile(`(?i)password\s*=\s*[^\s'"]+`),
	regexp.MustCompile(`(?i)\b(pgpassword|passfile)\s*=\s*\S+`),
}

// urlUserinfo matches the credentials section of a URL-style DSN, keeping the
// user name so the message still says which role failed.
var urlUserinfo = regexp.MustCompile(`(?i)(postgres(?:ql)?://[^:/@\s]*):[^@/\s]*@`)

// Redact removes credential material while keeping the rest of the message
// readable — the role name and the actual diagnostic both survive.
func Redact(message string) string {
	message = urlUserinfo.ReplaceAllString(message, "$1:…@")
	for _, pattern := range secretPatterns {
		message = pattern.ReplaceAllString(message, "password=…")
	}
	return message
}
