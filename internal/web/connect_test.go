package web

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAdminAddressNameAddress: the address API and the state carry
// name_address only when the supervisor announces a name (Deps.LANName),
// on the same port as the IP address.
func TestAdminAddressNameAddress(t *testing.T) {
	for _, name := range []string{"", "kids-shelf.local"} {
		ts, _, _ := newTestAdmin(t, func(d *Deps) { d.LANName = name })
		port := ts.URL[strings.LastIndex(ts.URL, ":")+1:]
		want := ""
		if name != "" {
			want = "http://kids-shelf.local:" + port + "/"
		}

		resp, out := doReq(t, http.MethodGet, ts.URL+"/api/admin/address", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("LANName %q: address status %d body %v", name, resp.StatusCode, out)
		}
		if got, ok := out["name_address"].(string); !ok || got != want {
			t.Errorf("LANName %q: address name_address = %v, want %q", name, out["name_address"], want)
		}

		resp, out = doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("LANName %q: state status %d body %v", name, resp.StatusCode, out)
		}
		settings, _ := out["settings"].(map[string]any)
		if got, ok := settings["name_address"].(string); !ok || got != want {
			t.Errorf("LANName %q: state name_address = %v, want %q", name, settings["name_address"], want)
		}
	}
}

// TestAdminQREncodesConnect: the QR code carries the IP address with
// ?connect=1, while the address shown as text stays without it.
func TestAdminQREncodesConnect(t *testing.T) {
	ts, _, _ := newTestAdmin(t, func(d *Deps) { d.LANName = "kids-shelf.local" })
	check := func(where, addr, svg string) {
		t.Helper()
		if strings.Contains(addr, "connect") || strings.Contains(addr, "kids-shelf.local") {
			t.Errorf("%s: address = %q, want the plain IP address", where, addr)
		}
		want, err := qrSVG(addr + "?connect=1")
		if err != nil {
			t.Fatalf("%s: qrSVG: %v", where, err)
		}
		if svg != want {
			t.Errorf("%s: qr_svg is not the QR code of %q", where, addr+"?connect=1")
		}
	}

	_, out := doReq(t, http.MethodGet, ts.URL+"/api/admin/address", nil)
	addr, _ := out["address"].(string)
	svg, _ := out["qr_svg"].(string)
	check("address", addr, svg)

	_, out = doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil)
	settings, _ := out["settings"].(map[string]any)
	addr, _ = settings["address"].(string)
	svg, _ = settings["qr_svg"].(string)
	check("state", addr, svg)
}

// connectApp is a parent-page app served in-process with a settable clock,
// so a test can choose each ping's peer, proxy headers and time.
func connectApp(t *testing.T, now *time.Time) http.Handler {
	t.Helper()
	pinThrottle.reset()
	adminSessions.clear()
	connectPings.reset()
	t.Cleanup(connectPings.reset)
	return NewApp("connect-test", Deps{
		DataDir:    t.TempDir(),
		ProxyToken: "proxy-secret",
		Now:        func() time.Time { return *now },
	}).Mux()
}

// TestConnectPing: a ping from a tablet is recorded with its time and
// address, the computer itself never counts, a spoofed proxy header does
// not make a tablet look local, and pings closer than connectPingEvery are
// refused without touching the recorded one.
func TestConnectPing(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	h := connectApp(t, &now)
	w, _ := serveFrom(t, h, http.MethodPost, "/api/admin/pin/setup", "192.0.2.20:5000", map[string]any{"pin": "2468"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: status %d", w.Code)
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == adminPinCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("setup issued no session")
	}
	status := func(wantLast time.Time, wantClient string) {
		t.Helper()
		w, out := serveFrom(t, h, http.MethodGet, "/api/admin/connect/status", "192.0.2.20:5000", nil, nil, session)
		if w.Code != http.StatusOK {
			t.Fatalf("status: %d", w.Code)
		}
		want := ""
		if !wantLast.IsZero() {
			want = wantLast.Format(time.RFC3339Nano)
		}
		if out["last"] != want || out["client"] != wantClient {
			t.Errorf("status = %v, want last %q client %q", out, want, wantClient)
		}
		if want != "" {
			if _, err := time.Parse(time.RFC3339, want); err != nil {
				t.Errorf("last %q is not RFC 3339: %v", want, err)
			}
		}
	}
	ping := func(peer string, hdr map[string]string, wantCode int) {
		t.Helper()
		if w, _ := serveFrom(t, h, http.MethodPost, "/api/connected", peer, nil, hdr); w.Code != wantCode {
			t.Errorf("ping from %s %v: status %d, want %d", peer, hdr, w.Code, wantCode)
		}
	}

	status(time.Time{}, "")

	// The computer itself, directly and through the launcher's proxy.
	ping("127.0.0.1:5000", nil, http.StatusNoContent)
	ping("[::1]:5000", nil, http.StatusNoContent)
	ping("127.0.0.1:5000", viaProxy("127.0.0.1"), http.StatusNoContent)
	status(time.Time{}, "")

	// A tablet on the home network.
	ping("192.0.2.7:5000", nil, http.StatusNoContent)
	status(now, "192.0.2.7")
	first := now

	// Too soon after it: refused, and the recorded ping stays.
	now = now.Add(connectPingEvery / 2)
	ping("127.0.0.1:5000", viaProxy("192.0.2.8"), http.StatusTooManyRequests)
	status(first, "192.0.2.7")

	// A tablet through the proxy, once the gap has passed.
	now = first.Add(connectPingEvery)
	ping("127.0.0.1:5000", viaProxy("192.0.2.8"), http.StatusNoContent)
	status(now, "192.0.2.8")

	// A tablet faking the proxy's "this computer" header is still a tablet.
	now = now.Add(connectPingEvery)
	ping("192.0.2.9:5000", viaProxy("127.0.0.1"), http.StatusNoContent)
	status(now, "192.0.2.9")
}

// TestConnectLedgerCapped: however many devices ping, the ledger keeps
// exactly one entry, the latest recorded one.
func TestConnectLedgerCapped(t *testing.T) {
	var l connectLedger
	start := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	stored := 0
	for i := range 1000 {
		at := start.Add(time.Duration(i) * connectPingEvery / 4)
		if l.record("192.0.2."+strconv.Itoa(i%10), at) {
			stored++
		}
	}
	if stored != 250 {
		t.Errorf("stored %d pings in 250 s of four pings a second, want 250 (one a second)", stored)
	}
	if got := l.latest(); !got.at.Equal(start.Add(996*connectPingEvery/4)) || got.client != "192.0.2.6" {
		t.Errorf("latest = %v %q, want the last recorded ping", got.at, got.client)
	}
}

// TestConnectStatusNeedsPIN: the status endpoint is behind the parent PIN;
// the ping itself is not (the kid page sends it before anyone signs in).
func TestConnectStatusNeedsPIN(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	connectPings.reset()
	t.Cleanup(connectPings.reset)
	if resp, _ := visitorReq(t, http.MethodGet, ts.URL+"/api/admin/connect/status", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status without a session: %d, want 401", resp.StatusCode)
	}
	if resp, out := doReq(t, http.MethodGet, ts.URL+"/api/admin/connect/status", nil); resp.StatusCode != http.StatusOK || out["last"] != "" {
		t.Errorf("status with a session: %d %v, want 200 with no ping yet", resp.StatusCode, out)
	}
	// From the test's own loopback client: accepted, and not recorded.
	if resp, _ := visitorReq(t, http.MethodPost, ts.URL+"/api/connected", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("ping without a session: %d, want 204", resp.StatusCode)
	}
	if _, out := doReq(t, http.MethodGet, ts.URL+"/api/admin/connect/status", nil); out["last"] != "" {
		t.Errorf("a loopback ping was recorded: %v", out)
	}
}
