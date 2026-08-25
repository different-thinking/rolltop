// File overview: A small in-process rate-limit gate for unauthenticated
// endpoints (login, password-reset request). It follows the same shape as the
// SMTP connection-test reservation in api_smtp_log.go -- a map keyed by caller
// with an expiring deadline and a bounded sweep -- but adds exponential backoff
// so repeated failures cost progressively more without ever locking an address
// out permanently.

package web

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// backoffGate throttles repeated attempts for a key (typically client IP plus
// the submitted email). A configurable burst passes freely; past it, each
// further failure pushes the next allowed time out by an exponentially growing
// delay, capped at max. A success clears the key. Entries idle past decay are
// swept so the map cannot grow without bound.
type backoffGate struct {
	burst int
	base  time.Duration
	max   time.Duration
	decay time.Duration

	mu    sync.Mutex
	state map[string]*backoffState
}

type backoffState struct {
	failures     int
	blockedUntil time.Time
	lastSeen     time.Time
}

// backoffGateSweep bounds the state map: expired entries are dropped once there
// are more of them than a plausible number of distinct callers in flight.
const backoffGateSweep = 1024

func newBackoffGate(burst int, base, max, decay time.Duration) *backoffGate {
	return &backoffGate{
		burst: burst,
		base:  base,
		max:   max,
		decay: decay,
		state: map[string]*backoffState{},
	}
}

// allow reports whether an attempt for key may proceed now. When it may not, it
// returns the time the caller should wait before retrying. It records nothing:
// the handler decides afterwards whether the attempt failed (recordFailure) or
// succeeded (recordSuccess), so a burst of legitimate logins never accumulates.
func (g *backoffGate) allow(key string, now time.Time) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	st, ok := g.state[key]
	if !ok {
		return true, 0
	}
	st.lastSeen = now
	if now.Before(st.blockedUntil) {
		return false, st.blockedUntil.Sub(now)
	}
	return true, 0
}

// recordFailure charges one failed attempt against key and, once the burst is
// spent, arms the next backoff window.
func (g *backoffGate) recordFailure(key string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state[key]
	if st == nil {
		st = &backoffState{}
		g.state[key] = st
	}
	st.failures++
	st.lastSeen = now
	if st.failures > g.burst {
		st.blockedUntil = now.Add(g.backoffFor(st.failures - g.burst))
	}
}

// recordSuccess forgets a key, so a user who eventually authenticates is not
// held back by earlier typos.
func (g *backoffGate) recordSuccess(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.state, key)
}

// backoffFor returns base*2^(n-1) capped at max, without overflowing the shift.
func (g *backoffGate) backoffFor(n int) time.Duration {
	d := g.base
	for i := 1; i < n; i++ {
		d *= 2
		if d >= g.max {
			return g.max
		}
	}
	if d > g.max {
		return g.max
	}
	return d
}

func (g *backoffGate) sweepLocked(now time.Time) {
	if len(g.state) <= backoffGateSweep {
		return
	}
	for key, st := range g.state {
		if now.Sub(st.lastSeen) >= g.decay && !now.Before(st.blockedUntil) {
			delete(g.state, key)
		}
	}
}

// clientIP returns the network address of the direct peer. It deliberately
// ignores X-Forwarded-For: that header is set by the client and trusting it
// would let an attacker rotate it to sidestep the gate. Behind a reverse proxy
// this collapses to the proxy address, which is why the rate-limit keys also
// include the submitted email -- per-account brute force stays throttled even
// when every request shares one upstream IP.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
