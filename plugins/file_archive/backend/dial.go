// File overview: The dial guard every transport shares -- which addresses a
// user-supplied host may resolve to, and which it may not.
//
// It lives beside the seam rather than inside one transport because all three
// dial through it: WebDAV over net/http, SFTP and SMB over their own TCP
// connections. The addresses worth refusing are a property of who typed the
// address, not of the protocol that follows.

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const (
	// dialTimeout bounds reaching the host at all: a server that does not answer
	// should fail long before a transport's own request budget is spent.
	dialTimeout = 15 * time.Second
)

// safeDialContext refuses the addresses a storage server is never at but an
// SSRF probe would want to reach. All three transports dial through it.
//
// It is deliberately more permissive than the remote-image fetcher's guard,
// and for a reason that is about what is being addressed rather than about
// risk appetite: a remote image URL is chosen by whoever sent the mail, while
// a target address is typed by the account holder into their own settings --
// and the whole point of this plugin is a share the reader runs themselves,
// which on most installs is on the same private network as Rolltop. Blocking
// RFC1918 would block the intended case.
//
// What stays blocked is what no self-hosted share is ever on and what an SSRF
// is worth attempting: link-local, and above all 169.254.169.254, the cloud
// metadata endpoint that hands out instance credentials. Multicast,
// unspecified, and the IANA special-purpose ranges go with it.
//
// Be clear about what that leaves reachable, because it is the whole cost of
// the decision: by default an account holder can point a target at RFC1918, at
// a ULA, and at loopback -- including services bound to 127.0.0.1 on this very
// host, which are often the ones with no authentication because they assumed
// nobody outside the machine could reach them. The browse and download routes
// return what the target answers, so a configured target is an authenticated
// GET proxy into whatever it can reach. Only the account holder's own targets
// are readable, and only by them, so this is a signed-in user reaching the
// private network rather than an anonymous one -- but on an install where the
// accounts are not all trusted, that is still a capability worth withholding.
//
// An operator in that position sets ROLLTOP_FILE_ARCHIVE_ALLOW_PRIVATE_HOSTS=0,
// which promotes this to the stricter guard: loopback, RFC1918, ULA, shared
// address space and site-local all become undialable, leaving only public
// addresses.
func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	allowPrivate := privateHostsAllowed()
	dialer := &net.Dialer{Timeout: dialTimeout}
	var firstErr error
	for _, ip := range ips {
		if blockedIP(ip.IP, allowPrivate) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, fmt.Errorf("the host %q resolves only to addresses this server will not dial", host)
}

func privateHostsAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ROLLTOP_FILE_ARCHIVE_ALLOW_PRIVATE_HOSTS"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// blockedIPNets are the ranges refused whatever the private-host setting
// says: none of them is somewhere a storage server lives, and the first is the
// one an SSRF is usually aimed at.
var blockedIPNets = parseCIDRs(
	"169.254.0.0/16",  // link-local, including the cloud metadata endpoint
	"0.0.0.0/8",       // "this host on this network"
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"192.88.99.0/24",  // 6to4 relay anycast
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"240.0.0.0/4",     // reserved / future use
	"2002::/16",       // 6to4, which embeds an IPv4 target
	"64:ff9b::/96",    // NAT64, likewise
	"100::/64",        // discard-only
	"2001:db8::/32",   // documentation
)

// privateIPNets are refused only when the operator has turned private
// hosts off. Loopback and the RFC1918/ULA ranges are handled by net.IP's own
// predicates in blockedIP.
var privateIPNets = parseCIDRs(
	"100.64.0.0/10", // shared address space (carrier-grade NAT)
	"fec0::/10",     // deprecated site-local
)

func parseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, parsed, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("file_archive: invalid blocked CIDR " + cidr + ": " + err.Error())
		}
		nets = append(nets, parsed)
	}
	return nets
}

// blockedIP reports whether one resolved address must not be dialed. An
// IPv4-mapped IPv6 address is matched against the IPv4 ranges too, because
// net.IPNet.Contains folds it to its v4 form -- so spelling a blocked v4 as
// ::ffff:a.b.c.d does not slip past.
func blockedIP(ip net.IP, allowPrivate bool) bool {
	if ip == nil {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, blocked := range blockedIPNets {
		if blocked.Contains(ip) {
			return true
		}
	}
	if allowPrivate {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	for _, blocked := range privateIPNets {
		if blocked.Contains(ip) {
			return true
		}
	}
	return false
}
