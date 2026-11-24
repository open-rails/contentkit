// Package clientaddr resolves a request's client address behind trusted
// proxies, and the key anonymous limits and reactions count a client under.
package clientaddr

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ErrMalformed is a forwarded chain that names no address where one is read,
// or a request carrying both Forwarded and X-Forwarded-For.
var ErrMalformed = errors.New("malformed forwarded client address")

// CheckTrusted refuses a prefix that is invalid or IPv4-mapped: write IPv4
// proxies as IPv4 prefixes.
func CheckTrusted(trusted []netip.Prefix) error {
	for _, p := range trusted {
		switch {
		case !p.IsValid():
			return fmt.Errorf("trusted proxy prefix %v is invalid", p)
		case p.Addr().Is4In6():
			return fmt.Errorf("trusted proxy prefix %v is IPv4-mapped; write it as an IPv4 prefix", p)
		case p.Addr().Zone() != "":
			return fmt.Errorf("trusted proxy prefix %v has a zone", p)
		}
	}
	return nil
}

// Resolve is the client's address. The peer (remoteAddr) is the client
// unless it is one of trusted; then the hops in Forwarded or X-Forwarded-For
// are read right to left, skipping trusted ones, and the first other is the
// client (the left-most when every hop is trusted). Only hops a trusted proxy
// wrote are read, so what a client prepends is never chosen. An unparseable
// peer is the zero Addr.
func Resolve(remoteAddr string, h http.Header, trusted []netip.Prefix) (netip.Addr, error) {
	peer := peerAddr(remoteAddr)
	if !peer.IsValid() || !contains(trusted, peer) {
		return peer, nil
	}
	forwarded, xff := h.Values("Forwarded"), h.Values("X-Forwarded-For")
	var hops []string
	switch {
	case len(forwarded) > 0 && len(xff) > 0:
		return netip.Addr{}, fmt.Errorf("%w: both Forwarded and X-Forwarded-For", ErrMalformed)
	case len(forwarded) > 0:
		var ok bool
		if hops, ok = split(strings.Join(forwarded, ","), ','); !ok {
			return netip.Addr{}, fmt.Errorf("%w: unbalanced quotes in Forwarded", ErrMalformed)
		}
	case len(xff) > 0:
		hops = strings.Split(strings.Join(xff, ","), ",")
	default:
		return peer, nil
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		node := strings.TrimSpace(hops[i])
		if len(forwarded) > 0 {
			node = forwardedFor(node)
		}
		addr, ok := parseNode(node)
		if !ok {
			return netip.Addr{}, fmt.Errorf("%w: hop %q", ErrMalformed, strings.TrimSpace(hops[i]))
		}
		if client = addr; !contains(trusted, addr) {
			return addr, nil
		}
	}
	return client, nil
}

// Key is what anonymous limits, reactions and votes count a client under:
// its IPv4 address, or its IPv6 /64, the least a subscriber is handed, so
// rotating addresses within it is one client. "" stays "", and text that is
// not an address is its own key.
func Key(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	p, _ := addr.Prefix(64)
	return p.String()
}

func peerAddr(remoteAddr string) netip.Addr {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap().WithZone("")
}

func contains(trusted []netip.Prefix, addr netip.Addr) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// parseNode reads one hop: an address, with or without a port, IPv6 bracketed
// or bare.
func parseNode(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		var ap netip.AddrPort
		if ap, err = netip.ParseAddrPort(s); err == nil {
			addr = ap.Addr()
		} else if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
			addr, err = netip.ParseAddr(s[1 : len(s)-1])
		}
	}
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// forwardedFor is a Forwarded element's for= value, unquoted (RFC 7239);
// "" when it has none.
func forwardedFor(element string) string {
	pairs, _ := split(element, ';')
	for _, pair := range pairs {
		name, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "for") {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			var b strings.Builder
			for i := 1; i < len(value)-1; i++ {
				if value[i] == '\\' && i+1 < len(value)-1 {
					i++
				}
				b.WriteByte(value[i])
			}
			value = b.String()
		}
		return value
	}
	return ""
}

// split cuts s at sep outside quoted strings; false for an unbalanced quote.
func split(s string, sep byte) ([]string, bool) {
	var out []string
	quoted, start := false, 0
	for i := 0; i < len(s); i++ {
		switch {
		case quoted && s[i] == '\\':
			i++
		case s[i] == '"':
			quoted = !quoted
		case !quoted && s[i] == sep:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:]), !quoted
}
