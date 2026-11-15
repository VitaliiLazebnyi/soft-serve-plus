package clientaddr

import (
	"net/netip"
	"testing"
)

func TestPrefix(t *testing.T) {
	for addr, want := range map[string]string{
		"192.0.2.1":         "192.0.2.1/32",
		"::ffff:192.0.2.1":  "192.0.2.1/32",
		"2001:db8::1":       "2001:db8::/64",
		"2001:db8::ffff:1":  "2001:db8::/64",
		"2001:db8:0:1::1":   "2001:db8:0:1::/64",
		"fe80::1%eth0":      "fe80::/64",
		"fe80::2%eth1":      "fe80::/64",
		"fe80:0:0:1::1%en0": "fe80:0:0:1::/64",
	} {
		p, ok := Prefix(netip.MustParseAddr(addr))
		if !ok || p.String() != want {
			t.Errorf("Prefix(%s) = %v, %v; want %s", addr, p, ok, want)
		}
	}
	if _, ok := Prefix(netip.Addr{}); ok {
		t.Error("Prefix of the zero Addr is ok")
	}
}
