package kiwix

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"syscall"
	"time"
)

// maxRedirects is the most redirects the catalog, meta4 and mirror HTTP
// clients follow.
const maxRedirects = 5

// errAddressBlocked is the dial-time rejection of a destination outside the
// allowed (public unicast) address space: loopback, RFC 1918 / fc00::/7
// private ranges, link-local, the 100.64.0.0/10 CGNAT range, multicast and
// unspecified addresses.
var errAddressBlocked = errors.New("kiwix: refusing to connect to a non-public address")

// restrictedDialer returns a Dialer whose Control enforces the outbound
// destination policy. The check runs on the resolved IP address at dial
// time, so a hostname that resolves to a banned range - DNS tricks
// included, and every hop of a redirect - is refused before a connection
// is attempted. allowLoopback permits loopback destinations for tests that
// point the client at an httptest server on 127.0.0.1; everything else is
// refused unconditionally.
func restrictedDialer(allowLoopback bool) *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%w: %s", errAddressBlocked, address)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("%w: %s", errAddressBlocked, address)
			}
			if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() ||
				ip.IsLinkLocalMulticast() || ip.IsPrivate() || isCGNAT(ip) {
				return errAddressBlocked
			}
			if ip.IsLoopback() && !allowLoopback {
				return errAddressBlocked
			}
			return nil
		},
	}
}

// isCGNAT reports whether ip is in the 100.64.0.0/10 shared-address space
// (RFC 6598), which net.IP does not classify as private.
func isCGNAT(ip netip.Addr) bool {
	if !ip.Is4() {
		return false
	}
	v4 := ip.As4()
	return v4[0] == 100 && v4[1]&0xC0 == 0x40
}

// checkRedirect caps redirects at maxRedirects and rejects redirects away
// from the http(s) scheme, where the dial policy cannot apply: the first
// maxRedirects hops are followed, the next is refused.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("kiwix: stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("kiwix: redirect to unsupported scheme %q", req.URL.Scheme)
	}
	return nil
}

// checkURLScheme rejects anything but absolute http(s) URLs before a
// request is built.
func checkURLScheme(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("kiwix: not a usable URL %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("kiwix: unsupported URL scheme %q", raw)
	}
	return nil
}

// AllowLoopback returns a copy of hc whose transport may dial loopback
// addresses; the rest of the outbound restriction (private, link-local,
// CGNAT, multicast, unspecified) stays in force. Tests that point the
// catalog or download clients at an httptest server on 127.0.0.1 opt in
// explicitly.
func AllowLoopback(hc *http.Client) *http.Client {
	return restrictClient(hc, true)
}

// policyTransports remembers, per restricted transport, whether loopback was
// explicitly allowed. NewCatalog wraps injected clients again; without this
// memory the second wrap would silently cancel a caller's AllowLoopback
// opt-in.
var policyTransports sync.Map // *http.Transport → bool (allowLoopback)

// restrictClient wraps hc's transport with the outbound dial policy and the
// redirect cap, keeping hc's timeouts and cookie jar. The returned client
// gets its own transport, so hc is never mutated. Restricting a client
// whose transport already carries the policy (NewCatalog wrapping an
// injected client) keeps the caller's loopback choice.
func restrictClient(hc *http.Client, allowLoopback bool) *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if tr, good := hc.Transport.(*http.Transport); ok && good && tr != nil {
		base = tr
	}
	if had, ok := policyTransports.Load(base); ok {
		allowLoopback = allowLoopback || had.(bool)
	}
	clone := base.Clone()
	clone.DialContext = restrictedDialer(allowLoopback).DialContext
	policyTransports.Store(clone, allowLoopback)
	out := *hc
	out.Transport = clone
	out.CheckRedirect = checkRedirect
	return &out
}
