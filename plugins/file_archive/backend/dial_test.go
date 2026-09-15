package main

import (
	"net"
	"testing"
)

// The metadata endpoint is the address an SSRF is worth attempting, and it
// stays refused whether or not the operator allows private hosts.
func TestBlockedIPAlwaysRefusesLinkLocal(t *testing.T) {
	for _, allowPrivate := range []bool{true, false} {
		if !blockedIP(net.ParseIP("169.254.169.254"), allowPrivate) {
			t.Fatalf("cloud metadata address allowed with allowPrivate=%v", allowPrivate)
		}
		if !blockedIP(net.ParseIP("fe80::1"), allowPrivate) {
			t.Fatalf("IPv6 link-local allowed with allowPrivate=%v", allowPrivate)
		}
		if !blockedIP(net.ParseIP("::ffff:169.254.169.254"), allowPrivate) {
			t.Fatalf("IPv4-mapped metadata address allowed with allowPrivate=%v", allowPrivate)
		}
	}
}

// A self-hosted share is normally on the same private network, so the default
// has to allow it -- and the strict setting has to take it away.
func TestBlockedIPFollowsThePrivateHostSetting(t *testing.T) {
	for _, address := range []string{"192.168.1.10", "10.0.0.5", "127.0.0.1", "fd00::1"} {
		ip := net.ParseIP(address)
		if blockedIP(ip, true) {
			t.Fatalf("%s refused by default, which blocks the self-hosted case", address)
		}
		if !blockedIP(ip, false) {
			t.Fatalf("%s allowed with private hosts turned off", address)
		}
	}
	if blockedIP(net.ParseIP("203.0.113.9"), true) == false {
		t.Fatal("a documentation range was dialed")
	}
	if blockedIP(net.ParseIP("93.184.216.34"), true) {
		t.Fatal("an ordinary public address was refused")
	}
}

func TestPrivateHostsAllowedReadsTheOptOut(t *testing.T) {
	t.Setenv("ROLLTOP_FILE_ARCHIVE_ALLOW_PRIVATE_HOSTS", "0")
	if privateHostsAllowed() {
		t.Fatal("the opt-out was ignored")
	}
	t.Setenv("ROLLTOP_FILE_ARCHIVE_ALLOW_PRIVATE_HOSTS", "")
	if !privateHostsAllowed() {
		t.Fatal("private hosts should be allowed unless the operator says otherwise")
	}
}
