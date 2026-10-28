package contentkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// engineAvatars is how the engine shows people's avatars (Config.Avatars),
// and on a site that is not the primary, forwards their writes to it.
type engineAvatars struct {
	urls    *media.Registry   // builds avatar URLs: the primary's registry, else one of the avatar kind alone
	rule    *media.PresetRule // the content model's
	forward *avatarForward    // nil on the primary
}

// newAvatars builds Config.Avatars over the engine's media.
func (e *Engine) newAvatars(deps Deps) (*engineAvatars, error) {
	a := e.cfg.Avatars
	if a == nil {
		return nil, nil
	}
	kind, preset := a.kind()
	if e.scope != nil {
		kind.Namespace = ""
		kind.Account = true
	}
	out := &engineAvatars{urls: e.media.reg}
	if a.Primary != "" {
		// Its media host serves the primary's files: URLs are built at it.
		var err error
		if out.urls, err = media.NewRegistry(media.Config{Namespace: e.media.reg.Config().Namespace, Scope: e.media.reg.Config().Scope, BaseURL: e.media.reg.Config().BaseURL,
			Kinds: []media.Kind{kind}, Avatars: media.Avatars{Kind: kind.Name, Preset: preset}}, media.Hooks{}); err != nil {
			return nil, fmt.Errorf("contentkit: Config.Avatars: %w", err)
		}
		if out.forward, err = newAvatarForward(e, deps, kind, a); err != nil {
			return nil, err
		}
	}
	for _, r := range out.urls.PresetRules() {
		if r.Kind == kind.Name && r.Name == preset {
			out.rule = &r
		}
	}
	return out, nil
}

// avatarForward sends a non-primary site's people's avatar uploads, edits
// and editor reads to the primary's routes, as a holder of
// root:avatars:write there. A person reaches only their own avatar, within
// this site's commit limit. Nothing is kept here: no journal row, bucket
// object or worker job. Tokens stay with their own site; the browser's
// only cross-site request is the presigned PUT to the primary's bucket.
type avatarForward struct {
	kind    string
	primary string // the primary's API base
	key     string
	media   *url.URL // this site's media host, where read URLs are rehosted
	gate    *gate.Gate
	commits media.CommitLimiter
	client  *http.Client
	log     *slog.Logger
}

// forwardTimeout bounds one forwarded call.
const forwardTimeout = 30 * time.Second

func newAvatarForward(e *Engine, deps Deps, kind media.Kind, a *AvatarsConfig) (*avatarForward, error) {
	base, err := url.Parse(e.media.reg.Config().BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errors.New("contentkit: Config.Avatars.Primary needs Config.Media.BaseURL, the media host avatars are shown at")
	}
	c := e.cfg.Media.CommitLimit
	return &avatarForward{kind: kind.Name, primary: strings.TrimRight(a.Primary, "/"), key: a.Key, media: base,
		gate: &gate.Gate{Auth: e.auth, Log: e.log}, client: &http.Client{Timeout: forwardTimeout}, log: e.log,
		commits: media.NewCommitLimiter(media.RateLimit{PerSecond: c.PerSecond, Burst: c.Burst, Disabled: c.Disabled, Redis: deps.Redis})}, nil
}

type forwardKey struct{}

// forwarded is what a request to forward names: its module's path and the
// avatar's person.
type forwarded struct {
	path   string
	person string
	body   []byte
}

// wrap serves module's avatar requests by forwarding them and passes every
// other request to next.
func (f *avatarForward) wrap(module httpapi.Module, next http.Handler) http.Handler {
	forward := f.gate.Guard(httpapi.Spec{Module: module, Auth: httpapi.User}, http.HandlerFunc(f.serve))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fw, ok, err := f.match(module, r)
		switch {
		case err != nil:
			fail(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, httpapi.BodyError(err), 0)
		case ok:
			forward.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), forwardKey{}, fw)))
		case next != nil:
			next.ServeHTTP(w, r)
		default:
			fail(w, http.StatusNotFound, httpapi.CodeNotFound, httpapi.Message(httpapi.CodeNotFound), 0)
		}
	})
}

// maxForwardBody bounds a presign or commit body read to find its item.
const maxForwardBody = 1 << 20

// match reports an avatar request: a presign or commit of the avatar kind,
// or a read of an avatar item. It restores the body it reads.
func (f *avatarForward) match(module httpapi.Module, r *http.Request) (forwarded, bool, error) {
	switch {
	case module == httpapi.Upload && r.Method == http.MethodPost && (r.URL.Path == "/presign" || r.URL.Path == "/commit"):
		body, err := io.ReadAll(io.LimitReader(r.Body, maxForwardBody+1))
		if err != nil {
			return forwarded{}, false, err
		}
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		var b struct {
			Ref media.RefBody `json:"ref"`
		}
		if len(body) > maxForwardBody || json.Unmarshal(body, &b) != nil || b.Ref.Kind != f.kind {
			return forwarded{}, false, nil
		}
		return forwarded{path: module.Prefix() + r.URL.Path, person: b.Ref.ID, body: body}, true, nil
	case module == httpapi.Media && r.Method == http.MethodGet:
		p := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(p) != 2 || p[0] != f.kind {
			return forwarded{}, false, nil
		}
		return forwarded{path: module.Prefix() + r.URL.Path, person: p[1]}, true, nil
	}
	return forwarded{}, false, nil
}

// serve forwards one avatar request of a signed-in person.
func (f *avatarForward) serve(w http.ResponseWriter, r *http.Request) {
	fw := r.Context().Value(forwardKey{}).(forwarded)
	actor := gate.Actor(r.Context())
	if actor.AccountID() != fw.person {
		fail(w, http.StatusForbidden, httpapi.CodeForbidden, "a person edits only their own avatar", 0)
		return
	}
	if strings.HasSuffix(fw.path, "/commit") {
		if ok, wait := f.commits.Allow(r.Context(), actor.AccountID()); !ok {
			fail(w, http.StatusTooManyRequests, httpapi.CodeRateLimited, httpapi.Message(httpapi.CodeRateLimited), wait)
			return
		}
	}
	target := f.primary + fw.path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	var body io.Reader
	if fw.body != nil {
		body = bytes.NewReader(fw.body)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		fail(w, http.StatusInternalServerError, httpapi.CodeInternal, httpapi.Message(httpapi.CodeInternal), 0)
		return
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	for _, h := range []string{"Content-Type", "Accept-Language"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.log.WarnContext(r.Context(), "contentkit: the avatar primary is unreachable", "err", err)
		fail(w, http.StatusServiceUnavailable, httpapi.CodeUnavailable, unavailable, 5*time.Second)
		return
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		fail(w, http.StatusServiceUnavailable, httpapi.CodeUnavailable, unavailable, 5*time.Second)
		return
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// The person passed this site's check: the primary refused the key.
		f.log.ErrorContext(r.Context(), "contentkit: the avatar primary refused Config.Avatars.Key", "status", resp.StatusCode)
		fail(w, http.StatusServiceUnavailable, httpapi.CodeUnavailable, unavailable, 5*time.Second)
		return
	case resp.StatusCode == http.StatusOK && r.Method == http.MethodGet:
		out = f.rehost(out)
	}
	for _, h := range []string{"Content-Type", "Cache-Control", "Retry-After"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// rehost moves a read's file URLs to this site's media host, which serves
// the primary's files: the gateway's same-site resource policy keeps
// another site's from loading in this site's pages.
func (f *avatarForward) rehost(body []byte) []byte {
	var r media.ReadResult
	if err := json.Unmarshal(body, &r); err != nil {
		return body
	}
	at := func(u *string) {
		p, err := url.Parse(*u)
		if err == nil && p.IsAbs() && strings.HasPrefix(p.Path, layout.URLPrefix) {
			p.Scheme, p.Host = f.media.Scheme, f.media.Host
			*u = p.String()
		}
	}
	for i := range r.Files {
		at(&r.Files[i].URL)
		at(&r.Files[i].EditorURL)
	}
	for i := range r.Public {
		for j := range r.Public[i].Renditions {
			at(&r.Public[i].Renditions[j].URL)
		}
	}
	for i := range r.Previews {
		at(&r.Previews[i])
	}
	out, err := json.Marshal(r)
	if err != nil {
		return body
	}
	return out
}

// unavailable is a forward's message while the primary can't serve it.
const unavailable = "avatars are unavailable; retry"

// fail answers the media modules' error body; retry > 0 sets retry_after.
func fail(w http.ResponseWriter, status int, code, msg string, retry time.Duration) {
	out := media.ErrorReply{Error: msg, Code: code}
	if retry > 0 {
		out.RetryAfter = int(math.Ceil(retry.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(out.RetryAfter))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}
