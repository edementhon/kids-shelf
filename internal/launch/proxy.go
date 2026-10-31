// The supervisor's reverse proxy: one childProxy per public listener. While
// the child is up, each proxy forwards its public port to the child's
// matching private loopback port, presenting the client's real address in
// X-KS-Client plus the proxy token (the app trusts that pair only from a
// loopback peer). While no child is up both answer a plain 503 so tablets
// get a "starting" page instead of connection errors, and a child that dies
// mid-stream ends the response rather than appending that page (the stdlib
// aborts the connection, see http.ErrAbortHandler).
package launch

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
)

// startingRefreshSeconds is how often the app port's starting page reloads
// itself while the child is not up.
const startingRefreshSeconds = 2

// startingPage is the plain 503 the public app port answers while no child
// answers health: the browser retries on its own through the short refresh.
var startingPage = fmt.Sprintf("<!doctype html><html><head><meta charset=\"utf-8\"><title>Kids Shelf</title><meta http-equiv=\"refresh\" content=\"%d\"></head><body>Kids Shelf is starting, one moment</body></html>\n", startingRefreshSeconds)

// startingNotice is the plain one-line 503 the libraries port answers while
// no child is up: no refresh, nothing for a ZIM script to hook.
const startingNotice = "Kids Shelf is starting, one moment\n"

// childProxy is one side of the supervisor's public listener: it forwards
// to the child's matching private loopback port while the child is up and
// answers the starting 503 while it is not. plain selects the libraries
// side (the response without the auto-refresh page).
type childProxy struct {
	token string
	plain bool // libraries side: no auto-refresh page

	mu    sync.RWMutex
	proxy *httputil.ReverseProxy // nil while no child is up
}

// newChildProxy returns a proxy that presents token to the child alongside
// the client address.
func newChildProxy(token string, plain bool) *childProxy {
	return &childProxy{token: token, plain: plain}
}

// setUp points the proxy at the child's private loopback base
// ("http://127.0.0.1:<port>"). It is called only once the child's own
// /healthz has answered on its private port, so a supervisor-generated
// response can never confirm a trial or a join.
func (p *childProxy) setUp(base string) error {
	target, err := url.Parse(base)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return fmt.Errorf("launch: the child's private base URL %q is not usable", base)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.proxy = p.build(target)
	return nil
}

// setDown makes the proxy answer the starting page again: the child exited
// or is being restarted.
func (p *childProxy) setDown() {
	p.mu.Lock()
	p.proxy = nil
	p.mu.Unlock()
}

// build returns the forwarding proxy for one child. The trusted headers are
// set in Rewrite, which the stdlib runs after its hop-by-hop stripping -
// including headers the client named in Connection - so a client listing
// X-KS-Token in Connection cannot have it removed afterwards. Any
// client-supplied X-KS-* copies are deleted first, and the public Host is
// preserved (the app's Host allowlist and same-site checks are built on
// it); Origin and Referer pass through untouched.
func (p *childProxy) build(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host // keep the public Host, per PLAN 1a
			pr.Out.Header.Del("X-KS-Client")
			pr.Out.Header.Del("X-KS-Token")
			pr.Out.Header.Set("X-KS-Client", peerIP(pr.In.RemoteAddr))
			pr.Out.Header.Set("X-KS-Token", p.token)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			// Only failures before the response started reach here (the
			// stdlib aborts mid-stream responses itself): show the
			// starting page. If something already went out (a 1xx
			// interchange), abort rather than append the page to it.
			if pw, ok := w.(*proxyResponseWriter); ok && pw.wrote() {
				panic(http.ErrAbortHandler)
			}
			p.starting(w)
		},
	}
}

// ServeHTTP forwards the request while the child is up, or answers the
// starting page while it is not.
func (p *childProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	proxy := p.proxy
	p.mu.RUnlock()
	if proxy == nil {
		p.starting(w)
		return
	}
	proxy.ServeHTTP(&proxyResponseWriter{ResponseWriter: w}, r)
}

// starting writes this side's 503 starting response.
func (p *childProxy) starting(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	if p.plain {
		io.WriteString(w, startingNotice)
		return
	}
	io.WriteString(w, startingPage)
}

// proxyResponseWriter tracks whether any bytes reached the client so the
// error handler can tell a not-yet-started failure from a mid-stream one,
// and forwards Flush so streamed media keeps flowing.
type proxyResponseWriter struct {
	http.ResponseWriter
	started atomic.Bool
}

func (w *proxyResponseWriter) WriteHeader(code int) {
	w.started.Store(true)
	w.ResponseWriter.WriteHeader(code)
}

func (w *proxyResponseWriter) Write(b []byte) (int, error) {
	w.started.Store(true)
	return w.ResponseWriter.Write(b)
}

func (w *proxyResponseWriter) Flush() {
	w.started.Store(true)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *proxyResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *proxyResponseWriter) wrote() bool                 { return w.started.Load() }

// peerIP returns the request's peer address without its port: the client
// address the child must see.
func peerIP(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return remoteAddr
}

// proxyToken returns the random non-empty secret the proxied requests carry
// (KS_PROXY_TOKEN on the child).
func proxyToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating the proxy token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
