package main

import (
	"context"
	"strings"
	"testing"
)

// There is no in-process SMB server to test against -- go-smb2 is a client
// library only -- so what is checked here is everything that happens before and
// around the wire: the address, the credentials, and the path confinement the
// share is addressed through. The protocol itself is the library's to get right.
func TestSMBStoreRefusesAnAddressWithoutAShare(t *testing.T) {
	// An smb:// address with no share never parses, so it cannot reach the
	// dialer at all.
	if _, err := parseTargetAddress("smb://s0123456.kasserver.com/"); err == nil {
		t.Fatal("an smb address with no share was accepted")
	}
	// A share is still checked at open, because an address can also be built
	// in code rather than parsed.
	address := targetAddress{Transport: transportSMB, Host: "host.example", Port: 445}
	if _, err := newSMBStore(context.Background(), address, "user", "secret"); err == nil {
		t.Fatal("a target with no share was opened")
	} else if !strings.Contains(err.Error(), "share") {
		t.Fatalf("error = %q, want it to name the missing share", err)
	}
}

func TestSplitSMBUserReadsADomainOffTheUserName(t *testing.T) {
	for raw, want := range map[string][2]string{
		`WORKGROUP\robert`: {"WORKGROUP", "robert"},
		`example.org/rob`:  {"example.org", "rob"},
		`s0123456`:         {"", "s0123456"},
		``:                 {"", ""},
	} {
		domain, user := splitSMBUser(raw)
		if domain != want[0] || user != want[1] {
			t.Errorf("splitSMBUser(%q) = %q, %q; want %q, %q", raw, domain, user, want[0], want[1])
		}
	}
}

// Paths are resolved relative to the mounted share, never with a leading slash,
// and nothing a caller passes may climb above the configured root.
func TestSMBStoreResolvesInsideTheShare(t *testing.T) {
	store := &smbStore{root: "recordings"}
	for _, tc := range []struct{ relative, want string }{
		{"", "recordings"},
		{"2026/05/x.m4a", "recordings/2026/05/x.m4a"},
		{"../../etc/passwd", "recordings/etc/passwd"},
		{"/absolute.m4a", "recordings/absolute.m4a"},
	} {
		got := store.resolve(tc.relative)
		if got != tc.want {
			t.Errorf("resolve(%q) = %q, want %q", tc.relative, got, tc.want)
		}
		if strings.HasPrefix(got, "/") || strings.Contains(got, "..") {
			t.Errorf("resolve(%q) = %q, which is not a share-relative path", tc.relative, got)
		}
	}
	// A target configured at the share root still confines its callers.
	bare := &smbStore{}
	if got := bare.resolve("../../secret"); got != "secret" {
		t.Errorf("resolve at the share root = %q", got)
	}
}

// The dial guard is one decision for all three transports.
func TestSMBStoreObeysTheDialGuard(t *testing.T) {
	t.Setenv("ROLLTOP_FILE_ARCHIVE_ALLOW_PRIVATE_HOSTS", "0")
	address, err := parseTargetAddress("smb://127.0.0.1/share")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSMBStore(context.Background(), address, "user", "secret"); err == nil {
		t.Fatal("loopback was dialed with private hosts turned off")
	}
}
