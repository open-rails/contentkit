package contentkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/open-rails/helpers/notify"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/notification"
)

// inprocessBase is the base URL of the in-process transport; no request
// leaves the process.
const inprocessBase = "http://contentkit.inprocess"

// Client is the typed Go client of the HTTP surface, acting as the host
// itself: holding the Engine is the authority, so it sends no credential and
// holds every staff permission. Engine.Client makes one over the in-process
// transport; it is safe for concurrent use.
type Client struct {
	hc   *http.Client
	base string
}

// Client is the engine's in-process client.
func (e *Engine) Client() *Client {
	return &Client{hc: &http.Client{Transport: inprocess{e.inproc}}, base: inprocessBase}
}

// Error is a refused call: the route's status and stable code
// (docs/api/routes.md) and a message for people.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("contentkit: %d %s: %s", e.Status, e.Code, e.Message)
}

// PublishNotifications publishes notices in one transaction (POST
// /v1/notifications/publish), each once per origin and recipient, and
// returns them as stored; a notice to an erased recipient is dropped.
func (c *Client) PublishNotifications(ctx context.Context, notices []notify.Input) ([]notify.Item, error) {
	var out notification.Published
	err := c.call(ctx, http.MethodPost, "/v1/notifications/publish", notification.Publication{Notifications: notices}, &out)
	return out.Items, err
}

// CreateContent creates a draft content item (POST /v1/contents).
func (c *Client) CreateContent(ctx context.Context, in catalog.ContentInput) (catalog.Content, error) {
	var out catalog.Content
	err := c.call(ctx, http.MethodPost, "/v1/contents", in, &out)
	return out, err
}

// UpdateContent edits a content item (PATCH /v1/contents/{id}).
func (c *Client) UpdateContent(ctx context.Context, id string, patch catalog.ContentPatch) (catalog.Content, error) {
	var out catalog.Content
	err := c.call(ctx, http.MethodPatch, "/v1/contents/"+url.PathEscape(id), patch, &out)
	return out, err
}

// PublishContent publishes a content item now, or schedules it (POST
// /v1/contents/{id}/publish).
func (c *Client) PublishContent(ctx context.Context, id string, in catalog.PublishInput) (catalog.Content, error) {
	var out catalog.Content
	err := c.call(ctx, http.MethodPost, "/v1/contents/"+url.PathEscape(id)+"/publish", in, &out)
	return out, err
}

// UnpublishContent takes a content item back to a draft (POST
// /v1/contents/{id}/unpublish).
func (c *Client) UnpublishContent(ctx context.Context, id string) (catalog.Content, error) {
	var out catalog.Content
	err := c.call(ctx, http.MethodPost, "/v1/contents/"+url.PathEscape(id)+"/unpublish", nil, &out)
	return out, err
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var reply struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&reply)
		return &Error{Status: res.StatusCode, Code: reply.Code, Message: reply.Error}
	}
	if out == nil || res.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// inprocess serves a request with the engine's handlers, as the host.
type inprocess struct{ h http.Handler }

func (t inprocess) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	r := req.Clone(gate.AsHost(req.Context()))
	r.RequestURI, r.RemoteAddr = r.URL.RequestURI(), "127.0.0.1:0"
	if r.Body == nil {
		r.Body = http.NoBody
	}
	w := &recorder{header: http.Header{}, status: http.StatusOK}
	t.h.ServeHTTP(w, r)
	return &http.Response{Status: http.StatusText(w.status), StatusCode: w.status, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: w.header, Body: io.NopCloser(&w.body), ContentLength: int64(w.body.Len()), Request: req}, nil
}

type recorder struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.body.Write(b)
}
