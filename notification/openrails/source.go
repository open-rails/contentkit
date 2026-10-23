// Package openrails reads OpenRails facts using an inbox-owned cursor.
// Hosts own identity mapping, optional-event policy and scheduling.
package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/helpers/notify"
)

// Backend reads a merchant-bound log without changing source state. Expired
// and reset cursors return ErrCursorExpired.
type Backend interface {
	ListEvents(context.Context, string, int) (EventPage, error)
}

// ErrCursorExpired requires an explicit destination resync. Consume never
// silently starts over or skips a lost interval.
var ErrCursorExpired = errors.New("openrails notifications: event cursor expired; resync required")

// Options binds one trusted deployment and merchant. Token supplies a
// merchant-authorized application token; Client can authenticate instead.
// URL precedes /v1. A native Backend needs no URL or network credentials.
type Options struct {
	Backend    Backend
	Name       string
	URL        string
	MerchantID string
	Client     *http.Client
	Token      func(context.Context) (string, error)
}

type Source struct {
	base, name, merchant string
	client               *http.Client
	token                func(context.Context) (string, error)
	backend              Backend
}

// New validates the trusted source. HTTP requests have a fifteen-second
// deadline and refuse redirects to keep credentials on that deployment.
func New(opts Options) (*Source, error) {
	u, err := url.Parse(opts.URL)
	if opts.Backend == nil && (err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "") {
		return nil, fmt.Errorf("openrails notifications: URL must be a trusted HTTP(S) deployment URL")
	}
	if !validCoordinate(opts.MerchantID) {
		return nil, fmt.Errorf("openrails notifications: a merchant is required")
	}
	name := opts.Name
	if name == "" && opts.Backend == nil {
		name = "openrails:" + strings.TrimRight(u.String(), "/")
	}
	if !validCoordinate(name) {
		return nil, fmt.Errorf("openrails notifications: invalid origin system name")
	}
	client := http.Client{}
	if opts.Client != nil {
		client = *opts.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base := ""
	if u != nil {
		base = strings.TrimRight(u.String(), "/")
	}
	return &Source{base: base, name: name, merchant: opts.MerchantID, client: &client, token: opts.Token, backend: opts.Backend}, nil
}

type EventSubject struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Event is the retained source snapshot. Customer fields are derived from its
// data, never fetched from a billing row which may have since been deleted.
type Event struct {
	ID         string          `json:"id"`
	MerchantID string          `json:"merchant_id"`
	Source     string          `json:"source"`
	EventType  string          `json:"type"`
	Version    int             `json:"version"`
	Subject    EventSubject    `json:"subject"`
	Data       json.RawMessage `json:"data"`
	CreatedAt  time.Time       `json:"occurred_at"`
	CustomerID string          `json:"-"`
	// Nil means unknown historical ownership, not a native account.
	AccountIssuer   *string `json:"-"`
	ActionRequested bool    `json:"-"`
	Optional        bool    `json:"-"`
}

type EventPage struct {
	Events           []Event `json:"events"`
	NextCursor       string  `json:"next_cursor"`
	HasMore          bool    `json:"has_more"`
	RetentionSeconds int64   `json:"retention_seconds"`
}

func (s *Source) read(ctx context.Context, after string, limit int) (EventPage, error) {
	var page EventPage
	var err error
	if s.backend != nil {
		page, err = s.backend.ListEvents(ctx, after, limit)
	} else {
		query := url.Values{"after": {after}, "limit": {fmt.Sprint(limit)}}
		err = s.request(ctx, "/v1/app/events?"+query.Encode(), &page)
	}
	if err != nil {
		return EventPage{}, err
	}
	if page.Events == nil || len(page.Events) > limit || page.NextCursor == "" || len(page.NextCursor) > 8192 || page.RetentionSeconds <= 0 || (len(page.Events) > 0 || page.HasMore) && page.NextCursor == after {
		return EventPage{}, fmt.Errorf("openrails notifications: invalid event page")
	}
	page.Events = append([]Event{}, page.Events...)
	for i := range page.Events {
		e := &page.Events[i]
		if e.MerchantID != s.merchant {
			return EventPage{}, fmt.Errorf("openrails notifications: event is outside configured merchant")
		}
		if !validCoordinate(e.ID) || !validCoordinate(e.EventType) || !validCoordinate(e.Source) || e.CreatedAt.IsZero() || e.Version != 1 {
			return EventPage{}, fmt.Errorf("openrails notifications: event lacks a supported identity, kind or time")
		}
		var data map[string]json.RawMessage
		if len(e.Data) > 64<<10 || json.Unmarshal(e.Data, &data) != nil || data == nil {
			return EventPage{}, fmt.Errorf("openrails notifications: data must be a JSON object up to 64 KiB")
		}
		if e.Source != "notification" {
			continue
		}
		var snapshot struct {
			AccountID     string  `json:"account_id"`
			CustomerID    string  `json:"customer_id"`
			AccountIssuer *string `json:"account_issuer"`
		}
		if json.Unmarshal(e.Data, &snapshot) != nil || e.Subject.Type != "customer" || !validCoordinate(e.Subject.ID) || snapshot.CustomerID != e.Subject.ID || snapshot.AccountID != e.Subject.ID {
			return EventPage{}, fmt.Errorf("openrails notifications: invalid customer snapshot")
		}
		e.CustomerID, e.AccountIssuer = snapshot.CustomerID, snapshot.AccountIssuer
		switch e.EventType {
		case "payment_method_failed", "payment_method_update_required", "payment_authentication_required", "order_due", "order_overdue", "order_collection_stopped", "account_delinquent", "balance_low", "balance_depleted":
			e.ActionRequested = true
		}
		// Interpret domain fields only for the kind that defines them. A new,
		// unrelated kind may use the same field name with another JSON type.
		var downgradeApplied bool
		if e.EventType == "premium_renewed" {
			if raw, exists := data["downgrade_applied"]; exists && json.Unmarshal(raw, &downgradeApplied) != nil {
				return EventPage{}, fmt.Errorf("openrails notifications: invalid premium renewal snapshot")
			}
		}
		e.Optional = e.EventType == "premium_renewed" && !downgradeApplied || e.EventType == "one_off_purchase_completed" || e.EventType == "usage_charged"
	}
	return page, nil
}

func validCoordinate(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 512 && !strings.ContainsRune(s, 0)
}

func (s *Source) request(ctx context.Context, path string, output any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+path, nil)
	if err != nil {
		return fmt.Errorf("openrails notifications: request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if s.token != nil {
		token, err := s.token(ctx)
		if err != nil {
			return fmt.Errorf("openrails notifications: obtain application token: %w", err)
		}
		if strings.TrimSpace(token) == "" {
			return fmt.Errorf("openrails notifications: application token is empty")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("openrails notifications: request: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusGone {
		return ErrCursorExpired
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("openrails notifications: source returned HTTP %d", res.StatusCode)
	}
	const maxPage = 8 << 20
	b, err := io.ReadAll(io.LimitReader(res.Body, maxPage+1))
	if err != nil {
		return fmt.Errorf("openrails notifications: read events: %w", err)
	}
	if len(b) > maxPage {
		return fmt.Errorf("openrails notifications: event page exceeds 8 MiB")
	}
	if err := json.Unmarshal(b, output); err != nil {
		return fmt.Errorf("openrails notifications: decode events: %w", err)
	}
	return nil
}

func (s *Source) Origin() notify.OriginScope {
	return notify.OriginScope{System: s.name, Scope: s.merchant}
}
