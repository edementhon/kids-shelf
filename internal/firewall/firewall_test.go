package firewall

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// psPayload marshals one probe payload through Go's encoder for the
// behavioral cases below; it is convenient, not a producer sample. The
// literal-JSON cases in TestParseStatus pin the shape the real PowerShell
// probe prints (extra informational fields, booleans as true/false),
// which Go's marshaler does not reproduce.
func psPayload(profiles []psProfile, firewalls []psFirewall, rules []psRule) []byte {
	b, err := json.Marshal(psStatus{Profiles: profiles, Firewalls: firewalls, Rules: rules})
	if err != nil {
		panic(err)
	}
	return b
}

func TestParseStatus(t *testing.T) {
	activePublic := []psProfile{{Name: "Wi-Fi", Category: "Public"}}
	fwOn := []psFirewall{{Profile: "Public", Enabled: true}}
	fwOff := []psFirewall{{Profile: "Public", Enabled: false}}
	blockOnPublic := []psRule{{
		Enabled: true, Action: "Block", Direction: "Inbound", Profile: "Public",
	}}
	allowOnPublic := []psRule{{
		Enabled: true, Action: "Allow", Direction: "Inbound", Profile: "Public",
	}}
	allowOnPrivate := []psRule{{
		Enabled: true, Action: "Allow", Direction: "Inbound", Profile: "Private",
	}}
	allowAny := []psRule{{
		Enabled: true, Action: "Allow", Direction: "Inbound", Profile: "Any",
	}}
	multiProfile := []psFirewall{
		{Profile: "Public", Enabled: true},
		{Profile: "Private", Enabled: false},
	}

	cases := []struct {
		name string
		data []byte
		want Status
	}{
		{
			name: "firewall off on the active profile",
			data: psPayload(activePublic, fwOff, nil),
			want: Status{Blocked: false, Detail: "Windows Firewall is off, so other devices can already reach Kids Shelf."},
		},
		{
			name: "firewall on for the active profile and off for others",
			data: psPayload(activePublic, multiProfile, nil),
			want: Status{Blocked: true, Detail: "Windows Firewall is blocking Kids Shelf."},
		},
		{
			name: "block rule on the active profile",
			data: psPayload(activePublic, fwOn, blockOnPublic),
			want: Status{Blocked: true, Detail: "A Windows Firewall rule is blocking Kids Shelf."},
		},
		{
			name: "allow rule on the active profile",
			data: psPayload(activePublic, fwOn, allowOnPublic),
			want: Status{Blocked: false, Detail: "Windows Firewall allows Kids Shelf on this network."},
		},
		{
			name: "allow rule covering every profile",
			data: psPayload(activePublic, fwOn, allowAny),
			want: Status{Blocked: false, Detail: "Windows Firewall allows Kids Shelf on this network."},
		},
		{
			name: "allow rule only on Private while the active profile is Public",
			data: psPayload(activePublic, fwOn, allowOnPrivate),
			want: Status{Blocked: true, Detail: "Windows Firewall is blocking Kids Shelf."},
		},
		{
			name: "no rules yet",
			data: psPayload(activePublic, fwOn, nil),
			want: Status{Blocked: true, Detail: "Windows Firewall is blocking Kids Shelf."},
		},
		{
			name: "disabled rule is ignored",
			data: psPayload(activePublic, fwOn, []psRule{{
				Enabled: false, Action: "Allow", Direction: "Inbound", Profile: "Public",
			}}),
			want: Status{Blocked: true, Detail: "Windows Firewall is blocking Kids Shelf."},
		},
		{
			name: "outbound rule does not open inbound",
			data: psPayload(activePublic, fwOn, []psRule{{
				Enabled: true, Action: "Allow", Direction: "Outbound", Profile: "Public",
			}}),
			want: Status{Blocked: true, Detail: "Windows Firewall is blocking Kids Shelf."},
		},
		{
			name: "no active profile",
			data: []byte(`{"profiles":[],"firewalls":[{"profile":"Public","enabled":true}],"rules":[]}`),
			want: Status{Unknown: true, Detail: "The firewall could not be checked."},
		},
		{
			name: "garbage",
			data: []byte(`{"profiles": [} this is not JSON`),
			want: Status{Unknown: true, Detail: "The firewall could not be checked."},
		},
		{
			name: "block wins over a later allow",
			data: psPayload(activePublic, fwOn, []psRule{
				{Enabled: true, Action: "Allow", Direction: "Inbound", Profile: "Public"},
				{Enabled: true, Action: "Block", Direction: "Inbound", Profile: "Public"},
			}),
			want: Status{Blocked: true, Detail: "A Windows Firewall rule is blocking Kids Shelf."},
		},
		{
			// Captured verbatim from the fixed probe on Windows 11 24H2
			// (26100), Public network, after Cancel on Windows' own
			// "allow public and private networks" prompt.
			name: "probe output captured on Windows 11 after Cancel",
			data: []byte(`{"firewalls":[{"profile":"Domain","active":false,"enabled":true},{"profile":"Private","active":false,"enabled":true},{"profile":"Public","active":true,"enabled":true}],"profiles":[{"active":true,"category":"Public","name":"Ethernet"}],"rules":[{"profile":"Public","direction":"Inbound","action":"Block","enabled":true},{"profile":"Public","direction":"Inbound","action":"Block","enabled":true}]}`),
			want: Status{Blocked: true, Detail: "A Windows Firewall rule is blocking Kids Shelf."},
		},
		{
			name: "array-wrapped probe output still reports blocked",
			data: []byte(`[{"profiles":[{"name":"Ethernet","category":"Public","active":true}],"firewalls":[{"profile":"Domain","enabled":true,"active":false},{"profile":"Private","enabled":true,"active":false},{"profile":"Public","enabled":true,"active":true}],"rules":[{"enabled":true,"action":"Block","direction":"Inbound","profile":"Public"},{"enabled":true,"action":"Block","direction":"Inbound","profile":"Public"}]}]`),
			want: Status{Blocked: true, Detail: "A Windows Firewall rule is blocking Kids Shelf."},
		},
		{
			name: "allow rule on Private, Public with the active network Private",
			data: []byte(`{"profiles":[{"name":"Ethernet","category":"Private","active":true}],"firewalls":[{"profile":"Private","enabled":true,"active":true}],"rules":[{"enabled":true,"action":"Allow","direction":"Inbound","profile":"Private, Public"}]}`),
			want: Status{Blocked: false, Detail: "Windows Firewall allows Kids Shelf on this network."},
		},
		{
			name: "domain network is governed by the Domain firewall profile",
			data: []byte(`{"profiles":[{"name":"Corp","category":"DomainAuthenticated","active":true}],"firewalls":[{"profile":"Domain","enabled":true,"active":true}],"rules":[]}`),
			want: Status{Blocked: true, Detail: "Windows Firewall is blocking Kids Shelf."},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseStatus(c.data)
			if got != c.want {
				t.Errorf("parseStatus = %+v, want %+v", got, c.want)
			}
		})
	}
}

// embedded in the command must be a closed single-quoted string holding only
// the path, with its quotes doubled, never the raw path.
func TestAllowScriptEscapesPath(t *testing.T) {
	path := `C:\Program Files (x86)\Kids' Shelf\kids-shelf.exe`
	script := allowScript(path)
	lit := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	if !strings.Contains(script, "-Program "+lit) {
		t.Errorf("allowScript does not embed the escaped literal %q:\n%s", lit, script)
	}
	if strings.Contains(script, path) {
		t.Errorf("allowScript embeds the raw path %q, which a quote could break out of:\n%s", path, script)
	}
}

// TestRemoveScriptEscapesPath pins removeScript's quoting the way
// TestAllowScriptEscapesPath pins allowScript's: the program path embedded
// in the command must be a closed single-quoted string holding only the
// path, with its quotes doubled, never the raw path.
func TestRemoveScriptEscapesPath(t *testing.T) {
	path := `C:\Program Files (x86)\Kids' Shelf\kids-shelf.exe`
	script := removeScript(path)
	lit := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	if !strings.Contains(script, "-Program "+lit) {
		t.Errorf("removeScript does not embed the escaped literal %q:\n%s", lit, script)
	}
	if strings.Contains(script, path) {
		t.Errorf("removeScript embeds the raw path %q, which a quote could break out of:\n%s", path, script)
	}
}

// TestRulesExistProbe drives the rules-exist probe through the hook: rules
// in the probe payload mean true, an empty rule list means false, and a
// failed probe is false - never a guess that would raise a UAC prompt.
func TestRulesExistProbe(t *testing.T) {
	payload := psPayload(
		[]psProfile{{Name: "Wi-Fi", Category: "Public"}},
		[]psFirewall{{Profile: "Public", Enabled: true}},
		[]psRule{{Enabled: true, Action: "Allow", Direction: "Inbound", Profile: "Public"}},
	)
	old := probeError
	probeError = func(ctx context.Context, program string) ([]byte, error) { return payload, nil }
	c := &controller{program: `C:\Kids' Shelf\kids-shelf.exe`}
	if !c.RulesExist(context.Background()) {
		t.Error("RulesExist with a rule in the probe payload = false, want true")
	}

	probeError = func(ctx context.Context, program string) ([]byte, error) {
		return psPayload(nil, nil, nil), nil
	}
	if c.RulesExist(context.Background()) {
		t.Error("RulesExist with no rules = true, want false")
	}

	probeError = func(ctx context.Context, program string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	if c.RulesExist(context.Background()) {
		t.Error("RulesExist with a failed probe = true, want false")
	}
	probeError = old
}

// TestEncodedCommandRoundTrips decodes a -EncodedCommand payload back to the
// original command, proving the base64 UTF-16LE encoding preserves it byte
// for byte (PowerShell decodes exactly this).
func TestEncodedCommandRoundTrips(t *testing.T) {
	cmd := "Write-Output 'it''s quoted'"
	raw, err := base64.StdEncoding.DecodeString(encodedCommand(cmd))
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("decoded payload has odd length %d, not UTF-16LE", len(raw))
	}
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	if got := string(utf16.Decode(u)); got != cmd {
		t.Errorf("round trip = %q, want %q", got, cmd)
	}
}

// TestStatusCacheAndInvalidation exercises the 30-second reuse and the
// post-Allow invalidation without spawning PowerShell: the probe hook takes
// the place of the real query (the hook lives in the Windows file, so this
// test runs on every platform the package builds on).
func TestStatusCacheAndInvalidation(t *testing.T) {
	payload := psPayload(
		[]psProfile{{Name: "Wi-Fi", Category: "Public"}},
		[]psFirewall{{Profile: "Public", Enabled: true}},
		nil,
	)
	probes := 0
	fresh := func(ctx context.Context, program string) ([]byte, error) {
		probes++
		return payload, nil
	}
	old := probeError
	probeError = fresh
	defer func() { probeError = old }()
	// New is nil outside Windows (no firewall to manage there), so the
	// tests build the controller directly: query is the only platform
	// bit, and the hook takes it over.
	c := &controller{program: `C:\Kids' Shelf\kids-shelf.exe`}
	if got := c.Status(context.Background()); !got.Blocked {
		t.Fatalf("first Status = %+v, want blocked", got)
	}
	// Within the cache window the probe does not run again.
	if got := c.Status(context.Background()); !got.Blocked {
		t.Fatalf("cached Status = %+v, want blocked", got)
	}
	if probes != 1 {
		t.Fatalf("after two Status calls the probe ran %d times, want 1", probes)
	}
	// Allow drops the cache; the next Status re-probes. (Allow itself
	// spawns the elevated PowerShell, so the invalidation is asserted by
	// clearing the cache timestamp the way Allow does.)
	c.at = time.Time{}
	c.Status(context.Background())
	if probes != 2 {
		t.Fatalf("after cache invalidation the probe ran %d times, want 2", probes)
	}
}

// TestStatusProbeFailureIsUnknown shows a failing probe surfaces as Unknown,
// not as a guess, and feeds the cache like any other result.
func TestStatusProbeFailureIsUnknown(t *testing.T) {
	old := probeError
	probeError = func(ctx context.Context, program string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	defer func() { probeError = old }()

	c := New(`C:\kids-shelf.exe`).(*controller)
	if got := c.Status(context.Background()); !got.Unknown || got.Detail == "" {
		t.Errorf("failed probe Status = %+v, want Unknown with a plain Detail", got)
	}
}

// TestStatusProbeCancelled shows a caller whose context dies gets the last
// known status, matching the single-flight behaviour of Status.
func TestStatusProbeCancelled(t *testing.T) {
	old := probeError
	probeError = func(ctx context.Context, program string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	defer func() { probeError = old }()

	c := New(`C:\kids-shelf.exe`).(*controller)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if got := c.Status(ctx); !got.Unknown {
		t.Errorf("cancelled probe Status = %+v, want Unknown", got)
	}
}
