// Package firewall reports and fixes whether other devices on the home
// network can reach kids-shelf through the operating system firewall. Only
// Windows has one to manage (Windows Defender Firewall): the launcher that
// listens on the LAN lives there and Windows blocks inbound connections
// until a rule allows them. Elsewhere New returns nil and the dashboard
// shows no firewall banner.
//
// The Windows implementation shells out to PowerShell (via internal/proc so
// no console window appears) and decides over the JSON PowerShell prints.
// It deliberately does not import internal/web: the web package declares the
// interface this package satisfies, and internal/app wires the two together.
package firewall

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// probeError is a package-level indirection over the real PowerShell probe
// so tests on every platform can exercise the caching and invalidation
// rules without spawning a process. Production never changes it.
var probeError func(ctx context.Context, program string) ([]byte, error)

// encodedCommand turns a PowerShell command into the base64 UTF-16LE blob
// -EncodedCommand expects, so the command's quoting and special characters
// survive the trip through the process boundary exactly.
func encodedCommand(cmd string) string {
	u := utf16.Encode([]rune(cmd))
	b := make([]byte, 0, len(u)*2)
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// ErrDeclined is the error Allow returns when the parent said No to the
// Windows administrator prompt. The dashboard shows its own message for it;
// every other Allow error is a generic save failure.
var ErrDeclined = errors.New("the Windows permission prompt was declined")

// New returns the firewall control for the program at program (the root
// launcher that listens on the LAN; app passes its install-root path).
// Windows gets a real control; every other system has no firewall to
// manage and New returns nil (firewall_other.go).
func New(program string) Control {
	return newController(program)
}

// hookProbe answers a query through the test hook when one is installed,
// reporting ok=false so the platform implementation runs otherwise.
func (c *controller) hookProbe(ctx context.Context) ([]byte, error, bool) {
	h := probeError
	if h == nil {
		return nil, nil, false
	}
	out, err := h(ctx, c.program)
	return out, err, true
}

// controller checks and fixes the firewall for one program. The status
// check and the allow action are platform-specific (query and runElevated,
// defined per platform); the caching in between is not.
type controller struct {
	program string

	mu      sync.Mutex
	pending chan struct{} // non-nil while a status probe is running
	cached  Status
	at      time.Time
}

// Status reports whether inbound connections to the program are allowed on
// the active network profile. One hidden PowerShell run answers it; the
// result is reused for cacheLifetime so the dashboard does not spawn a
// process on every paint. A failed or nonsensical probe is Unknown, never a
// guess.
func (c *controller) Status(ctx context.Context) Status {
	c.mu.Lock()
	if c.pending != nil {
		// Another caller's probe is already in flight; wait for its
		// result instead of spawning a second one.
		done := c.pending
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.cached
	}
	if time.Since(c.at) < cacheLifetime {
		defer c.mu.Unlock()
		return c.cached
	}
	c.pending = make(chan struct{})
	c.mu.Unlock()

	st := c.probe(ctx)

	c.mu.Lock()
	c.cached = st
	c.at = time.Now()
	close(c.pending)
	c.pending = nil
	c.mu.Unlock()
	return st
}

// probe runs the platform's status check once and turns its JSON into a
// Status. The Windows implementation is in firewall_windows.go.
func (c *controller) probe(ctx context.Context) Status {
	out, err := c.query(ctx)
	if err != nil {
		return Status{Unknown: true, Detail: "The firewall could not be checked."}
	}
	st := parseStatus(trimBOM(out))
	if st.Unknown {
		st.Detail = "The firewall could not be checked."
	}
	return st
}

// Allow asks Windows for an administrator's approval (the UAC prompt) and
// adds the allow rule, removing the program's own block rules. It returns
// when the elevated command has finished or was declined; the status cache
// is dropped so the next check sees the new rules.
func (c *controller) Allow(ctx context.Context) error {
	err := runElevated(ctx, allowScript(c.program))
	// Whatever the outcome, the cached verdict can no longer be trusted.
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
	return err
}

// Remove asks Windows for an administrator's approval (the UAC prompt)
// and deletes every firewall rule bound to the program's path: the allow
// rule Allow created and any block rules. Callers probe RulesExist first
// so a computer with nothing to delete never sees the prompt. It returns
// when the elevated command has finished or was declined; the status
// cache is dropped so the next check sees the new rules.
func (c *controller) Remove(ctx context.Context) error {
	err := runElevated(ctx, removeScript(c.program))
	// Whatever the outcome, the cached verdict can no longer be trusted.
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
	return err
}

// RulesExist reports whether any firewall rule is bound to the program's
// path, without elevation: the same probe Status runs, kept to the rule
// list. A failed probe reports false, so a removal never raises a UAC
// prompt on a guess.
func (c *controller) RulesExist(ctx context.Context) bool {
	out, err := c.query(ctx)
	if err != nil {
		return false
	}
	ps, ok := decodeStatus(trimBOM(out))
	return ok && len(ps.Rules) > 0
}

// Control is the portion of web.FirewallControl the implementations in this
// package satisfy, declared here so the package does not import internal/web
// (web's FirewallStatus and FirewallControl are structurally identical, and
// app wires firewall.New's return value into web.Deps.Firewall).
type Control interface {
	// Status reports whether inbound connections to the program that
	// listens on the LAN are allowed on the active network profile.
	Status(ctx context.Context) Status
	// Allow asks Windows for an administrator's approval (the UAC prompt)
	// and adds the allow rule, removing kids-shelf's own block rules. It
	// returns when the elevated command has finished or was declined.
	Allow(ctx context.Context) error
	// Remove asks Windows for an administrator's approval (the UAC prompt)
	// and deletes every firewall rule bound to the program's path. The
	// status cache is dropped afterwards. Callers probe RulesExist first
	// so a computer with no rules never sees the prompt.
	Remove(ctx context.Context) error
	// RulesExist reports whether any firewall rule is bound to the
	// program's path, without elevation. A failed probe reports false:
	// removal never prompts on a guess.
	RulesExist(ctx context.Context) bool
}

// Status is the result of one firewall check. Blocked means other devices
// cannot connect; Unknown means the check itself could not run. Detail is
// plain words for the parent page, never command output.
type Status struct {
	Blocked bool
	Unknown bool
	Detail  string
}

// cacheLifetime is how long a Status result is reused before the check runs
// again. Firewall state changes slowly and every check spawns PowerShell, so
// a half-minute of staleness is a good trade.
const cacheLifetime = 30 * time.Second

// psStatus is the PowerShell-generated shape the decision runs over: the
// active connection profiles, the firewall profile states, and the firewall
// rules bound to the kids-shelf program. Field names survive ConvertTo-Json
// and its -Compress form as here; casing is irrelevant to the decoder.
//
// Profiles come from Get-NetConnectionProfile, firewalls from
// Get-NetFirewallProfile, and rules from Get-NetFirewallApplicationFilter
// piped to Get-NetFirewallRule (see the Windows implementation).
type psStatus struct {
	Profiles  []psProfile  `json:"profiles"`
	Firewalls []psFirewall `json:"firewalls"`
	Rules     []psRule     `json:"rules"`
}

// psProfile is one network the machine is connected to and the firewall
// profile category Windows puts it in ("Public", "Private", "Domain").
type psProfile struct {
	Name     string `json:"name"`
	Category string `json:"category"`
}

// psFirewall is whether the firewall is on for one profile category.
type psFirewall struct {
	Profile string `json:"profile"` // "Public", "Private", "Domain" or a comma list
	Enabled bool   `json:"enabled"`
}

// psRule is one firewall rule bound to the program's path.
type psRule struct {
	Enabled   bool   `json:"enabled"`
	Action    string `json:"action"`    // "Allow" or "Block"
	Direction string `json:"direction"` // "Inbound" or "Outbound"
	Profile   string `json:"profile"`   // "Any", or a comma list like "Private,Public"
}

// programLiteral quotes program as a PowerShell single-quoted literal, the
// only form in which a path can be embedded verbatim (single quotes inside
// the path are doubled; nothing else in the path is interpreted).
func programLiteral(program string) string {
	return "'" + strings.ReplaceAll(program, "'", "''") + "'"
}

// allowScript returns the PowerShell that frees the program in the
// firewall: every enabled inbound Block rule bound to its path goes, and
// fresh inbound Allow rules for it take their place: TCP for the shelf's
// ports, and UDP 5353 so tablets can find it by the name kids-shelf.local
// (internal/mdns). The program path is embedded as a single-quoted
// literal, so a path containing a quote cannot break out of the script. It
// is pure so tests can check the escaping on any platform; the Windows
// implementation runs it elevated.
func allowScript(program string) string {
	return "try {" +
		"  Get-NetFirewallApplicationFilter -Program " + programLiteral(program) + " |" +
		"    Get-NetFirewallRule -ErrorAction Stop |" +
		"    Where-Object { $_.Enabled -and $_.Action -eq 'Block' } |" +
		"    Remove-NetFirewallRule -ErrorAction Stop ;" +
		"  New-NetFirewallRule -DisplayName 'kids-shelf' -Direction Inbound -Action Allow" +
		"    -Program " + programLiteral(program) + " -Protocol TCP -Profile Any -RemoteAddress LocalSubnet" +
		"    -ErrorAction Stop | Out-Null ;" +
		"  New-NetFirewallRule -DisplayName 'kids-shelf (name)' -Direction Inbound -Action Allow" +
		"    -Program " + programLiteral(program) + " -Protocol UDP -LocalPort 5353 -Profile Any -RemoteAddress LocalSubnet" +
		"    -ErrorAction Stop | Out-Null" +
		"} catch { Write-Error $_; exit 1 }"
}

// removeScript returns the PowerShell that deletes every firewall rule
// bound to the program's path: the allow rule Allow created and any block
// rules; when no rule is bound, the pipeline removes nothing. The program
// path is embedded as a single-quoted literal, like allowScript. It is
// pure so tests can check the escaping on any platform; the Windows
// implementation runs it elevated.
func removeScript(program string) string {
	return "try {" +
		"  Get-NetFirewallApplicationFilter -Program " + programLiteral(program) + " |" +
		"    Get-NetFirewallRule -ErrorAction Stop |" +
		"    Remove-NetFirewallRule -ErrorAction Stop ;" +
		"} catch { Write-Error $_; exit 1 }"
}

// decodeStatus parses the PowerShell probe payload; ok is false when the
// payload does not have the expected shape. An earlier probe wrapped its
// payload in a one-element array (ConvertTo-Json -InputObject @(...)),
// which is peeled so a blocked firewall still surfaces instead of
// degrading to Unknown.
func decodeStatus(data []byte) (ps psStatus, ok bool) {
	if err := json.Unmarshal(data, &ps); err == nil {
		return ps, true
	}
	var wrapped []psStatus
	if err := json.Unmarshal(data, &wrapped); err != nil || len(wrapped) != 1 {
		return psStatus{}, false
	}
	return wrapped[0], true
}

// parseStatus decides whether the LAN program is reachable from the JSON
// PowerShell printed (see the Windows implementation for the exact query and
// output shape). The rules:
//
//   - no active profile, or the firewall off for the active profile: open
//     (the firewall is not the thing in the way);
//   - an enabled inbound Block rule for the program: blocked (an explicit
//     block wins over an allow);
//   - an enabled inbound Allow rule covering the active profile: open;
//   - otherwise blocked, because Windows Firewall blocks inbound
//     connections until a rule allows them.
//
// If the payload does not parse, the result is Unknown, never a guess.
func parseStatus(data []byte) Status {
	ps, ok := decodeStatus(data)
	if !ok {
		return Status{Unknown: true, Detail: "The firewall could not be checked."}
	}
	if len(ps.Profiles) == 0 {
		return Status{Unknown: true, Detail: "The firewall could not be checked."}
	}
	active := categorize(ps.Profiles)
	anyOn := false
	for _, f := range ps.Firewalls {
		if f.Enabled && parseProfileUnion(f.Profile).intersects(active) {
			anyOn = true
			break
		}
	}
	if !anyOn {
		// The firewall is off for every active profile (or no profile
		// state was reported), so it is not what keeps devices out.
		return Status{Blocked: false, Detail: "Windows Firewall is off, so other devices can already reach kids-shelf."}
	}
	hasAllow := false
	for _, r := range ps.Rules {
		if !r.Enabled || !strings.EqualFold(r.Direction, "Inbound") {
			continue
		}
		action := strings.ToLower(r.Action)
		if action == "block" && parseProfileUnion(r.Profile).intersects(active) {
			return Status{Blocked: true, Detail: "A Windows Firewall rule is blocking kids-shelf."}
		}
		if action == "allow" && parseProfileUnion(r.Profile).intersects(active) {
			hasAllow = true
		}
	}
	if hasAllow {
		return Status{Blocked: false, Detail: "Windows Firewall allows kids-shelf on this network."}
	}
	return Status{Blocked: true, Detail: "Windows Firewall is blocking kids-shelf."}
}

// profileMap is a set of firewall profile names. The "all" key is the
// sentinel for the "Any" union, which covers every profile.
type profileMap map[string]bool

// parseProfileUnion parses a profile union into the set of profile names it
// contains. A union is one name or a comma-separated list ("Private",
// "Private,Public", and the Windows API spaces the list as "Private,
// Public"), or "Any", which matches every profile. Unknown names are kept
// so a category spelled differently still matches itself if both sides say
// the same thing.
func parseProfileUnion(s string) profileMap {
	set := profileMap{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		low := strings.ToLower(p)
		set[low] = true
		if low == "any" {
			set["*"] = true
		}
	}
	return set
}

func (set profileMap) intersects(other profileMap) bool {
	if set["*"] || other["*"] {
		return true
	}
	for p := range other {
		if set[p] {
			return true
		}
	}
	return false
}

// categorize reduces the connection profiles to the set of firewall profile
// categories in use, which is what a rule listing names ("Private",
// "Public", "Domain"). Profiles whose category is missing fall back to the
// profile name itself.
func categorize(profiles []psProfile) profileMap {
	active := profileMap{}
	for _, p := range profiles {
		cat := p.Category
		if cat == "" {
			cat = p.Name
		}
		if cat != "" {
			low := strings.ToLower(cat)
			// The connection-profile vocabulary calls the domain category
			// "DomainAuthenticated"; the firewall profile that governs it
			// is "Domain". Normalize so the two sides match.
			if low == "domainauthenticated" {
				low = "domain"
			}
			active[low] = true
		}
	}
	return active
}
