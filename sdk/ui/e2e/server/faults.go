package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/contentkit/media/token"
)

// rule injects one fault into matching requests: at the fault proxy in front
// of media-gateway (no-cors, rate-limit, expired, missing), at the bucket
// (drop; hold, for the media worker's requests), or at ContentKit's API (api).
type rule struct {
	ID int `json:"id"`
	// Item is the item id the request's path names; empty matches any.
	Item string `json:"item,omitempty"`
	// Dest matches Sec-Fetch-Dest: "empty" is fetch and XHR (hls.js),
	// "image" an <img>; empty matches any.
	Dest string `json:"dest,omitempty"`
	// Fault is one of:
	//	no-cors     the gateway's answer without its CORS headers
	//	rate-limit  429 with Retry-After, before the gateway
	//	expired     the request's token re-signed as expired an hour ago
	//	missing     the request for an object that does not exist
	//	drop        a bucket PUT whose connection drops after After bytes
	//	hold        the media worker's reads of the item's sources wait until the rule is cleared (From "browser": the browser's PUTs)
	//	api         ContentKit's API answers Status with an error reply (Code, Error, RetryAfter)
	Fault string `json:"fault"`
	// Path (a substring of the request path) and Method narrow api rules.
	Path       string `json:"path,omitempty"`
	Method     string `json:"method,omitempty"`
	Status     int    `json:"status,omitempty"`
	Code       string `json:"code,omitempty"`
	Error      string `json:"error,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
	Part       int    `json:"part,omitempty"`  // drop: only this multipart part (0: any PUT)
	From       string `json:"from,omitempty"`  // hold: "browser" holds the browser's PUTs instead of the worker
	After      int64  `json:"after,omitempty"` // drop: body bytes forwarded first
	Skip       int    `json:"skip,omitempty"`  // matching requests let through first
	Times      int    `json:"times,omitempty"` // matching requests to fault; 0 is every one
	Hits       int    `json:"hits"`
	seen       int
}

var faultKinds = map[string]bool{"no-cors": true, "rate-limit": true, "expired": true, "missing": true, "drop": true, "hold": true, "api": true}

type faults struct {
	h     *harness
	mu    sync.Mutex
	rules []*rule
	next  int
}

func newFaults(h *harness) *faults { return &faults{h: h} }

func (f *faults) add(rs []rule) ([]rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]rule, 0, len(rs))
	for _, r := range rs {
		if !faultKinds[r.Fault] {
			return nil, fmt.Errorf("unknown fault %q", r.Fault)
		}
		if r.Fault == "api" && (r.Status < 400 || r.Code == "") {
			return nil, errors.New("an api fault needs a status of 400 or more and a code")
		}
		f.next++
		r.ID, r.Hits = f.next, 0
		f.rules = append(f.rules, &r)
		out = append(out, r)
	}
	return out, nil
}

func (f *faults) list() []rule {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]rule, len(f.rules))
	for i, r := range f.rules {
		out[i] = *r
	}
	return out
}

// clear removes the item's rules, or every rule.
func (f *faults) clear(item string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.rules[:0]
	for _, r := range f.rules {
		if item != "" && r.Item != item {
			kept = append(kept, r)
		}
	}
	f.rules = kept
}

// match returns the first live rule of the faults in kinds matching r, and
// counts the hit.
func (f *faults) match(r *http.Request, part int, kinds ...string) *rule {
	return f.matchBody(r, "", part, kinds...)
}

// matchBody is match where the item may also be named in body (the upload
// API carries the ref in its JSON).
func (f *faults) matchBody(r *http.Request, body string, part int, kinds ...string) *rule {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.rules {
		if (x.Times > 0 && x.Hits >= x.Times) || !contains(kinds, x.Fault) {
			continue
		}
		if x.Item != "" && !strings.Contains(r.URL.Path+"/", "/"+x.Item+"/") && !strings.Contains(body, `"`+x.Item+`"`) {
			continue
		}
		if x.Dest != "" && r.Header.Get("Sec-Fetch-Dest") != x.Dest {
			continue
		}
		if x.Path != "" && !strings.Contains(r.URL.Path, x.Path) || x.Method != "" && !strings.EqualFold(x.Method, r.Method) {
			continue
		}
		if worker := strings.Contains(r.UserAgent(), "aws-sdk"); x.Fault == "hold" && worker == (x.From == "browser") {
			continue
		}
		if x.Fault == "drop" && (part == 0 && x.Part != 0 || x.Part != 0 && x.Part != part) {
			continue
		}
		if x.seen++; x.seen <= x.Skip {
			continue
		}
		x.Hits++
		c := *x
		return &c
	}
	return nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// gatewayProxy is the media origin: media-gateway behind the fault rules.
// The browser's Host passes through (the gateway serves the namespaces by it).
func (f *faults) gatewayProxy(gateway *url.URL) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(gateway)
			pr.Out.Host = pr.In.Host
		},
		ModifyResponse: func(res *http.Response) error {
			if f.match(res.Request, 0, "no-cors") != nil {
				for k := range res.Header {
					if strings.HasPrefix(k, "Access-Control-") {
						res.Header.Del(k)
					}
				}
			}
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			proxy.ServeHTTP(w, r)
			return
		}
		if x := f.match(r, 0, "rate-limit", "expired", "missing"); x != nil {
			switch x.Fault {
			case "rate-limit":
				if o := r.Header.Get("Origin"); o != "" {
					w.Header().Set("Access-Control-Allow-Origin", o)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
					w.Header().Set("Vary", "Origin")
				}
				w.Header().Set("Retry-After", "5")
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			case "expired":
				r = f.expire(r)
			case "missing":
				r = r.Clone(r.Context())
				i := strings.LastIndex(r.URL.Path, "/")
				r.URL.Path = r.URL.Path[:i+1] + "sha256-" + strings.Repeat("0", 64) + "-00000000-0000-7000-8000-000000000000"
				r.URL.RawPath = ""
			}
		}
		proxy.ServeHTTP(w, r)
	})
}

// expire re-signs the request's token for its item with an expiry an hour
// ago, as a stale grant reaches the gateway.
func (f *faults) expire(r *http.Request) *http.Request {
	r = r.Clone(r.Context())
	// /v1/{ns}/{kind}/{id}/private/{blob}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(p) < 5 {
		return r
	}
	q := r.URL.Query()
	q.Set("t", f.h.ring.Sign(token.ItemScope(p[1], p[2], p[3]), time.Now().Add(-time.Hour)))
	r.URL.RawQuery = q.Encode()
	return r
}

// bucket forwards /{bucket}/ (presigned uploads) to MinIO unchanged, Host
// included, so the signatures hold; drop rules cut a PUT's connection.
func (f *faults) bucket(endpoint *url.URL) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		part, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
		var drop *rule
		if r.Method == http.MethodPut {
			drop = f.match(r, part, "drop")
		}
		// The worker's reads of an item's private files (its sources) wait;
		// its manifest reads do not, since it reads those under the item's
		// lock and would block the host's commits.
		worker := strings.Contains(r.UserAgent(), "aws-sdk")
		if !worker || r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/private/") {
			if x := f.match(r, part, "hold"); x != nil {
				f.wait(r, x.ID)
			}
		}
		out, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint.Scheme+"://"+endpoint.Host+r.URL.RequestURI(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Header = r.Header.Clone()
		out.Host = r.Host
		out.ContentLength = r.ContentLength
		if r.ContentLength != 0 {
			out.Body = r.Body
		}
		if drop != nil {
			hijack(w, r, drop.After)
			return
		}
		res, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		for k, v := range res.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(res.StatusCode)
		_, _ = io.Copy(w, res.Body)
	})
}

// wait blocks until rule id is cleared, the request ends, or two minutes pass.
func (f *faults) wait(r *http.Request, id int) {
	until := time.Now().Add(2 * time.Minute)
	for time.Now().Before(until) && r.Context().Err() == nil && f.has(id) {
		time.Sleep(50 * time.Millisecond)
	}
}

// any reports a rule of fault kind exists.
func (f *faults) any(kind string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.rules {
		if x.Fault == kind {
			return true
		}
	}
	return false
}

func (f *faults) has(id int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.rules {
		if x.ID == id {
			return true
		}
	}
	return false
}

// api answers matching api rules with ContentKit's error reply.
func (f *faults) api(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil && r.Method != http.MethodGet && f.any("api") {
			body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		x := f.matchBody(r, string(body), 0, "api")
		if x == nil {
			next.ServeHTTP(w, r)
			return
		}
		out := map[string]any{"error": x.Error, "code": x.Code}
		if x.Error == "" {
			out["error"] = x.Code
		}
		if x.RetryAfter > 0 {
			out["retry_after"] = x.RetryAfter
			w.Header().Set("Retry-After", strconv.Itoa(x.RetryAfter))
		}
		reply(w, x.Status, out)
	})
}

// hijack reads after bytes of the body, then drops the client connection
// without an answer, as a network failure mid-upload does.
func hijack(w http.ResponseWriter, r *http.Request, after int64) {
	if after > 0 {
		_, _ = io.CopyN(io.Discard, r.Body, after)
	}
	rc := http.NewResponseController(w)
	conn, _, err := rc.Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}
