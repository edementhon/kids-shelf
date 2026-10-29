// Package lanaddr finds this computer's home-network IPv4 addresses: the
// ones a tablet on the same Wi-Fi can reach. The parent page shows the best
// one, and the kids-shelf.local responder (internal/mdns) answers each
// question with the address on the asker's network.
package lanaddr

import (
	"bytes"
	"net"
	"sort"
)

// Addr is one usable home-network address.
type Addr struct {
	IP    net.IP     // always 4 bytes
	Net   *net.IPNet // the address's network; nil when the prefix is unknown
	Iface net.Interface
}

// candidate is one IPv4 address of one interface, before filtering.
type candidate struct {
	ip    net.IP // 4 bytes
	ones  int    // prefix length, -1 when unknown
	flags net.Flags
	iface net.Interface
}

// All returns this computer's usable home-network addresses, best first
// (see usable for the rules). It is empty when there are none.
func All() []Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var cands []candidate
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			c := candidate{ones: -1, flags: ifc.Flags, iface: ifc}
			switch v := a.(type) {
			case *net.IPNet:
				c.ip = v.IP.To4()
				ones, bits := v.Mask.Size()
				if bits == 8*net.IPv6len {
					ones -= 8 * (net.IPv6len - net.IPv4len) // a v4 address in a 16-byte mask
				}
				c.ones = ones
			case *net.IPAddr:
				c.ip = v.IP.To4()
			}
			if c.ip != nil {
				cands = append(cands, c)
			}
		}
	}
	return usable(cands)
}

// Best returns the address to show for other devices, or "" when this
// computer has none.
func Best() string {
	if all := All(); len(all) > 0 {
		return all[0].IP.String()
	}
	return ""
}

// usable keeps the addresses tablets can reach, best first. It skips
// interfaces that are down, have no link (an idle Docker bridge), are
// loopback or point-to-point, addresses with a /32 prefix (VPN tunnels such
// as WireGuard hand out a single host address; on a home server running
// Mullvad the tunnel's 10.68.x.x won over the LAN before this), and
// link-local 169.254/16. Of the rest it prefers 192.168/16, the usual home
// router range, then 10/8 (some ISP routers use 10.0.0.x), then 172.16/12
// (mostly container and VM bridges), then anything else; the lowest address
// comes first within a range.
func usable(cands []candidate) []Addr {
	var out []Addr
	for _, c := range cands {
		f := c.flags
		if f&net.FlagUp == 0 || f&net.FlagRunning == 0 || f&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		if len(c.ip) != net.IPv4len || c.ones == 32 || c.ip.IsLinkLocalUnicast() {
			continue
		}
		a := Addr{IP: c.ip, Iface: c.iface}
		if c.ones >= 0 {
			a.Net = &net.IPNet{IP: c.ip.Mask(net.CIDRMask(c.ones, 32)), Mask: net.CIDRMask(c.ones, 32)}
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i].IP), rank(out[j].IP)
		if ri != rj {
			return ri < rj
		}
		return bytes.Compare(out[i].IP, out[j].IP) < 0
	})
	return out
}

func rank(ip net.IP) int {
	switch {
	case ip[0] == 192 && ip[1] == 168:
		return 0
	case ip[0] == 10:
		return 1
	case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31:
		return 2
	}
	return 3
}
