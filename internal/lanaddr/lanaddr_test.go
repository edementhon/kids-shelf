package lanaddr

import (
	"net"
	"testing"
)

// TestUsableOrder covers the address the parent page tells tablets to open
// (the first usable one). The first case is the regression: the lowest
// private address won, and on a home server running Mullvad that was the
// WireGuard tunnel (10.68.x.x/32), which no tablet can reach, instead of the
// LAN address (2026-10-27).
func TestUsableOrder(t *testing.T) {
	const (
		lan    = net.FlagUp | net.FlagBroadcast | net.FlagMulticast | net.FlagRunning
		tunnel = net.FlagUp | net.FlagPointToPoint | net.FlagRunning
	)
	c := func(ip string, ones int, flags net.Flags) candidate {
		return candidate{ip: net.ParseIP(ip).To4(), ones: ones, flags: flags}
	}
	for _, tc := range []struct {
		name  string
		cands []candidate
		want  string
	}{
		{"wireguard tunnel beside the LAN", []candidate{
			c("10.68.230.74", 32, tunnel), c("192.168.1.20", 24, lan),
		}, "192.168.1.20"},
		{"VPN host address without the point-to-point flag", []candidate{
			c("10.64.0.2", 32, net.FlagUp|net.FlagRunning), c("192.168.1.20", 24, lan),
		}, "192.168.1.20"},
		{"10.0.0.x home network beside an idle docker bridge", []candidate{
			c("172.17.0.1", 16, net.FlagUp|net.FlagBroadcast), c("10.0.0.37", 24, lan),
		}, "10.0.0.37"},
		{"10.0.0.x home network beside a busy docker bridge", []candidate{
			c("172.17.0.1", 16, lan), c("10.0.0.37", 24, lan),
		}, "10.0.0.37"},
		{"192.168 wins over 10 whatever the order", []candidate{
			c("10.1.2.3", 24, lan), c("192.168.1.20", 24, lan),
		}, "192.168.1.20"},
		{"lowest address within a range", []candidate{
			c("10.0.0.37", 24, lan), c("10.0.0.5", 24, lan),
		}, "10.0.0.5"},
		{"link-local only beside a public address", []candidate{
			c("169.254.10.20", 16, lan), c("203.0.113.9", 24, lan),
		}, "203.0.113.9"},
		{"interface that is down", []candidate{
			c("192.168.1.20", 24, net.FlagBroadcast), c("10.0.0.37", 24, lan),
		}, "10.0.0.37"},
		{"only a tunnel", []candidate{c("10.68.230.74", 32, tunnel)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ""
			if all := usable(tc.cands); len(all) > 0 {
				got = all[0].IP.String()
			}
			if got != tc.want {
				t.Errorf("best usable = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUsableNetwork: the responder picks the address on the asker's network
// by Net, so the prefix must become the right network, and an unknown prefix
// must leave Net nil rather than claim the whole address space.
func TestUsableNetwork(t *testing.T) {
	const lan = net.FlagUp | net.FlagMulticast | net.FlagRunning
	all := usable([]candidate{
		{ip: net.ParseIP("192.0.2.77").To4(), ones: 24, flags: lan},
		{ip: net.ParseIP("198.51.100.9").To4(), ones: -1, flags: lan},
	})
	if len(all) != 2 {
		t.Fatalf("usable = %v, want 2 addresses", all)
	}
	if all[0].Net == nil || all[0].Net.String() != "192.0.2.0/24" || !all[0].Net.Contains(net.ParseIP("192.0.2.200")) {
		t.Errorf("Net = %v, want 192.0.2.0/24", all[0].Net)
	}
	if all[1].Net != nil {
		t.Errorf("unknown prefix gave Net %v, want nil", all[1].Net)
	}
}
