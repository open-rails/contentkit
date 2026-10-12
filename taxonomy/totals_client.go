package taxonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/helpers/cache"

	"github.com/open-rails/contentkit/internal/httpapi"
)

// TotalsClient reads another deployment's catalog totals (GET /catalog/totals),
// such as a sibling site's, server to server, cached per kind: reloaded in the
// background after ten minutes, the last good totals served while reloads
// fail, for up to a day.
type TotalsClient struct {
	url    string
	http   *http.Client
	totals *cache.Cache[string, Total]
}

// NewTotalsClient reads the deployment whose ContentKit routes are under baseURL
// (its Routes.Prefix), e.g. "https://hentai0.com/api/contentkit". A nil hc is a client with a
// ten-second timeout.
func NewTotalsClient(baseURL string, hc *http.Client) (*TotalsClient, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: TotalsClient base URL %q must be absolute http(s)", ErrInvalid, baseURL)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	c := &TotalsClient{url: u.String() + httpapi.PublicCatalog.Prefix() + "/totals", http: hc}
	if c.totals, err = cache.New(remoteTotalsPolicy, c.load); err != nil {
		return nil, err
	}
	return c, nil
}

// Close releases the cache's goroutines.
func (c *TotalsClient) Close() { c.totals.Close() }

// Totals reads the remote's totals as Store.Totals answers them, in the scope
// the remote derives from the request: q.Kinds is required and q.Scope must
// be empty.
func (c *TotalsClient) Totals(ctx context.Context, q TotalsQuery) (Totals, error) {
	out := Totals{Kinds: map[string]Total{}}
	if len(q.Kinds) == 0 || len(q.Scope.AnyOf)+len(q.Scope.NoneOf) > 0 {
		return out, fmt.Errorf("%w: TotalsClient needs kinds and no scope", ErrInvalid)
	}
	for _, k := range q.Kinds {
		if !identRE.MatchString(k) {
			return out, fmt.Errorf("%w: kind %q must match %s", ErrInvalid, k, identRE)
		}
	}
	languages, err := normalizeLanguages(q.Languages)
	if err != nil {
		return out, err
	}
	got, err := c.totals.Get(ctx, q.Kinds)
	if err != nil {
		return out, err
	}
	for _, k := range q.Kinds {
		t, ok := got[k]
		if !ok {
			return out, fmt.Errorf("taxonomy: %s answered no total for %q", c.url, k)
		}
		out.Kinds[k] = t.only(languages, slices.Sorted(maps.Keys(t.Tiers)))
	}
	return out, nil
}

// load fetches kinds in one request.
func (c *TotalsClient) load(ctx context.Context, kinds []string) (map[string]Total, error) {
	kinds = slices.Compact(slices.Sorted(slices.Values(kinds)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"?kinds="+url.QueryEscape(strings.Join(kinds, ",")), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("taxonomy: catalog totals: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("taxonomy: catalog totals: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e errorBody
		_ = json.Unmarshal(body, &e)
		return nil, fmt.Errorf("taxonomy: catalog totals: %s answered %d %s", c.url, resp.StatusCode, e.Code)
	}
	var t Totals
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("taxonomy: catalog totals: %w", err)
	}
	return t.Kinds, nil
}
