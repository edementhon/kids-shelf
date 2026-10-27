package web

import (
	"net"
	"testing"
)

// TestPickLANIPv4 covers the address the parent page tells tablets to open.
// The first case is the regression: the lowest private address won, and on a
// home server running Mullvad that was the WireGuard tunnel (10.68.x.x/32),
// which no tablet can reach, instead of the LAN address (2026-10-27).
func TestPickLANIPv4(t *testing.T) {
	const (
		lan    = net.FlagUp | net.FlagBroadcast | net.FlagMulticast | net.FlagRunning
		tunnel = net.FlagUp | net.FlagPointToPoint | net.FlagRunning
	)
	c := func(ip string, ones int, flags net.Flags) lanCandidate {
		return lanCandidate{ip: net.ParseIP(ip).To4(), ones: ones, flags: flags}
	}
	for _, tc := range []struct {
		name  string
		cands []lanCandidate
		want  string
	}{
		{"wireguard tunnel beside the LAN", []lanCandidate{
			c("10.68.230.74", 32, tunnel), c("192.168.1.20", 24, lan),
		}, "192.168.1.20"},
		{"VPN host address without the point-to-point flag", []lanCandidate{
			c("10.64.0.2", 32, net.FlagUp|net.FlagRunning), c("192.168.1.20", 24, lan),
		}, "192.168.1.20"},
		{"10.0.0.x home network beside an idle docker bridge", []lanCandidate{
			c("172.17.0.1", 16, net.FlagUp|net.FlagBroadcast), c("10.0.0.37", 24, lan),
		}, "10.0.0.37"},
		{"10.0.0.x home network beside a busy docker bridge", []lanCandidate{
			c("172.17.0.1", 16, lan), c("10.0.0.37", 24, lan),
		}, "10.0.0.37"},
		{"192.168 wins over 10 whatever the order", []lanCandidate{
			c("10.1.2.3", 24, lan), c("192.168.1.20", 24, lan),
		}, "192.168.1.20"},
		{"lowest address within a range", []lanCandidate{
			c("10.0.0.37", 24, lan), c("10.0.0.5", 24, lan),
		}, "10.0.0.5"},
		{"link-local only beside a public address", []lanCandidate{
			c("169.254.10.20", 16, lan), c("203.0.113.9", 24, lan),
		}, "203.0.113.9"},
		{"interface that is down", []lanCandidate{
			c("192.168.1.20", 24, net.FlagBroadcast), c("10.0.0.37", 24, lan),
		}, "10.0.0.37"},
		{"only a tunnel", []lanCandidate{c("10.68.230.74", 32, tunnel)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickLANIPv4(tc.cands); got != tc.want {
				t.Errorf("pickLANIPv4 = %q, want %q", got, tc.want)
			}
		})
	}
}
