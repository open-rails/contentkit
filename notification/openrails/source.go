// Package openrails imports OpenRails customer inbox notices without depending
// on its Go module. The host owns identity mapping, credentials and scheduling.
// It never reads operator findings or acknowledges OpenRails host events.
package openrails

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Options binds one trusted OpenRails deployment and merchant. Token returns a
// merchant-bound application access token with merchant:customers:read. Client
// may instead supply authentication through its transport. URL is the origin
// and any deployment prefix, before /v1. Credentials never enter a browser.
type Options struct {
	// Name is the full stable origin system, used verbatim; defaults to openrails:URL.
	Name       string
	URL        string
	MerchantID string
	Client     *http.Client
	Token      func(context.Context) (string, error)
}

// Source reads customer notifications from one configured OpenRails merchant.
type Source struct {
	base     string
	name     string
	merchant string
	client   *http.Client
	token    func(context.Context) (string, error)
}

// New validates deployment configuration. Requests have a 15-second deadline
// even when Client has no timeout. Redirects are refused to keep credentials
// and requests on the configured deployment.
func New(opts Options) (*Source, error) {
	u, err := url.Parse(opts.URL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("openrails notifications: URL must be a trusted HTTP(S) deployment URL")
	}
	if strings.TrimSpace(opts.MerchantID) == "" {
		return nil, fmt.Errorf("openrails notifications: MerchantID is required")
	}
	client := http.Client{}
	if opts.Client != nil {
		client = *opts.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	name := opts.Name
	if name == "" {
		name = "openrails:" + strings.TrimRight(u.String(), "/")
	}
	return &Source{base: strings.TrimRight(u.String(), "/"), name: name, merchant: opts.MerchantID, client: &client, token: opts.Token}, nil
}

// Notice is the consumed subset of OpenRails' public customer notification.
// EventType intentionally accepts future kinds. Data contains only the
// user-visible billing payload; host applications choose how to render it.
type Notice struct {
	ID              string          `json:"id"`
	MerchantID      string          `json:"merchant_id"`
	CustomerID      string          `json:"customer_id"`
	EventType       string          `json:"event_type"`
	Data            json.RawMessage `json:"data"`
	CreatedAt       time.Time       `json:"created_at"`
	ActionRequested bool            `json:"action_requested,omitempty"`
}

// Page is one retained source inbox page. An empty NextCursor finishes a scan.
// Source read state is deliberately ignored: each destination owns its state.
type Page struct {
	Data       []Notice `json:"data"`
	NextCursor *string  `json:"next_cursor"`
}

// Fetch reads one newest-first page. CustomerID must come from the host's
// verified account mapping, never an untrusted browser's recipient selection.
// Begin periodic rescans with an empty cursor: this retained inbox is not an
// append-only event archive and timestamps are not delivery watermarks.
func (s *Source) Fetch(ctx context.Context, customerID, cursor string) (Page, error) {
	var page Page
	if strings.TrimSpace(customerID) == "" || len(customerID) > 512 || len(cursor) > 4096 {
		return page, fmt.Errorf("openrails notifications: invalid customer or cursor")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	q := url.Values{"limit": {"100"}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/v1/app/customers/"+url.PathEscape(customerID)+"/notifications?"+q.Encode(), nil)
	if err != nil {
		return page, fmt.Errorf("openrails notifications: request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if s.token != nil {
		token, err := s.token(ctx)
		if err != nil {
			return page, fmt.Errorf("openrails notifications: obtain application token: %w", err)
		}
		if strings.TrimSpace(token) == "" {
			return page, fmt.Errorf("openrails notifications: application token is empty")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return page, fmt.Errorf("openrails notifications: fetch: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return page, fmt.Errorf("openrails notifications: source returned HTTP %d", res.StatusCode)
	}
	const maxPage = 8 << 20
	body, err := io.ReadAll(io.LimitReader(res.Body, maxPage+1))
	if err != nil {
		return page, fmt.Errorf("openrails notifications: read page: %w", err)
	}
	if len(body) > maxPage {
		return page, fmt.Errorf("openrails notifications: page exceeds 8 MiB")
	}
	if err = json.Unmarshal(body, &page); err != nil {
		return page, fmt.Errorf("openrails notifications: decode page: %w", err)
	}
	if page.Data == nil || len(page.Data) > 100 {
		return Page{}, fmt.Errorf("openrails notifications: invalid page data")
	}
	if page.NextCursor != nil && (len(*page.NextCursor) > 4096 || (*page.NextCursor != "" && *page.NextCursor == cursor)) {
		return Page{}, fmt.Errorf("openrails notifications: invalid next cursor")
	}
	for _, n := range page.Data {
		if n.MerchantID != s.merchant || n.CustomerID != customerID {
			return Page{}, fmt.Errorf("openrails notifications: source notification is outside configured merchant/customer")
		}
		if n.ID == "" || n.EventType == "" || n.CreatedAt.IsZero() {
			return Page{}, fmt.Errorf("openrails notifications: source notification lacks identity, type or time")
		}
		var data map[string]json.RawMessage
		if len(n.Data) > 64<<10 || json.Unmarshal(n.Data, &data) != nil || data == nil {
			return Page{}, fmt.Errorf("openrails notifications: data must be a JSON object up to 64 KiB")
		}
	}
	return page, nil
}
