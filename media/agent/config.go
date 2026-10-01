package agent

import (
	"fmt"
	"net/url"
	"strings"
)

// validOrigin accepts exactly "scheme://host[:port]".
func validOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Contains(u.Host, "*") ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || o != u.Scheme+"://"+u.Host {
		return fmt.Errorf("agent: invalid CORS origin %q (want exactly scheme://host[:port])", o)
	}
	return nil
}
