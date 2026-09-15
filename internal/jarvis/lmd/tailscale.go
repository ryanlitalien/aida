package lmd

import (
	"fmt"
	"net"
)

// tailscaleCGNAT is the CGNAT range every tailnet node gets an address in -
// 100.64.0.0/10 (RFC 6598) - see
// https://tailscale.com/kb/1015/100.x-addresses.
var tailscaleCGNAT = mustParseCIDR("100.64.0.0/10")

func mustParseCIDR(cidr string) *net.IPNet {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err) // unreachable: cidr is a constant we control
	}
	return n
}

// ResolveTailscaleIP scans the host's network interfaces for an address in
// the Tailscale CGNAT range and returns it, or "" (with a nil error) if none
// is found - e.g. tailscaled isn't running. Deliberately enumerates
// interfaces directly rather than shelling out to the `tailscale` binary:
// this must work whether or not the CLI is installed, and per
// docs/lmd-protocol.md the daemon binds to the resolved address itself
// rather than trusting an external process to report it.
func ResolveTailscaleIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("lmd: enumerate network interfaces: %w", err)
	}
	var addrs []net.Addr
	for _, iface := range ifaces {
		a, err := iface.Addrs()
		if err != nil {
			continue // best-effort per interface; one uncooperative iface shouldn't abort the scan
		}
		addrs = append(addrs, a...)
	}
	return tailscaleIPFromAddrs(addrs), nil
}

// tailscaleIPFromAddrs is the pure core of ResolveTailscaleIP, split out so
// it's table-testable against fake net.Addr values instead of the host's
// real interfaces.
func tailscaleIPFromAddrs(addrs []net.Addr) string {
	for _, a := range addrs {
		ip := ipFromAddr(a)
		if ip == nil {
			continue
		}
		ip4 := ip.To4()
		if ip4 != nil && tailscaleCGNAT.Contains(ip4) {
			return ip4.String()
		}
	}
	return ""
}

// ipFromAddr extracts the net.IP from the two concrete types
// net.Interface.Addrs() actually returns: *net.IPNet for an interface
// address with a netmask (the common case), *net.IPAddr for a bare address.
// Anything else is skipped.
func ipFromAddr(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	default:
		return nil
	}
}
