package main

import (
	"fmt"
	"net/netip"
	"strings"
)

// readHost reads the host of a CONNECT target. It lowercases it and drops one
// trailing dot. An address, in brackets or as a dotted quad as netip reads it,
// is returned as one; one with a zone is refused. A host whose last label is
// written as a number is refused, as WHATWG's URL standard reads a host that
// ends in a number, so that 2130706433, 0x7f000001, 0177.0.0.1 and 127.1 never
// reach a resolver. Any other host must be a name (isName).
func readHost(h string) (string, netip.Addr, string) {
	host := strings.TrimSuffix(strings.ToLower(h), ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return "", netip.Addr{}, fmt.Sprintf("%s has a zone", host)
		}
		return host, addr, ""
	}
	if endsInNumber(host) {
		return "", netip.Addr{}, fmt.Sprintf("%s is written as a number", host)
	}
	if !isName(host) {
		return "", netip.Addr{}, fmt.Sprintf("%q is not a name", host)
	}
	return host, netip.Addr{}, ""
}

// endsInNumber reports whether the last label of a host is all digits or
// starts with 0x.
func endsInNumber(host string) bool {
	last := host[strings.LastIndex(host, ".")+1:]
	if strings.HasPrefix(last, "0x") {
		return true
	}
	return last != "" && strings.Trim(last, "0123456789") == ""
}

// isName reports whether a host is a name of lower-case letters, digits,
// hyphens and dots, with at least one dot and no empty label, that does not
// end in a number. internal/seal checks a list by the same rule.
func isName(host string) bool {
	if !strings.Contains(host, ".") || strings.Contains(host, "..") ||
		strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || endsInNumber(host) {
		return false
	}
	for _, c := range host {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// refusedV4 are the IPv4 blocks that are not public: the entries of IANA's
// IPv4 special-purpose address registry (read on 2026-11-19), multicast and
// 240/4, which takes in the limited broadcast address.
var refusedV4 = prefixes(
	"0.0.0.0/8",       // "this network"
	"10.0.0.0/8",      // private use
	"100.64.0.0/10",   // shared address space
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link local
	"172.16.0.0/12",   // private use
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"192.31.196.0/24", // AS112
	"192.52.193.0/24", // AMT
	"192.88.99.0/24",  // 6to4 relay anycast
	"192.168.0.0/16",  // private use
	"192.175.48.0/24", // AS112, direct delegation
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, with 255.255.255.255
)

// globalUnicast is the only IPv6 block that can be public. Outside it lie ::1,
// ::, ::ffff:0:0/96, the NAT64 blocks 64:ff9b::/96 and 64:ff9b:1::/48,
// fc00::/7, fe80::/10, fec0::/10 and ff00::/8 among others.
var globalUnicast = netip.MustParsePrefix("2000::/3")

// refusedV6 are the special-purpose blocks of IANA's IPv6 registry inside
// 2000::/3 (read on 2026-11-19).
var refusedV6 = prefixes(
	"2001::/23",         // IETF protocol assignments, with Teredo 2001::/32
	"2001:db8::/32",     // documentation
	"2002::/16",         // 6to4
	"2620:4f:8000::/48", // AS112, direct delegation
	"3fff::/20",         // documentation
)

func prefixes(s ...string) []netip.Prefix {
	p := make([]netip.Prefix, len(s))
	for i, v := range s {
		p[i] = netip.MustParsePrefix(v)
	}
	return p
}

// notPublic returns why an address is not public, or "" when it is. The
// caller has unmapped it: an IPv4-mapped address that is still mapped lies
// outside 2000::/3 and is refused.
func notPublic(a netip.Addr) string {
	blocks := refusedV4
	if a.Is6() {
		if !globalUnicast.Contains(a) {
			return fmt.Sprintf("%s is not public", a)
		}
		blocks = refusedV6
	}
	for _, p := range blocks {
		if p.Contains(a) {
			return fmt.Sprintf("%s is not public", a)
		}
	}
	return ""
}
