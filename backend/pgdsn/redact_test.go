package pgdsn

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []string{
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
	}
	for _, message := range cases {
		if got := Redact(message); strings.Contains(got, "hunter2") {
			t.Errorf("Redact(%q) = %q, password survived", message, got)
		}
	}
	// Redaction must not swallow the useful part of the message.
	got := Redact("cannot parse `postgres://rolltop:hunter2@db:5432/x`: invalid port")
	if !strings.Contains(got, "invalid port") || !strings.Contains(got, "rolltop") {
		t.Errorf("Redact removed diagnostic context: %s", got)
	}
}
