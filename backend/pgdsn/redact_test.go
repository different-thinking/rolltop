package pgdsn

import (
	"strings"
	"testing"
)

// leakCases are messages that must not carry the password out. Each keyword
// spelling and each URL shape below was verified against pgx v5 to be a form it
// echoes back verbatim from a parse error.
var leakCases = []string{
	"failed: host=db password=hunter2 port=5432",
	"failed: host=db password = hunter2 port=5432",
	"failed: host=db PASSWORD  =  hunter2",
	"failed: host=db password='hunter2'",
	"failed: host=db password = 'hunter2'",
	`failed: host=db password="hunter2"`,
	"cannot parse `postgres://rolltop:hunter2@db:5432/x`: bad port",
	"cannot parse `postgresql://rolltop:hunter2@db:5432/x`: bad port",
	"failed: PGPASSWORD=hunter2",
	"TEST_DATABASE_URL must be a URL, got \"host=localhost user=rolltop password=hunter2\"",
	// libpq lets a backslash hide a space in an unquoted value, so the value
	// does not end where the first space is.
	"cannot parse `host=db password=hun\\ ter2 sslmode=bogus`: err",
	// An unencoded '@' or '/' in a URL password is itself what makes the DSN
	// unparseable, which is exactly when pgx quotes the whole thing back.
	"cannot parse `postgres://rolltop:hun@ter2@db:5432/x`: bad port",
	"cannot parse `postgres://rolltop:hun/ter2@db/x`: bad port",
	"cannot parse `postgres://rolltop:hun@ter@2@db:5432/x`: bad port",
}

func TestRedact(t *testing.T) {
	for _, message := range leakCases {
		if got := Redact(message); strings.Contains(got, "hunter2") || strings.Contains(got, "ter2") {
			t.Errorf("Redact(%q) = %q, password survived", message, got)
		}
	}
}

// TestRedactHidesPassfile covers the path to a password rather than the
// password: a passfile name is a pointer to the secret and does not belong in
// a log either.
func TestRedactHidesPassfile(t *testing.T) {
	for _, message := range []string{
		"failed: PGPASSFILE=/root/.pgpass something",
		"cannot parse `host=db passfile=/root/.pgpass`: err",
	} {
		if got := Redact(message); strings.Contains(got, ".pgpass") {
			t.Errorf("Redact(%q) = %q, passfile survived", message, got)
		}
	}
}

// TestRedactKeepsDiagnosis guards the other half of the job: over-redacting
// until the message says nothing makes operators paste the raw DSN somewhere
// worse.
func TestRedactKeepsDiagnosis(t *testing.T) {
	got := Redact("cannot parse `postgres://rolltop:hunter2@db:5432/x`: invalid port")
	for _, want := range []string{"invalid port", "rolltop", "db:5432"} {
		if !strings.Contains(got, want) {
			t.Errorf("Redact removed %q: %s", want, got)
		}
	}
	// The URL pattern must not swallow an '@' belonging to the message rather
	// than the DSN.
	got = Redact("cannot parse `postgres://rolltop:hunter2@db/x`: cannot resolve host@example.com")
	if !strings.Contains(got, "host@example.com") {
		t.Errorf("Redact ate text after the DSN: %s", got)
	}
}

func TestRedactLeavesCleanTextAlone(t *testing.T) {
	const message = "connect: dial tcp 127.0.0.1:5432: connection refused"
	if got := Redact(message); got != message {
		t.Errorf("Redact(%q) = %q", message, got)
	}
}
