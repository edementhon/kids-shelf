package launch

// Tests for the supervisor's reverse proxy (internal/launch/proxy.go):
// trusted-header setting after hop-by-hop stripping (including the
// "X-KS-Token listed in Connection" attack), client-supplied X-KS-* removal,
// Host/Origin/Referer preservation, Range/206/416, HEAD and conditional
// media requests, the starting page while no child is up, a mid-stream
// child death that must abort rather than append that page, and the PIN
// throttle keyed per device through a real proxy.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/web"
)

// startPublicProxy serves proxy on a real loopback listener and returns its
// URL. A real server (not httptest) is used so the stdlib's mid-stream abort
// path (panic http.ErrAbortHandler) behaves like production.
func startPublicProxy(t *testing.T, proxy http.Handler) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := &http.Server{Handler: proxy}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return "http://" + l.Addr().String()
}

// rawGet sends a hand-crafted HTTP/1.1 request over a raw connection, so
// the test controls every header including Connection, and reads the
// response. The caller owns resp.Body's lifecycle.
func rawGet(t *testing.T, baseURL, extraHeaders string) *http.Response {
	t.Helper()
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parsing %s: %v", baseURL, err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dialing %s: %v", u.Host, err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET /x HTTP/1.1\r\nHost: %s\r\n%s\r\n", u.Host, extraHeaders)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestProxySetsTrustedHeadersAfterHopByHopStripping: the trusted headers
// must be the proxy's own values even when the client sends X-KS-* copies
// and lists X-KS-Token in Connection (which the stdlib strips before
// Rewrite runs). Host, Origin and Referer pass through unchanged.
func TestProxySetsTrustedHeadersAfterHopByHopStripping(t *testing.T) {
	var gotClient, gotToken, gotHost, gotOrigin, gotReferer string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClient = r.Header.Get("X-KS-Client")
		gotToken = r.Header.Get("X-KS-Token")
		gotHost = r.Host
		gotOrigin = r.Header.Get("Origin")
		gotReferer = r.Header.Get("Referer")
	}))
	t.Cleanup(backend.Close)

	proxy := newChildProxy("server-token", false)
	if err := proxy.setUp(backend.URL); err != nil {
		t.Fatalf("setUp: %v", err)
	}
	pub := startPublicProxy(t, proxy)
	wantHost, err := url.Parse(pub)
	if err != nil {
		t.Fatal(err)
	}

	resp := rawGet(t, pub, "X-KS-Client: client-fake\r\nX-KS-Token: client-token\r\nConnection: X-KS-Token\r\nOrigin: http://tablet.lan:8070\r\nReferer: http://tablet.lan:8070/libraries\r\n")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotToken != "server-token" {
		t.Errorf("backend X-KS-Token = %q, want the proxy's own %q (a client listing the header in Connection must not strip it)", gotToken, "server-token")
	}
	if gotClient != "127.0.0.1" {
		t.Errorf("backend X-KS-Client = %q, want the real loopback peer, not the client's fake %q", gotClient, "client-fake")
	}
	if gotHost != wantHost.Host {
		t.Errorf("backend Host = %q, want the public Host %q preserved", gotHost, wantHost.Host)
	}
	if gotOrigin != "http://tablet.lan:8070" {
		t.Errorf("backend Origin = %q, want it passed unchanged", gotOrigin)
	}
	if gotReferer != "http://tablet.lan:8070/libraries" {
		t.Errorf("backend Referer = %q, want it passed unchanged", gotReferer)
	}
}

// TestProxyStartingPageWhileChildDown: with no child up the app side answers
// the 503 auto-refresh page and the libraries side a plain 503.
func TestProxyStartingPageWhileChildDown(t *testing.T) {
	appProxy := newChildProxy("tok", false)
	libsProxy := newChildProxy("tok", true)

	resp, err := http.Get(startPublicProxy(t, appProxy) + "/")
	if err != nil {
		t.Fatalf("app port: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("app port status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(body), "kids-shelf is starting") {
		t.Errorf("app port body missing the starting message: %q", body)
	}
	if !strings.Contains(string(body), `http-equiv="refresh" content="2"`) {
		t.Errorf("app port page has no short auto-refresh: %q", body)
	}

	resp, err = http.Get(startPublicProxy(t, libsProxy) + "/kiwix/")
	if err != nil {
		t.Fatalf("libraries port: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("libraries port status = %d, want 503", resp.StatusCode)
	}
	if strings.Contains(string(body), "refresh") {
		t.Errorf("libraries port must be a plain 503, got the refresh page: %q", body)
	}
}

// TestProxyMediaRangeAndConditional: Range/206, unsatisfiable Range/416,
// HEAD and conditional GET pass through the proxy untouched.
func TestProxyMediaRangeAndConditional(t *testing.T) {
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"media-v1"`)
		http.ServeContent(w, r, "movie.mp4", time.Unix(1_700_000_000, 0), strings.NewReader(string(data)))
	}))
	t.Cleanup(backend.Close)

	proxy := newChildProxy("tok", false)
	if err := proxy.setUp(backend.URL); err != nil {
		t.Fatalf("setUp: %v", err)
	}
	pub := startPublicProxy(t, proxy)
	get := func(headers map[string]string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, pub+"/video", nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, body
	}

	resp, body := get(nil)
	if resp.StatusCode != http.StatusOK || string(body) != string(data) {
		t.Fatalf("plain GET = %d %q, want 200 with the full body", resp.StatusCode, body)
	}

	resp, body = get(map[string]string{"Range": "bytes=2-5"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range GET = %d, want 206", resp.StatusCode)
	}
	if string(body) != "2345" {
		t.Errorf("Range body = %q, want %q", body, "2345")
	}

	resp, _ = get(map[string]string{"Range": "bytes=9999-"})
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("oversized Range = %d, want 416", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes */") {
		t.Errorf("416 Content-Range = %q, want bytes */total", cr)
	}

	req, _ := http.NewRequest(http.MethodHead, pub+"/video", nil)
	hresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Errorf("HEAD = %d, want 200", hresp.StatusCode)
	}

	resp, body = get(map[string]string{"If-None-Match": `"media-v1"`})
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Errorf("304 carried a body: %q", body)
	}
}

// TestProxyAbortsWhenChildDiesMidStream: a child that dies in the middle of
// a response must end that response - truncated, with the connection cut -
// and never append the starting page to the partial bytes.
func TestProxyAbortsWhenChildDiesMidStream(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, strings.Repeat("x", 100))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Die like a killed child: cut the connection mid-body.
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
	}))
	t.Cleanup(backend.Close)

	proxy := newChildProxy("tok", false)
	if err := proxy.setUp(backend.URL); err != nil {
		t.Fatalf("setUp: %v", err)
	}
	pub := startPublicProxy(t, proxy)

	resp, err := http.Get(pub + "/video")
	if err != nil {
		// The allowed "connection cut" outcome: the child wrote 100 of its
		// declared 1000 bytes and died, and the proxy aborted before the
		// buffered bytes reached the client, so the request ends with a
		// transport error and no response at all. The starting page was
		// never appended either way.
		return
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "kids-shelf is starting") {
		t.Fatalf("the starting page was appended to a partial response: %q", body)
	}
	if len(body) >= 1000 && rerr == nil {
		t.Fatalf("the full body arrived; the mid-stream death did not abort the response")
	}
}

// TestPinthrottleThroughProxy runs a real web app behind a real proxy: two
// devices (different loopback source addresses, so the proxy sees different
// peers) share the public port, and one device's successful PIN must not
// reset the other's failures. This is the per-device guarantee the
// X-KS-Client identity exists for.
func TestPinthrottleThroughProxy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dialing from 127.0.0.2/127.0.0.3 is not reliable on Windows")
	}
	// macOS has only 127.0.0.1 on its loopback unless an alias is added
	// (macos-smoke runners: "bind: can't assign requested address").
	for _, ip := range []string{"127.0.0.2", "127.0.0.3"} {
		l, err := net.Listen("tcp", ip+":0")
		if err != nil {
			t.Skipf("this machine has no %s loopback address: %v", ip, err)
		}
		l.Close()
	}
	const token = "the-proxy-token"
	dir := t.TempDir()
	pinFile := filepath.Join(dir, "dashboard-pin.json")
	if err := os.WriteFile(pinFile, []byte(`{"pin":"1234"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	app := web.NewApp("proxy-test", web.Deps{DataDir: dir, ProxyToken: token})
	backend := httptest.NewServer(app.Mux())
	t.Cleanup(backend.Close)

	proxy := newChildProxy(token, false)
	if err := proxy.setUp(backend.URL); err != nil {
		t.Fatalf("setUp: %v", err)
	}
	pub := startPublicProxy(t, proxy)
	u, err := url.Parse(pub)
	if err != nil {
		t.Fatal(err)
	}

	clientFrom := func(ip string) *http.Client {
		dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}}
		return &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext}}
	}
	pinCode := func(c *http.Client, pin string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, pub+"/api/admin/pin", strings.NewReader(`{"pin":"`+pin+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://"+u.Host)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("PIN request: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	deviceA, deviceB := clientFrom("127.0.0.2"), clientFrom("127.0.0.3")
	for range 4 {
		if code := pinCode(deviceA, "0000"); code != http.StatusUnauthorized {
			t.Fatalf("device A wrong try = %d, want 401", code)
		}
	}
	// Device B succeeds: its own clean counter clears.
	if code := pinCode(deviceB, "1234"); code != http.StatusOK {
		t.Fatalf("device B correct PIN = %d, want 200", code)
	}
	// Device A's fifth failure still counts...
	if code := pinCode(deviceA, "0000"); code != http.StatusUnauthorized {
		t.Fatalf("device A fifth wrong try = %d, want 401 (B's success must not reset A)", code)
	}
	// ...and the sixth is hard-locked: the per-device identity survived.
	if code := pinCode(deviceA, "0000"); code != http.StatusTooManyRequests {
		t.Fatalf("device A locked-out try = %d, want 429", code)
	}
	// Device B, which never failed, is not locked by A's misses.
	if code := pinCode(deviceB, "0000"); code != http.StatusUnauthorized {
		t.Fatalf("device B after A's lock = %d, want 401 (A's failures must not leak to B)", code)
	}
}

// TestProxyTokenIsRandomAndNonEmpty guards the KS_PROXY_TOKEN contract.
func TestProxyTokenIsRandomAndNonEmpty(t *testing.T) {
	a, err := proxyToken()
	if err != nil {
		t.Fatalf("proxyToken: %v", err)
	}
	b, err := proxyToken()
	if err != nil {
		t.Fatalf("proxyToken: %v", err)
	}
	if a == "" || b == "" {
		t.Fatal("proxyToken returned an empty token")
	}
	if a == b {
		t.Fatal("proxyToken returned the same value twice")
	}
}

// TestPeerIPStripsPort keeps peerIP's contract: the client address never
// carries the ephemeral source port.
func TestPeerIPStripsPort(t *testing.T) {
	if got := peerIP("192.0.2.5:54321"); got != "192.0.2.5" {
		t.Errorf("peerIP(%q) = %q", "192.0.2.5:54321", got)
	}
	if got := peerIP("[::1]:54321"); got != "::1" {
		t.Errorf("peerIP(%q) = %q", "[::1]:54321", got)
	}
	if got := peerIP("bogus"); got != "bogus" {
		t.Errorf("peerIP(%q) = %q", "bogus", got)
	}
}

// TestServeChildBindFailureIsErrBindFailed: the child's app listener
// failing to bind must surface ErrBindFailed so main.go can exit with
// ExitBindFailed.
func TestServeChildBindFailureIsErrBindFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	err = ServeChild(ctx, busy.Addr().String(), "", http.NotFoundHandler(), nil)
	if !errors.Is(err, ErrBindFailed) {
		t.Fatalf("ServeChild on a busy address = %v, want ErrBindFailed", err)
	}
}

// TestProxyChildUpDownTransitions: after setDown the starting page returns.
func TestProxyChildUpDownTransitions(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	proxy := newChildProxy("tok", false)
	if err := proxy.setUp(backend.URL); err != nil {
		t.Fatalf("setUp: %v", err)
	}
	pub := startPublicProxy(t, proxy)
	resp, err := http.Get(pub + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status while up = %d, want 200", resp.StatusCode)
	}
	proxy.setDown()
	resp, err = http.Get(pub + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status after setDown = %d, want 503", resp.StatusCode)
	}
}
