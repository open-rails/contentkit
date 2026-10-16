package contentkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/contentkit/internal/gate"
)

// inprocessBase is the base URL of the in-process transport; no request
// leaves the process.
const inprocessBase = "http://contentkit.inprocess"

// Client is the typed Go client of the HTTP surface: one method per route
// (client_gen.go, generated from the route catalog), named by its verb and
// path (PATCH /v1/entities/{id} is PatchEntity). Engine.Client makes one over
// the in-process transport, acting as the host; NewClient one for a remote
// ContentKit. It is safe for concurrent use.
type Client struct {
	hc   *http.Client
	base string
}

// Client is the engine's in-process client, acting as the host itself:
// holding the Engine is the authority, so it sends no credential and holds
// every staff permission. Routes for a signed-in person (tier user) refuse
// it.
func (e *Engine) Client() *Client {
	if e.erasureRecovery {
		return &Client{hc: &http.Client{Transport: erasureRecoveryTransport{}}, base: inprocessBase}
	}
	return &Client{hc: &http.Client{Transport: inprocess{e.inproc}}, base: inprocessBase}
}

type erasureRecoveryTransport struct{}

func (erasureRecoveryTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, ErrErasureReplayRequired
}

// NewClient is a client of the ContentKit served at base, the host's prefix
// ("https://example.com/api/contentkit"). hc carries the caller's
// credentials (its Transport sets Authorization); nil is
// http.DefaultClient, anonymous.
func NewClient(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{hc: hc, base: strings.TrimRight(base, "/")}
}

// Error is a refused call: the route's status and stable code
// (docs/api/routes.md) and a message for people.
type Error struct {
	Status  int
	Code    string
	Message string
	// RetryAfter is the server's minimum delay before another attempt.
	RetryAfter time.Duration
	// Details retains structured validation information without coupling the
	// client to one module's error type.
	Details   json.RawMessage
	Blobs     []string
	Field     string
	RequestID string
	Retryable bool
}

func (e *Error) Error() string {
	return fmt.Sprintf("contentkit: %d %s: %s", e.Status, e.Code, e.Message)
}

// Reply is a success a generated method leaves undecoded: a stream, or a
// route answering several body types (Status says which).
type Reply struct {
	Status int
	Header http.Header
	Body   []byte
}

// Decode decodes the JSON body into v.
func (r *Reply) Decode(v any) error { return json.Unmarshal(r.Body, v) }

func (c *Client) call(ctx context.Context, method, path string, query url.Values, in, out any) error {
	res, err := c.do(ctx, method, path, query, in)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil || res.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (c *Client) raw(ctx context.Context, method, path string, query url.Values, in any) (*Reply, error) {
	res, err := c.do(ctx, method, path, query, in)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return &Reply{Status: res.StatusCode, Header: res.Header, Body: b}, nil
}

// do sends one request; a status of 300 or more is an *Error.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		var reply struct {
			Error      string          `json:"error"`
			Code       string          `json:"code"`
			RetryAfter int             `json:"retry_after"`
			Details    json.RawMessage `json:"details"`
			Blobs      []string        `json:"blobs"`
			Field      string          `json:"field"`
			RequestID  string          `json:"request_id"`
			Retryable  bool            `json:"retryable"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&reply)
		wait := max(time.Duration(reply.RetryAfter)*time.Second, retryAfter(res.Header))
		if reply.RequestID == "" {
			reply.RequestID = res.Header.Get("X-Request-ID")
		}
		if reply.Error == "" {
			reply.Error = http.StatusText(res.StatusCode)
		}
		return nil, &Error{Status: res.StatusCode, Code: reply.Code, Message: reply.Error,
			RetryAfter: max(wait, 0), Details: reply.Details, Blobs: reply.Blobs,
			Field: reply.Field, RequestID: reply.RequestID, Retryable: reply.Retryable}
	}
	return res, nil
}

func retryAfter(header http.Header) time.Duration {
	h := header.Get("Retry-After")
	if seconds, err := strconv.Atoi(h); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if until, err := http.ParseTime(h); err == nil {
		return max(time.Until(until), 0)
	}
	return 0
}

// expand fills a route path's wildcards: pairs of "{name}" and its escaped
// value.
func expand(path string, pairs ...string) string {
	for i := 0; i+1 < len(pairs); i += 2 {
		path = strings.Replace(path, pairs[i], pairs[i+1], 1)
	}
	return path
}

// escapePath escapes each segment of a trailing {name...} value.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
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
