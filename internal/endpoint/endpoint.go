// Package endpoint checks the URLs a credential is sent to.
package endpoint

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// Secure refuses a URL a bearer credential or token would cross in clear: it
// must be https, or http to a loopback host.
func Secure(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.User != nil:
		return nil, fmt.Errorf("%q is not an absolute URL", raw)
	case u.Scheme == "https":
		return u, nil
	case u.Scheme == "http" && Loopback(u.Hostname()):
		return u, nil
	case u.Scheme == "http":
		return nil, fmt.Errorf("%q is plain http; use https (http only for loopback)", raw)
	}
	return nil, fmt.Errorf("%q is not an http(s) URL", raw)
}

// Loopback reports "localhost" or a loopback address literal.
func Loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}
