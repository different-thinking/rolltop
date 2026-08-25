package remoteimages

import (
	"net"
	"testing"
)

func TestPrivateIPBlocksInternalAndSpecialPurpose(t *testing.T) {
	blocked := []string{
		"127.0.0.1",       // loopback
		"10.1.2.3",        // RFC1918
		"172.16.0.1",      // RFC1918
		"192.168.1.1",     // RFC1918
		"169.254.10.10",   // link-local
		"0.0.0.0",         // unspecified
		"0.1.2.3",         // "this network" /8
		"100.64.0.1",      // CGNAT
		"100.127.255.254", // CGNAT upper edge
		"192.0.0.1",       // IETF protocol assignments
		"192.0.2.5",       // TEST-NET-1
		"198.18.0.1",      // benchmarking
		"198.51.100.5",    // TEST-NET-2
		"203.0.113.5",     // TEST-NET-3
		"240.0.0.1",       // reserved
		"255.255.255.255", // limited broadcast
		"224.0.0.1",       // multicast
		"::1",             // IPv6 loopback
		"fe80::1",         // IPv6 link-local
		"fc00::1",         // IPv6 ULA
		"::ffff:10.0.0.1", // IPv4-mapped private
		"64:ff9b::7f00:1", // NAT64 wrapping 127.0.0.1
		"2001:db8::1",     // IPv6 documentation
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test address %q did not parse", s)
		}
		if !privateIP(ip) {
			t.Errorf("privateIP(%s) = false, want blocked", s)
		}
	}

	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34", // example.com
		"2606:2800:220:1::1",
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test address %q did not parse", s)
		}
		if privateIP(ip) {
			t.Errorf("privateIP(%s) = true, want allowed", s)
		}
	}
}
