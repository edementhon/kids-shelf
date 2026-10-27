// The PIN brute-force throttle for the kid unlock, watch-together and
// dashboard PIN checks. Failure state lives per (realm, client IP, account):
// five wrong tries lock that client for 30 s, and every further wrong try
// doubles the lock (60 s, 120 s, ...) up to a quarter of an hour. A correct
// PIN clears only that client's own counters in its own realm, so a kid's
// correct unlock never resets dashboard failures and one device's wrong
// guesses never lock another device out. A per-realm budget (20 wrong tries
// per minute across all IPs) only slows guessing: once it is spent, further
// wrong attempts are refused with 429, but a correct PIN from a client with
// no recent failures of its own still verifies. The throttle is process-wide
// (one shelf serves one household) and a restart forgets every lock.
package web

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// pinThrottleAfter is how many wrong tries a client may make before its
	// (realm, IP, account) lockout starts.
	pinThrottleAfter = 5
	// pinThrottleStart is the first lockout length.
	pinThrottleStart = 30 * time.Second
	// pinThrottleMax caps the doubling lockout.
	pinThrottleMax = 15 * time.Minute
	// pinThrottleShiftCap bounds the doubling exponent before it is shifted,
	// so even an absurd failure count lands on the cap instead of wrapping
	// the duration to zero (or negative).
	pinThrottleShiftCap = 5
	// pinAggregateWindow is how long a wrong try counts toward the per-realm
	// budget, and pinAggregateLimit how many may land in it per realm before
	// further wrong attempts are slowed down.
	pinAggregateWindow = time.Minute
	pinAggregateLimit  = 20
)

// pinRealm names one PIN check family. Each realm keeps its own failures; a
// success in one realm never clears another.
type pinRealm string

const (
	realmKidUnlock     pinRealm = "kid-unlock"
	realmWatchTogether pinRealm = "watch-together"
	realmDashboard     pinRealm = "dashboard"
)

// pinLockDuration returns how long `failures` consecutive wrong tries lock a
// client out: nothing below pinThrottleAfter, then pinThrottleStart doubled
// per further failure, capped at pinThrottleMax. The exponent is bounded
// before shifting, so the duration can never wrap to zero.
func pinLockDuration(failures int) time.Duration {
	if failures < pinThrottleAfter {
		return 0
	}
	n := failures - pinThrottleAfter
	if n > pinThrottleShiftCap {
		n = pinThrottleShiftCap
	}
	d := pinThrottleStart << n
	if d > pinThrottleMax {
		d = pinThrottleMax
	}
	return d
}

// pinClientKey identifies one client's failure counters: the realm, the
// client IP and (where a realm has accounts, as the kid unlock does) the
// account id. Realms without accounts (watch-together, dashboard) use "".
type pinClientKey struct {
	realm   pinRealm
	ip      string
	account string
}

// pinClientState is one client's consecutive-failure history.
type pinClientState struct {
	failures  int
	lockUntil time.Time
}

// pinThrottler is the shared brute-force ledger. Each (realm, IP, account)
// client feeds the lock schedule on its own; the per-realm window counts
// wrong tries across all IPs so a guessing spree can be slowed without ever
// hard-locking every device together.
type pinThrottler struct {
	mu      sync.Mutex
	clients map[pinClientKey]pinClientState
	window  map[pinRealm][]time.Time
}

func newPinThrottler() *pinThrottler {
	return &pinThrottler{
		clients: make(map[pinClientKey]pinClientState),
		window:  make(map[pinRealm][]time.Time),
	}
}

// pinThrottle is the process-wide throttle. Tests reset it between servers
// so one suite's wrong tries never lock another's.
var pinThrottle = newPinThrottler()

// checkLocked reports whether this client may try a PIN now. When locked it
// returns the whole seconds the client must wait.
func (t *pinThrottler) checkLocked(realm pinRealm, ip, account string, now time.Time) (waitSec int, locked bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	until := t.clients[pinClientKey{realm: realm, ip: ip, account: account}].lockUntil
	if !until.After(now) {
		return 0, false
	}
	wait := until.Sub(now)
	return int((wait + time.Second - 1) / time.Second), true
}

// fail records one wrong PIN try from this client and extends its lockout.
// It also enters the attempt into the realm's rolling budget window; callers
// consult aggregateTripped first so a slowed attempt is never recorded.
func (t *pinThrottler) fail(realm pinRealm, ip, account string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := pinClientKey{realm: realm, ip: ip, account: account}
	st := t.clients[k]
	st.failures++
	st.lockUntil = now.Add(pinLockDuration(st.failures))
	t.clients[k] = st
	t.window[realm] = append(t.pruneWindow(t.window[realm], now), now)
}

// aggregateTripped reports whether `realm` has spent its per-minute budget
// of wrong tries across all IPs, and how long until the oldest attempt ages
// out of the window. It only slows guessing: the caller consults it after a
// PIN failed, so a correct PIN from an unladen client still verifies.
func (t *pinThrottler) aggregateTripped(realm pinRealm, now time.Time) (waitSec int, tripped bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.pruneWindow(t.window[realm], now)
	t.window[realm] = w
	if len(w) < pinAggregateLimit {
		return 0, false
	}
	wait := w[0].Add(pinAggregateWindow).Sub(now)
	if wait < time.Second {
		wait = time.Second
	}
	return int((wait + time.Second - 1) / time.Second), true
}

// pruneWindow drops the attempts that fell out of the rolling window.
func (t *pinThrottler) pruneWindow(w []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-pinAggregateWindow)
	i := 0
	for i < len(w) && !w[i].After(cutoff) {
		i++
	}
	if i == 0 {
		return w
	}
	kept := make([]time.Time, len(w)-i)
	copy(kept, w[i:])
	return kept
}

// ok clears this client's counters after a correct PIN. Only that realm,
// IP and account are touched: other devices, other kids and other realms
// keep their state, and the per-realm budget window is left alone.
func (t *pinThrottler) ok(realm pinRealm, ip, account string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.clients, pinClientKey{realm: realm, ip: ip, account: account})
}

// reset forgets every count and lockout; tests call it between servers.
func (t *pinThrottler) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clients = make(map[pinClientKey]pinClientState)
	t.window = make(map[pinRealm][]time.Time)
}

// clientIP returns the request's client address without its port. When the
// request arrived from the launcher's proxy it trusts the X-KS-Client the
// proxy set (the real tablet's address); any other client-supplied value is
// ignored, so the throttle keys on the connection's own peer.
func (s *Server) clientIP(r *http.Request) string {
	if s.trustedProxyPeer(r) {
		if h := r.Header.Get("X-KS-Client"); h != "" {
			return h
		}
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// trustedProxyPeer reports whether this request was really forwarded by the
// launcher's proxy: a configured, non-empty proxy token, a loopback peer
// (the supervisor is the only process that connects from loopback to the
// child's private listeners), and a presented token that matches in
// constant time. Empty equals empty must never enable trusted headers.
func (s *Server) trustedProxyPeer(r *http.Request) bool {
	if s.deps.ProxyToken == "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	presented := r.Header.Get("X-KS-Token")
	return presented != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(s.deps.ProxyToken)) == 1
}

// pinLocked writes the 429 plain-message response and reports true when
// this client is locked out in `realm`: the per-client hard gate. Call it
// right before verifying a PIN.
func (s *Server) pinLocked(w http.ResponseWriter, r *http.Request, realm pinRealm, account string) bool {
	waitSec, locked := pinThrottle.checkLocked(realm, s.clientIP(r), account, s.now())
	if !locked {
		return false
	}
	writeError(w, http.StatusTooManyRequests, tooManyPINsMessage(waitSec))
	return true
}

// pinWrong records one wrong PIN try, unless the realm's per-minute budget
// across all IPs is spent, then it writes the 429 that slows guessing and
// reports true. Call it after a PIN failed to verify, so the budget slows
// brute force without ever refusing a correct PIN from a fresh device.
func (s *Server) pinWrong(w http.ResponseWriter, realm pinRealm, ip, account string) bool {
	if waitSec, tripped := pinThrottle.aggregateTripped(realm, s.now()); tripped {
		writeError(w, http.StatusTooManyRequests, tooManyPINsMessage(waitSec))
		return true
	}
	pinThrottle.fail(realm, ip, account, s.now())
	return false
}

// tooManyPINsMessage is the plain 429 wording every PIN check shares.
func tooManyPINsMessage(waitSec int) string {
	return "Too many wrong PINs. Try again in " + strconv.Itoa(waitSec) + " seconds."
}
