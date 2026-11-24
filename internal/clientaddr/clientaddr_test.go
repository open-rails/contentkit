package clientaddr

import (
	"errors"
	"net/http"
	"net/netip"
	"testing"
)

func TestResolve(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	cases := []struct {
		name, peer string
		header     http.Header
		want       string
		malformed  bool
	}{
		{"untrusted peer ignores headers", "198.51.100.9:443", http.Header{"X-Forwarded-For": {"203.0.113.7"}}, "198.51.100.9", false},
		{"no trusted proxies ignores headers", "10.0.0.1:1", http.Header{"X-Forwarded-For": {"203.0.113.7"}}, "", false},
		{"trusted peer without headers is the client", "10.0.0.1:1", nil, "10.0.0.1", false},
		{"right-most untrusted hop", "10.0.0.1:1", http.Header{"X-Forwarded-For": {"203.0.113.7, 10.0.0.2"}}, "203.0.113.7", false},
		{"a prepended spoof is never chosen", "10.0.0.1:1", http.Header{"X-Forwarded-For": {"198.51.100.1, 203.0.113.7"}}, "203.0.113.7", false},
		{"spoof garbage left of the client is never read", "10.0.0.1:1", http.Header{"X-Forwarded-For": {"not an address, 203.0.113.7"}}, "203.0.113.7", false},
		{"header lines join in order", "10.0.0.1:1", http.Header{"X-Forwarded-For": {"198.51.100.1", "203.0.113.7, 10.1.1.1"}}, "203.0.113.7", false},
		{"every hop trusted is the left-most", "10.0.0.1:1", http.Header{"X-Forwarded-For": {"10.9.9.9, 10.0.0.2"}}, "10.9.9.9", false},
		{"ports and brackets", "10.0.0.1:1", http.Header{"X-Forwarded-For": {"[2001:db8::7]:443, 203.0.113.7:8080"}}, "203.0.113.7", false},
		{"bracketed IPv6 without port", "[fd00::1]:1", http.Header{"X-Forwarded-For": {"[2001:db8::7]"}}, "2001:db8::7", false},
		{"IPv4-mapped is unmapped", "[::ffff:10.0.0.1]:1", http.Header{"X-Forwarded-For": {"::ffff:203.0.113.7"}}, "203.0.113.7", false},
		{"malformed hop a trusted proxy wrote", "10.0.0.1:1", http.Header{"X-Forwarded-For": {"203.0.113.7, garbage"}}, "", true},
		{"empty chain", "10.0.0.1:1", http.Header{"X-Forwarded-For": {""}}, "", true},
		{"zone is refused", "10.0.0.1:1", http.Header{"X-Forwarded-For": {"fe80::1%eth0"}}, "", true},
		{"Forwarded", "10.0.0.1:1", http.Header{"Forwarded": {`for=198.51.100.1, for="[2001:db8:cafe::17]:4711";proto=https, For=10.0.0.3;by=10.0.0.1`}}, "2001:db8:cafe::17", false},
		{"Forwarded quoted separators", "10.0.0.1:1", http.Header{"Forwarded": {`for=203.0.113.7;host="a,b;c"`}}, "203.0.113.7", false},
		{"Forwarded unknown", "10.0.0.1:1", http.Header{"Forwarded": {"for=unknown"}}, "", true},
		{"Forwarded without for", "10.0.0.1:1", http.Header{"Forwarded": {"proto=https"}}, "", true},
		{"Forwarded unbalanced quote", "10.0.0.1:1", http.Header{"Forwarded": {`for="203.0.113.7`}}, "", true},
		{"both headers are ambiguous", "10.0.0.1:1", http.Header{"Forwarded": {"for=203.0.113.7"}, "X-Forwarded-For": {"203.0.113.7"}}, "", true},
		{"unparseable peer", "pipe", nil, "invalid IP", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			list := trusted
			if c.name == "no trusted proxies ignores headers" {
				list, c.want = nil, "10.0.0.1"
			}
			got, err := Resolve(c.peer, c.header, list)
			if c.malformed {
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("got %v, %v; want ErrMalformed", got, err)
				}
				return
			}
			if err != nil || got.String() != c.want {
				t.Fatalf("got %v, %v; want %s", got, err, c.want)
			}
		})
	}
}

func TestKey(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"203.0.113.7":            "203.0.113.7",
		"::ffff:203.0.113.7":     "203.0.113.7",
		"2001:db8:1:2:3:4:5:6":   "2001:db8:1:2::/64",
		"2001:db8:1:2::ffff":     "2001:db8:1:2::/64",
		"fe80::1%eth0":           "fe80::/64",
		"not-an-address":         "not-an-address",
		"2001:db8:1:3:0:0:0:0":   "2001:db8:1:3::/64",
		"2001:0db8:0001:0002::1": "2001:db8:1:2::/64",
	} {
		if got := Key(in); got != want {
			t.Errorf("Key(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckTrusted(t *testing.T) {
	if err := CheckTrusted([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []netip.Prefix{{}, netip.MustParsePrefix("::ffff:10.0.0.0/104")} {
		if CheckTrusted([]netip.Prefix{bad}) == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
