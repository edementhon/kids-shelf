package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthzReportsInstance(t *testing.T) {
	oldVersion := Version
	Version = "9.9.9-test"
	defer func() { Version = oldVersion }()

	s := New("instance-abc")
	ts := httptest.NewServer(s.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("GET /healthz Content-Type = %q, want application/json", ct)
	}
	var h healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decoding /healthz: %v", err)
	}
	if h.Status != "ok" {
		t.Errorf("status = %q, want ok", h.Status)
	}
	if h.ID != "instance-abc" {
		t.Errorf("id = %q, want the instance id", h.ID)
	}
	if h.Version != "9.9.9-test" {
		t.Errorf("version = %q, want the set version", h.Version)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	s := New("any-id")
	ts := httptest.NewServer(s.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/no-such-page")
	if err != nil {
		t.Fatalf("GET /no-such-page: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /no-such-page status = %d, want 404", resp.StatusCode)
	}
}

// TestHostAllowlistRejectsForeignHosts checks the DNS-rebinding gate: a
// request whose Host names an attacker's domain is refused before any route
// runs, while the launcher's 127.0.0.1 /healthz and the allowlisted names
// keep answering.
func TestHostAllowlistRejectsForeignHosts(t *testing.T) {
	do := func(ts *httptest.Server, host string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/healthz", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /healthz with Host %q: %v", host, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	s := New("any-id")
	ts := httptest.NewServer(s.Mux())
	defer ts.Close()

	if code := do(ts, "attacker.example"); code != http.StatusMisdirectedRequest {
		t.Errorf("Host attacker.example = %d, want 421", code)
	}
	if code := do(ts, "attacker.example:80"); code != http.StatusMisdirectedRequest {
		t.Errorf("Host attacker.example:80 = %d, want 421", code)
	}
	// The launcher checks /healthz by its 127.0.0.1 address; that must keep
	// working, as must the built-in names.
	for _, host := range []string{"127.0.0.1", "localhost", "kids-shelf.local"} {
		if code := do(ts, host+":8070"); code != http.StatusOK {
			t.Errorf("Host %s:8070 = %d, want 200", host, code)
		}
	}

	// A name from Deps.ExtraHosts is accepted too.
	app := NewApp("any-id", Deps{ExtraHosts: []string{"shelf.home"}})
	tsApp := httptest.NewServer(app.Mux())
	defer tsApp.Close()
	if code := do(tsApp, "shelf.home:8070"); code != http.StatusOK {
		t.Errorf("ExtraHosts shelf.home = %d, want 200", code)
	}
}
