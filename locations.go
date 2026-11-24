package contentkit

import (
	"context"
	"net"
	"net/http"
	"net/netip"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/locationpolicy"
)

// SiteBlock describes the location and entry kind that matched a whole-site rule.
type SiteBlock struct {
	content.VisitorLocation
	Kind   string `json:"kind"`
	RuleID string `json:"rule_id"`
}

// SiteBlock checks the tenant's current enabled whole-site rules for a visitor.
// A GeoIP miss never matches countries or subdivisions; networks still match.
func (e *Engine) SiteBlock(ctx context.Context, ip netip.Addr, host string) (SiteBlock, bool, error) {
	if e.erasureRecovery {
		return SiteBlock{}, false, ErrErasureReplayRequired
	}
	l := e.geo.Lookup(ctx, ip)
	ctx = locationpolicy.WithLocation(ctx, l)
	d, _ := e.cfg.Content.DomainFor(host)
	b, err := locationpolicy.Resolve(ctx, e.pool, e.schema, e.cfg.Scope.AppID.String(), locationpolicy.DomainName(d.Name), ip.String())
	if err != nil {
		return SiteBlock{}, false, err
	}
	for _, m := range b.Matches {
		if m.Target == "site" {
			return SiteBlock{l, m.Kind, m.RuleID}, true, nil
		}
	}
	return SiteBlock{}, false, nil
}

func (e *Engine) locationRequest(next http.Handler, reader bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ipText, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ipText = r.RemoteAddr
		}
		ip, _ := netip.ParseAddr(ipText)
		ctx := locationpolicy.WithLocation(r.Context(), e.geo.Lookup(r.Context(), ip))
		if reader {
			d, _ := e.cfg.Content.DomainFor(r.Host)
			name := locationpolicy.DomainName(d.Name)
			block, err := locationpolicy.Prime(ctx, e.pool, e.schema, e.cfg.Scope.AppID.String(), name, ipText)
			if err != nil {
				e.log.ErrorContext(ctx, "location: read rules", "err", err)
				fail(w, http.StatusInternalServerError, httpapi.CodeInternal, httpapi.Message(httpapi.CodeInternal), 0)
				return
			}
			if block.Site {
				fail(w, http.StatusNotFound, httpapi.CodeNotFound, httpapi.Message(httpapi.CodeNotFound), 0)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
