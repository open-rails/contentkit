// Package openrails consumes independently acknowledged OpenRails notification
// events. Hosts own identity mapping, optional-event policy and scheduling.
// OpenRails produces facts; ContentKit owns the user inbox and its read state.
package openrails

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Options binds one trusted OpenRails deployment, merchant and independent
// consumer. Token supplies a merchant-authorized application access token;
// Client may instead authenticate through its transport. URL precedes /v1.
type Options struct {
	// Name is the complete stable origin system; defaults to openrails:URL.
	Name       string
	URL        string
	MerchantID string
	// Consumer is stable across restarts and unique per destination within the
	// authenticated application/merchant. Tokens can rotate without losing ACKs.
	Consumer string
	Client   *http.Client
	Token    func(context.Context) (string, error)
}

// Source consumes the notification events of one configured merchant.
type Source struct {
	base, name, merchant, consumer string
	client                         *http.Client
	token                          func(context.Context) (string, error)
}

var consumerName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// New validates configuration. Every HTTP request has a 15-second deadline.
// Redirects are refused to keep credentials on the configured deployment.
func New(opts Options) (*Source, error) {
	u, err := url.Parse(opts.URL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("openrails notifications: URL must be a trusted HTTP(S) deployment URL")
	}
	if strings.TrimSpace(opts.MerchantID) == "" || len(opts.MerchantID) > 512 || strings.ContainsRune(opts.MerchantID, 0) || !consumerName.MatchString(opts.Consumer) || opts.Consumer == "." || opts.Consumer == ".." {
		return nil, fmt.Errorf("openrails notifications: merchant and a valid 1-64 character consumer name are required")
	}
	name := opts.Name
	if name == "" {
		name = "openrails:" + strings.TrimRight(u.String(), "/")
	}
	if strings.TrimSpace(name) == "" || len(name) > 512 || strings.ContainsRune(name, 0) {
		return nil, fmt.Errorf("openrails notifications: invalid origin system name")
	}
	client := http.Client{}
	if opts.Client != nil {
		client = *opts.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Source{base: strings.TrimRight(u.String(), "/"), name: name, merchant: opts.MerchantID, consumer: opts.Consumer, client: &client, token: opts.Token}, nil
}

// Event is an immutable source fact, not an OpenRails-owned inbox item. Kind
// strings and JSON fields remain extensible. ActionRequested is historical,
// independent of a destination's tracked workflow state.
type Event struct {
	ID              string          `json:"id"`
	MerchantID      string          `json:"merchant_id"`
	CustomerID      string          `json:"customer_id"`
	EventType       string          `json:"event_type"`
	Data            json.RawMessage `json:"data"`
	CreatedAt       time.Time       `json:"created_at"`
	ActionRequested bool            `json:"action_requested,omitempty"`
	Optional        bool            `json:"optional,omitempty"`
}

type eventPage struct {
	Data []Event `json:"data"`
}

func (s *Source) path() string { return "/v1/app/notification-consumers/" + s.consumer }

// Register idempotently establishes this consumer's independent ACK namespace.
// Register before scheduling Consume; the source controls historical retention.
func (s *Source) Register(ctx context.Context) error {
	return s.request(ctx, http.MethodPost, s.path(), nil, nil)
}

// Retire explicitly ends this consumer's delivery obligation. Do not retire on
// an ordinary worker shutdown: registration and ACKs must survive restarts.
func (s *Source) Retire(ctx context.Context) error {
	return s.request(ctx, http.MethodDelete, s.path(), nil, nil)
}

func (s *Source) pending(ctx context.Context, limit int) ([]Event, error) {
	var page eventPage
	if err := s.request(ctx, http.MethodGet, fmt.Sprintf("%s/events?limit=%d", s.path(), limit), nil, &page); err != nil {
		return nil, err
	}
	if page.Data == nil || len(page.Data) > limit {
		return nil, fmt.Errorf("openrails notifications: invalid event page")
	}
	for _, e := range page.Data {
		if e.MerchantID != s.merchant || strings.TrimSpace(e.CustomerID) == "" || len(e.CustomerID) > 512 || strings.ContainsRune(e.CustomerID, 0) {
			return nil, fmt.Errorf("openrails notifications: event is outside configured merchant or lacks a valid customer")
		}
		if e.ID == "" || len(e.ID) > 512 || e.EventType == "" || len(e.EventType) > 512 || e.CreatedAt.IsZero() {
			return nil, fmt.Errorf("openrails notifications: event lacks identity, kind or time")
		}
		var data map[string]json.RawMessage
		if len(e.Data) > 64<<10 || json.Unmarshal(e.Data, &data) != nil || data == nil {
			return nil, fmt.Errorf("openrails notifications: data must be a JSON object up to 64 KiB")
		}
	}
	return page.Data, nil
}

func (s *Source) acknowledge(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.request(ctx, http.MethodPost, s.path()+"/acknowledge", struct {
		IDs []string `json:"notification_ids"`
	}{ids}, nil)
}

func (s *Source) request(ctx context.Context, method, path string, input, output any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("openrails notifications: encode request: %w", err)
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, body)
	if err != nil {
		return fmt.Errorf("openrails notifications: request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("openrails notifications: source returned HTTP %d", res.StatusCode)
	}
	if output == nil {
		return nil
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
