package lmd

import (
	"net"
	"testing"
)

// fakeIPNetAddr and fakeIPAddr construct the two concrete net.Addr types
// net.Interface.Addrs() actually returns (an interface address with a
// netmask, and a bare address), so tailscaleIPFromAddrs can be tested
// against a fabricated interface table instead of the host's real one.
func fakeIPNetAddr(ip string, ones, bits int) net.Addr {
	return &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(ones, bits)}
}

func fakeIPAddr(ip string) net.Addr {
	return &net.IPAddr{IP: net.ParseIP(ip)}
}

// TestTailscaleIPFromAddrs exercises the pure core of ResolveTailscaleIP -
// tailscaleIPFromAddrs - against fabricated interface address tables: the
// usual loopback + private-LAN + Tailscale mix, IPv6-only interfaces, a
// tailnet-free host, and a bare *net.IPAddr (no netmask) entry.
func TestTailscaleIPFromAddrs(t *testing.T) {
	cases := []struct {
		name  string
		addrs []net.Addr
		want  string
	}{
		{
			name: "loopback + LAN + tailscale - tailscale wins",
			addrs: []net.Addr{
				fakeIPNetAddr("127.0.0.1", 8, 32),
				fakeIPNetAddr("192.168.1.42", 24, 32),
				fakeIPNetAddr("100.64.0.1", 32, 32),
			},
			want: "100.64.0.1",
		},
		{
			name: "no tailscale interface present",
			addrs: []net.Addr{
				fakeIPNetAddr("127.0.0.1", 8, 32),
				fakeIPNetAddr("192.168.1.42", 24, 32),
				fakeIPNetAddr("10.0.0.5", 24, 32),
			},
			want: "",
		},
		{
			name:  "no interfaces at all",
			addrs: nil,
			want:  "",
		},
		{
			name: "IPv6-only interfaces are skipped",
			addrs: []net.Addr{
				fakeIPNetAddr("fe80::1", 64, 128),
				fakeIPNetAddr("2001:db8::1", 64, 128),
			},
			want: "",
		},
		{
			name: "bare *net.IPAddr entry (no netmask) still matches",
			addrs: []net.Addr{
				fakeIPAddr("100.64.0.1"),
			},
			want: "100.64.0.1",
		},
		{
			name: "CGNAT range boundaries: 100.63.x is out, 100.64.x is in, 100.127.x is in, 100.128.x is out",
			addrs: []net.Addr{
				fakeIPNetAddr("100.63.255.255", 32, 32), // just below 100.64.0.0/10
				fakeIPNetAddr("100.128.0.0", 32, 32),    // just above 100.64.0.0/10
				fakeIPNetAddr("100.100.0.1", 32, 32),    // inside the range
			},
			want: "100.100.0.1",
		},
		{
			name: "unrecognized net.Addr implementation is skipped, not fatal",
			addrs: []net.Addr{
				unknownAddr{},
				fakeIPNetAddr("100.64.0.1", 32, 32),
			},
			want: "100.64.0.1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tailscaleIPFromAddrs(c.addrs); got != c.want {
				t.Errorf("tailscaleIPFromAddrs() = %q, want %q", got, c.want)
			}
		})
	}
}

// unknownAddr satisfies net.Addr but is neither *net.IPNet nor *net.IPAddr -
// exercises ipFromAddr's default case.
type unknownAddr struct{}

func (unknownAddr) Network() string { return "fake" }
func (unknownAddr) String() string  { return "fake-addr" }
