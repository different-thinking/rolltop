// File overview: What a connection string is allowed to look like.

package pgdsn

import (
	"strings"
	"testing"
)

// TestValidateReportsASocketHostInTheDescription is the startup line that
// catches a DSN whose host was lost: it names the socket directory rather than
// the server that was meant, in the first lines of the container log.
func TestValidateReportsASocketHostInTheDescription(t *testing.T) {
	const dsn = "dbname=rolltop"
	if err := Validate(dsn); err != nil {
		t.Fatalf("Validate(%s) = %v; pgconn fills in a default host, so this parses", dsn, err)
	}
	got := Describe(dsn)
	if !strings.Contains(got, "/") {
		t.Fatalf("Describe(%s) = %q, want it to show the socket path it will actually use", dsn, got)
	}
}

// TestValidateAcceptsTheKeywordForm is what compose.yml relies on. It builds a
// DSN by substituting a generated password verbatim, and the keyword form is
// the one that needs no percent-encoding — in a URL a password containing `@`,
// `/`, `?`, or `#` silently reparses as a different host, database, or option.
func TestValidateAcceptsTheKeywordForm(t *testing.T) {
	for _, dsn := range []string{
		`host=db port=5432 user=rolltop password='p@ss/wo?rd#1' dbname=rolltop sslmode=disable`,
		`host=db.example port=5432 user=rolltop password='abc+def/gh=' dbname=rolltop sslmode=require`,
	} {
		if err := Validate(dsn); err != nil {
			t.Errorf("Validate(%s) = %v", dsn, err)
			continue
		}
		if got := Describe(dsn); !strings.HasPrefix(got, "rolltop@db") || strings.Contains(got, "ss") && strings.Contains(got, "wo") {
			t.Errorf("Describe(%s) = %q", dsn, got)
		}
	}
}

// TestValidateRejectsWhatItCanCatch pins the checks that exist because pgconn
// accepts these and then connects somewhere else.
//
// A missing host is deliberately not among them: pgconn resolves it to the
// local socket directory, so nothing empty ever reaches Validate. See the
// comment on Validate for what covers that case instead.
func TestValidateRejectsWhatItCanCatch(t *testing.T) {
	for _, tc := range []struct{ name, dsn, want string }{
		{"empty", "", "is empty"},
		{"no database", "postgres://user:pw@host:5432", "names no database"},
	} {
		err := Validate(tc.dsn)
		if err == nil {
			t.Errorf("Validate(%s) accepted it", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%s) = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

// TestValidateNeverQuotesTheDSN keeps the password out of a startup error.
// pgconn's own parse errors quote the whole string back.
func TestValidateNeverQuotesTheDSN(t *testing.T) {
	err := Validate(`postgres://user:sup3rs3cret@host:notaport/rolltop`)
	if err == nil {
		t.Fatal("a malformed port was accepted")
	}
	if strings.Contains(err.Error(), "sup3rs3cret") {
		t.Fatalf("the error carries the password: %v", err)
	}
}
