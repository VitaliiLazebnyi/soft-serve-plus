// Package clientaddr groups client addresses for per-client connection
// limits.
package clientaddr

import "net/netip"

// Prefix returns the key a client at ip is counted under: the address itself
// for IPv4 (and IPv4-mapped IPv6), and its /64 for IPv6, since one client
// usually holds a whole /64. ok is false when ip is not valid.
//
// The zone of an IPv6 address is dropped, so every peer in fe80::/64 shares
// one bucket, whatever its interface.
func Prefix(ip netip.Addr) (p netip.Prefix, ok bool) {
	if !ip.IsValid() {
		return netip.Prefix{}, false
	}
	ip = ip.Unmap().WithZone("")
	bits := 32
	if ip.Is6() {
		bits = 64
	}
	p, err := ip.Prefix(bits)
	return p, err == nil
}
