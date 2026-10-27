package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPinLockDurationCapsBeforeOverflow(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{pinThrottleAfter - 1, 0},
		{pinThrottleAfter, 30 * time.Second},
		{pinThrottleAfter + 1, time.Minute},
		{pinThrottleAfter + 2, 2 * time.Minute},
		{pinThrottleAfter + 3, 4 * time.Minute},
		{pinThrottleAfter + 4, 8 * time.Minute},
		{pinThrottleAfter + 5, pinThrottleMax}, // 30 s << 5 = 16 min, capped
		{10, pinThrottleMax},
		{200, pinThrottleMax}, // the pre-fix shift wrapped this to 0
		{1 << 20, pinThrottleMax},
	}
	for _, tc := range tests {
		if got := pinLockDuration(tc.failures); got != tc.want {
			t.Errorf("pinLockDuration(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

// TestKidSuccessDoesNotResetDashboardCounter records dashboard failures,
// lets a kid's correct unlock succeed in between, and asserts the dashboard
// counter is untouched: a success clears only its own realm.
func TestKidSuccessDoesNotResetDashboardCounter(t *testing.T) {
	th := newPinThrottler()
	now := fixedNow()
	ip := "192.0.2.10"

	for range pinThrottleAfter - 1 {
		th.fail(realmDashboard, ip, "", now)
	}
	// A kid's correct PIN in the kid-unlock realm, between dashboard guesses.
	th.ok(realmKidUnlock, ip, "kid-ada")
	th.fail(realmDashboard, ip, "", now) // the fifth dashboard failure
	if waitSec, locked := th.checkLocked(realmDashboard, ip, "", now); !locked {
		t.Fatal("dashboard counter was reset by a kid-unlock success")
	} else if waitSec != 30 {
		t.Errorf("dashboard wait = %ds, want 30s", waitSec)
	}
	// The dashboard guesses never leaked into the kid realm either.
	if _, locked := th.checkLocked(realmKidUnlock, ip, "kid-ada", now); locked {
		t.Error("dashboard failures locked the kid-unlock realm")
	}
}

// TestThrottleStateIsPerRealmIPAndAccount checks that one client's failures
// never bleed into another realm, another IP or another kid's account.
func TestThrottleStateIsPerRealmIPAndAccount(t *testing.T) {
	th := newPinThrottler()
	now := fixedNow()
	ada, bob := "kid-ada", "kid-bob"

	for range pinThrottleAfter {
		th.fail(realmKidUnlock, "198.51.100.7", ada, now)
	}
	// Same IP, different kid: not locked (per-account state).
	if _, locked := th.checkLocked(realmKidUnlock, "198.51.100.7", bob, now); locked {
		t.Error("kid B on the same IP is locked by kid A's failures")
	}
	// Same kid, different IP: not locked (per-IP state).
	if _, locked := th.checkLocked(realmKidUnlock, "198.51.100.8", ada, now); locked {
		t.Error("a second device is locked by the first device's failures")
	}
	// Different realm, same IP: not locked (per-realm state).
	if _, locked := th.checkLocked(realmDashboard, "198.51.100.7", "", now); locked {
		t.Error("dashboard is locked by kid-unlock failures")
	}
	if _, locked := th.checkLocked(realmWatchTogether, "198.51.100.7", "", now); locked {
		t.Error("watch-together is locked by kid-unlock failures")
	}
	// Clearing kid A's counters leaves kid B and the other realms alone.
	th.ok(realmKidUnlock, "198.51.100.7", ada)
	if _, locked := th.checkLocked(realmKidUnlock, "198.51.100.7", ada, now); locked {
		t.Error("ok did not clear the client's own counters")
	}
	if _, locked := th.checkLocked(realmKidUnlock, "198.51.100.7", bob, now); locked {
		t.Error("ok cleared another kid's counters")
	}
}

// TestManyFailuresKeepFifteenMinuteLockout pumps 200 consecutive wrong
// tries from one client and asserts the lockout stays clamped at the
// 15-minute cap instead of wrapping to zero (and unlocking).
func TestManyFailuresKeepFifteenMinuteLockout(t *testing.T) {
	th := newPinThrottler()
	now := fixedNow()
	ip, ada := "203.0.113.9", "kid-ada"

	for range 200 {
		th.fail(realmKidUnlock, ip, ada, now)
	}
	waitSec, locked := th.checkLocked(realmKidUnlock, ip, ada, now)
	if !locked {
		t.Fatal("200 failures left the client unlocked — the lockout wrapped")
	}
	if want := int(pinThrottleMax / time.Second); waitSec != want {
		t.Errorf("wait after 200 failures = %ds, want the %ds cap", waitSec, want)
	}
	// The lockout is real: the client stays locked right up to the cap and
	// unlocks the moment it passes.
	if _, locked := th.checkLocked(realmKidUnlock, ip, ada, now.Add(pinThrottleMax-1)); !locked {
		t.Error("client unlocked before the 15-minute cap elapsed")
	}
	if _, locked := th.checkLocked(realmKidUnlock, ip, ada, now.Add(pinThrottleMax)); locked {
		t.Error("client still locked after the 15-minute cap elapsed")
	}
}

// TestRealmAggregateSlowsGuessing verifies the per-realm per-minute budget:
// once twenty wrong tries land in the rolling window across all IPs, the
// realm reports tripped (further wrong attempts get 429), a fresh client is
// not hard-locked, other realms keep their own budgets, and the window rolls
// over after a minute.
func TestRealmAggregateSlowsGuessing(t *testing.T) {
	th := newPinThrottler()
	now := fixedNow()
	ada := "kid-ada"

	for i := range pinAggregateLimit {
		th.fail(realmKidUnlock, "192.0.2."+strconv.Itoa(i+1), ada, now)
	}
	if waitSec, tripped := th.aggregateTripped(realmKidUnlock, now); !tripped {
		t.Fatal("realm budget not spent after pinAggregateLimit failures")
	} else if waitSec < 1 {
		t.Errorf("slow-down wait = %ds, want at least 1", waitSec)
	}
	// The budget only slows guessing: a fresh client is not hard-locked.
	if _, locked := th.checkLocked(realmKidUnlock, "192.0.2.250", ada, now); locked {
		t.Error("realm budget hard-locked a fresh client")
	}
	// Other realms keep their own budgets.
	if _, tripped := th.aggregateTripped(realmDashboard, now); tripped {
		t.Error("dashboard realm slowed by kid-unlock failures")
	}
	// The window rolls: a minute later the budget is spent no longer.
	if _, tripped := th.aggregateTripped(realmKidUnlock, now.Add(pinAggregateWindow)); tripped {
		t.Error("realm still slowed after the window rolled")
	}
}

// TestClientIPRequiresTrustedProxy: X-KS-Client is honored only from a
// loopback peer presenting the configured, non-empty token - and empty
// equals empty must never enable it in dev.
func TestClientIPRequiresTrustedProxy(t *testing.T) {
	token := "the-proxy-token"
	s := NewApp("trust-test", Deps{ProxyToken: token})
	req := func(peer, header string, presented ...string) *http.Request {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/kid/unlock", nil)
		r.RemoteAddr = peer + ":54321"
		if header != "" {
			r.Header.Set("X-KS-Client", header)
		}
		if len(presented) > 0 {
			r.Header.Set("X-KS-Token", presented[0])
		}
		return r
	}
	if got := s.clientIP(req("127.0.0.1", "192.0.2.10", token)); got != "192.0.2.10" {
		t.Fatalf("trusted loopback peer + matching token: clientIP = %q, want 192.0.2.10", got)
	}
	if got := s.clientIP(req("127.0.0.1", "192.0.2.10", "wrong")); got != "127.0.0.1" {
		t.Fatalf("mismatched token: clientIP = %q, want the peer", got)
	}
	if got := s.clientIP(req("127.0.0.1", "192.0.2.10")); got != "127.0.0.1" {
		t.Fatalf("no token presented: clientIP = %q, want the peer", got)
	}
	if got := s.clientIP(req("10.0.0.9", "192.0.2.10", token)); got != "10.0.0.9" {
		t.Fatalf("non-loopback peer with the token: clientIP = %q, want the peer", got)
	}
	open := NewApp("trust-test", Deps{})
	if got := open.clientIP(req("127.0.0.1", "192.0.2.10", token)); got != "127.0.0.1" {
		t.Fatalf("no proxy token configured: clientIP = %q, want the peer", got)
	}
}

// TestThrottleKeepsDevicesSeparateThroughProxyHeaders: the proxy presents
// each device's real address in X-KS-Client; device A's failures must not
// be reset by device B's correct PIN, and A's lock must not leak to B.
func TestThrottleKeepsDevicesSeparateThroughProxyHeaders(t *testing.T) {
	const token = "the-proxy-token"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dashboard-pin.json"), []byte(`{"pin":"1234"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewApp("throttle-test", Deps{DataDir: dir, ProxyToken: token})
	pinThrottle.reset()
	t.Cleanup(pinThrottle.reset)

	pin := func(device, value string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/admin/pin", strings.NewReader(`{"pin":"`+value+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "127.0.0.1:54321" // the proxy's loopback connection
		r.Header.Set("X-KS-Client", device)
		r.Header.Set("X-KS-Token", token)
		rec := httptest.NewRecorder()
		s.handleAdminPIN(rec, r)
		return rec.Code
	}

	for range pinThrottleAfter - 1 {
		if code := pin("192.0.2.10", "0000"); code != http.StatusUnauthorized {
			t.Fatalf("device A wrong try = %d, want 401", code)
		}
	}
	// Device B's correct PIN succeeds; A's counter is untouched.
	if code := pin("192.0.2.11", "1234"); code != http.StatusOK {
		t.Fatalf("device B correct PIN = %d, want 200", code)
	}
	if code := pin("192.0.2.10", "0000"); code != http.StatusUnauthorized {
		t.Fatalf("device A fifth wrong try = %d, want 401 (B's success must not reset A)", code)
	}
	if code := pin("192.0.2.10", "0000"); code != http.StatusTooManyRequests {
		t.Fatalf("device A locked-out try = %d, want 429", code)
	}
	if code := pin("192.0.2.11", "0000"); code != http.StatusUnauthorized {
		t.Fatalf("device B after A's lock = %d, want 401 (A's lock must not leak)", code)
	}
}

// TestThrottleIgnoresUntrustedXKSClient: a client that fakes X-KS-Client
// without the proxy token is counted against its real address.
func TestThrottleIgnoresUntrustedXKSClient(t *testing.T) {
	const token = "the-proxy-token"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dashboard-pin.json"), []byte(`{"pin":"1234"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewApp("throttle-test", Deps{DataDir: dir, ProxyToken: token})
	pinThrottle.reset()
	t.Cleanup(pinThrottle.reset)

	pin := func(fakeIP string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/admin/pin", strings.NewReader(`{"pin":"0000"}`))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "127.0.0.1:54321" // the real peer
		if fakeIP != "" {
			r.Header.Set("X-KS-Client", fakeIP)
		}
		rec := httptest.NewRecorder()
		s.handleAdminPIN(rec, r)
		return rec.Code
	}

	// Without the token the fake client address is ignored and all five
	// failures key on the real loopback peer.
	for range pinThrottleAfter {
		if code := pin("6.6.6.6"); code != http.StatusUnauthorized {
			t.Fatalf("untrusted wrong try = %d, want 401", code)
		}
	}
	if code := pin("6.6.6.6"); code != http.StatusTooManyRequests {
		t.Fatalf("untrusted client after 5 misses = %d, want 429 on the real peer", code)
	}
}
