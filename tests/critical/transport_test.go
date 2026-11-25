package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/server"
	"github.com/open-rails/openrails/billing"
)

// standaloneAuth is w's AuthKit configuration for the server, its HTTP
// declaring no client-IP posture: the server derives it from the engine's.
func standaloneAuth(t *testing.T, w *world) authkit.Config {
	t.Helper()
	var c authkit.Config
	authtest.Replica(t, w.auth, authtest.WithConfig(func(cfg *authkit.Config) { c = *cfg }))
	h := *c.HTTP
	h.DirectPeerIP = false
	c.HTTP = &h
	return c
}

// serveStandalone runs a's app as the standalone server on loopback until the
// test ends; it returns the server's base URL.
func serveStandalone(t *testing.T, w *world, a *application, configure func(*server.Config)) string {
	t.Helper()
	cfg := server.Config{Engine: a.cfg, Auth: standaloneAuth(t, w), Authorization: &server.AuthorizationConfig{Issuer: authtest.Issuer, AppGroupID: a.cfg.Scope.AppID.String()},
		Prefix: "/content", DrainDelay: time.Millisecond, ShutdownTimeout: 5 * time.Second}
	cfg.Engine.Database.SkipMigrations = true
	if configure != nil {
		configure(&cfg)
	}
	standalone, err := server.New(t.Context(), cfg, server.Deps{Postgres: w.pool})
	must(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	base := "http://" + listener.Addr().String()
	run, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- standalone.Serve(run, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("standalone shutdown: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("standalone did not stop")
		}
		must(t, standalone.Close(context.Background()))
	})
	waitFor(t, func() bool {
		res, err := http.Get(base + "/readyz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == http.StatusNoContent || res.StatusCode == http.StatusOK
	})
	return base
}

func staffKey(t *testing.T, w *world, a *application) string {
	t.Helper()
	persona, err := w.auth.Persona("app")
	must(t, err)
	key, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "Transport staff", Role: persona.OwnerRole()})
	must(t, err)
	return key.Secret
}

// answer is a request's status and error code.
func answer(t *testing.T, method, url string, header http.Header, body []byte) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
	must(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	defer res.Body.Close()
	var reply struct{ Code string }
	_ = json.NewDecoder(res.Body).Decode(&reply)
	return res.StatusCode, reply.Code, res.Header
}

func TestStandaloneClientAddress(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	_, err := a.engine.Client().PostModerationLocationRules(t.Context(), content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "203.0.113.0/24"}},
		Target: "site", Domains: []string{"main"}, Enabled: true, Note: "blocked network"})
	must(t, err)
	base := serveStandalone(t, w, a, func(c *server.Config) {
		c.Engine.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	})
	t.Run("CK-J02-S40_a_trusted_proxy_names_the_client_and_nobody_else_can", func(t *testing.T) {
		kinds := func(t *testing.T, at string, name, value string) (int, string) {
			t.Helper()
			status, code, _ := answer(t, http.MethodGet, at+"/content/v1/kinds", http.Header{name: {value}}, nil)
			return status, code
		}
		for _, c := range []struct {
			name, header, value string
			status              int
			code                string
		}{
			{"a blocked client", "X-Forwarded-For", "203.0.113.7", http.StatusNotFound, "not_found"},
			{"another client", "X-Forwarded-For", "198.51.100.1", http.StatusOK, ""},
			{"a client's own claim is never chosen", "X-Forwarded-For", "198.51.100.1, 203.0.113.7", http.StatusNotFound, "not_found"},
			{"trusted hops are skipped", "X-Forwarded-For", "203.0.113.7, 127.0.0.5", http.StatusNotFound, "not_found"},
			{"Forwarded", "Forwarded", `for=203.0.113.7;proto=https`, http.StatusNotFound, "not_found"},
			{"Forwarded IPv6", "Forwarded", `for="[2001:db8::7]:443"`, http.StatusOK, ""},
			{"a malformed chain", "X-Forwarded-For", "203.0.113.7, not-an-address", http.StatusBadRequest, "invalid_request"},
		} {
			if status, code := kinds(t, base, c.header, c.value); status != c.status || code != c.code {
				t.Errorf("%s: %d %s, want %d %s", c.name, status, code, c.status, c.code)
			}
		}
		if status, code, _ := answer(t, http.MethodGet, base+"/content/v1/kinds", http.Header{"Forwarded": {"for=198.51.100.1"}, "X-Forwarded-For": {"198.51.100.1"}}, nil); status != http.StatusBadRequest || code != "invalid_request" {
			t.Errorf("both forwarding headers: %d %s", status, code)
		}
		// The embedded mount trusts no proxy: its peer, loopback, is the client.
		if status, code := kinds(t, a.server.URL, "X-Forwarded-For", "203.0.113.7"); status != http.StatusOK {
			t.Errorf("an untrusted peer chose its address: %d %s", status, code)
		}
	})
}

func TestAnonymousIPv6Prefix(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) {
		c.Interactions.Anonymous.Reactions = true
		c.Interactions.Limits.Reaction = []ck.Rate{{Count: 3, Per: time.Hour}}
	})
	a := w.apps[0]
	one := publishedArticle(t, a, "IPv6 prefix one")
	two := publishedArticle(t, a, "IPv6 prefix two")
	visitor := func(ip string) *ck.Client { return a.engine.VisitorClient(netip.MustParseAddr(ip), "") }
	t.Run("CK-J02-S41_an_IPv6_64_is_one_anonymous_visitor", func(t *testing.T) {
		like := func(t *testing.T, ip, id string) content.ReactionCounts {
			t.Helper()
			got, err := visitor(ip).PostContentLike(t.Context(), id, ck.PostContentLikeQuery{})
			must(t, err)
			return got
		}
		if got := like(t, "2001:db8:1:2::1", one.ID); got.Likes == nil || *got.Likes != 1 || got.Mine != 1 {
			t.Fatalf("first like: %+v", got)
		}
		// Another address in the same /64 is the same visitor: its like is
		// the one already counted.
		if got := like(t, "2001:db8:1:2:ffff::9", one.ID); got.Likes == nil || *got.Likes != 1 || got.Mine != 1 {
			t.Fatalf("a rotated address liked again: %+v", got)
		}
		if got := like(t, "2001:db8:1:3::1", one.ID); got.Likes == nil || *got.Likes != 2 {
			t.Fatalf("another /64 is another visitor: %+v", got)
		}
		// The limit counts the /64 too: its third reaction fits, a fourth
		// from yet another of its addresses does not.
		like(t, "2001:db8:1:2::3", two.ID)
		_, err := visitor("2001:db8:1:2::4").PostContentDislike(t.Context(), two.ID, ck.PostContentDislikeQuery{})
		requireError(t, err, http.StatusTooManyRequests, "rate_limited")
		like(t, "2001:db8:1:3::2", two.ID)
		like(t, "192.0.2.1", two.ID)
	})
}

// panickingBody is a host BodySanitizer with a bug: it panics on a marked
// body.
type panickingBody struct{}

func (panickingBody) SanitizeHTML(_ context.Context, raw string) (string, error) {
	if strings.Contains(raw, "PANIC") {
		panic("host body processor bug")
	}
	return raw, nil
}

func TestFailureContainment(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	cfg, deps := a.cfg, a.deps
	cfg.Database.SkipMigrations = true
	deps.BodySanitizer = panickingBody{}
	engine, err := ck.New(t.Context(), cfg, deps)
	must(t, err)
	t.Cleanup(func() { must(t, engine.Close(context.Background())) })
	srv := mountEngine(t, engine)
	secret := staffKey(t, w, a)
	staff := ck.NewClient(srv.URL+"/content", &http.Client{Transport: bearerTransport{secret, http.DefaultTransport}})
	article := func(body string) catalog.ContentInput {
		return catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Contained", Body: body}}}}}
	}
	t.Run("CK-J01-S11_a_panic_answers_500_over_HTTP_and_in_process_and_serving_continues", func(t *testing.T) {
		_, err := staff.PostContents(t.Context(), article("PANIC over HTTP"))
		requireError(t, err, http.StatusInternalServerError, "internal_error")
		raw, _ := json.Marshal(article("PANIC raw"))
		status, code, header := answer(t, http.MethodPost, srv.URL+"/content/v1/contents", http.Header{"Authorization": {"Bearer " + secret}, "Content-Type": {"application/json"}}, raw)
		if status != http.StatusInternalServerError || code != "internal_error" || header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Content-Type") != "application/json" {
			t.Fatalf("raw panic answer: %d %s %v", status, code, header)
		}
		// The in-process client shares the host's goroutine: the panic is
		// its error, not the host's crash.
		_, err = engine.Client().PostContents(t.Context(), article("PANIC in process"))
		requireError(t, err, http.StatusInternalServerError, "internal_error")
		for _, c := range []*ck.Client{staff, engine.Client()} {
			item, err := c.PostContents(t.Context(), article("A well-formed body"))
			must(t, err)
			got, err := c.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
			must(t, err)
			if got.ID != item.ID {
				t.Fatalf("serving after a panic: %+v", got)
			}
		}
	})
	t.Run("CK-J01-S11_an_empty_create_is_400", func(t *testing.T) {
		status, code, _ := answer(t, http.MethodPost, srv.URL+"/content/v1/contents", http.Header{"Authorization": {"Bearer " + secret}, "Content-Type": {"application/json"}}, nil)
		if status != http.StatusBadRequest || code != "invalid_request" {
			t.Fatalf("empty body: %d %s", status, code)
		}
		_, err := staff.PostContents(t.Context(), catalog.ContentInput{})
		requireError(t, err, http.StatusBadRequest, "invalid_request")
	})
}

func TestStartupRefusesUnsafeTransport(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	prepared := func() (ck.Config, ck.Deps) {
		cfg := a.cfg
		cfg.Database.SkipMigrations = true
		return cfg, a.deps
	}
	refused := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want an error naming %q", err, want)
		}
	}
	t.Run("CK-J01-S12_the_engine_refuses_shared_queues_plain_credentials_and_bad_proxies", func(t *testing.T) {
		for name, c := range map[string]struct {
			mutate func(*ck.Config)
			want   string
		}{
			"River in the catalog schema": {func(c *ck.Config) { c.Database.RiverSchema = c.Database.Schema }, "both schema"},
			"accounts in the River schema": {func(c *ck.Config) {
				c.Accounts = &ck.AccountsConfig{Schema: c.Database.Schema + "_river"}
			}, "both schema"},
			"an IPv4-mapped trusted proxy": {func(c *ck.Config) { c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("::ffff:10.0.0.0/104")} }, "IPv4-mapped"},
			"a ClickHouse CA without TLS":  {func(c *ck.Config) { c.ClickHouse.CAFile = "ca.pem" }, "needs TLS"},
		} {
			cfg, deps := prepared()
			c.mutate(&cfg)
			engine, err := ck.New(t.Context(), cfg, deps)
			if err == nil {
				must(t, engine.Close(context.Background()))
			}
			t.Run(name, func(t *testing.T) { refused(t, err, c.want) })
		}
		cfg, _ := prepared()
		cfg.Media = &ck.MediaConfig{Config: media.Config{BaseURL: "http://media.example"}}
		_, err := cfg.MediaRegistry()
		refused(t, err, "plain http")
		// A plain base off loopback sends nothing: the credential stays home.
		sent := false
		remote := ck.NewClient("http://contentkit.example/content", &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			sent = true
			return nil, errors.New("sent")
		})})
		_, err = remote.GetKinds(t.Context())
		refused(t, err, "plain http")
		if sent {
			t.Fatal("a plain http client sent its request")
		}
	})
	authConfig := standaloneAuth(t, w)
	build := func(t *testing.T, mutate func(*server.Config)) error {
		cfg, _ := prepared()
		auth := authConfig
		h := *auth.HTTP
		auth.HTTP = &h
		sc := server.Config{Engine: cfg, Auth: auth, Authorization: &server.AuthorizationConfig{Issuer: authtest.Issuer, AppGroupID: cfg.Scope.AppID.String()}}
		mutate(&sc)
		s, err := server.New(t.Context(), sc, server.Deps{Postgres: w.pool})
		if err == nil {
			must(t, s.Close(context.Background()))
		}
		return err
	}
	t.Run("CK-J01-S12_the_server_refuses_plain_OpenRails_and_shared_schemas", func(t *testing.T) {
		refused(t, build(t, func(c *server.Config) {
			c.OpenRails = server.OpenRailsConfig{BaseURL: "http://openrails.example", APIKey: "merchant-key"}
		}), "plain http")
		refused(t, build(t, func(c *server.Config) { c.Auth.Database.Schema = c.Engine.Database.Schema }), "are both")
		refused(t, build(t, func(c *server.Config) { c.Auth.Database.RiverSchema = c.Engine.Database.Schema + "_river" }), "are both")
		refused(t, build(t, func(c *server.Config) { c.Auth.Database.RiverSchema = c.Engine.Database.Schema }), "are both")
		// AuthKit's tables and its River may share a schema: that is AuthKit's to allow.
		must(t, build(t, func(c *server.Config) { c.Auth.Database.RiverSchema = c.Auth.Database.Schema }))
		refused(t, build(t, func(c *server.Config) {
			c.OpenRails = server.OpenRailsConfig{BaseURL: "http://127.0.0.1:1", APIKey: "merchant-key"}
		}), "merchant_id")
		must(t, build(t, func(c *server.Config) {
			c.OpenRails = server.OpenRailsConfig{BaseURL: "http://127.0.0.1:1", MerchantID: billing.MerchantID(uuid.New()), APIKey: "merchant-key"}
		}))
	})
	t.Run("CK-J01-S15_the_server_gives_AuthKit_the_engine_proxies", func(t *testing.T) {
		proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("173.245.48.0/20")}
		refused(t, build(t, func(c *server.Config) {
			c.Engine.TrustedProxies = proxies
			c.Auth.HTTP.DirectPeerIP = true
		}), "direct_peer_ip")
		refused(t, build(t, func(c *server.Config) { c.Auth.HTTP.TrustedProxies = []string{"10.0.0.0/8"} }), "differ")
		refused(t, build(t, func(c *server.Config) {
			c.Engine.TrustedProxies = proxies
			c.Auth.HTTP.TrustedProxies = []string{"192.168.0.0/16"}
		}), "differ")
		must(t, build(t, func(c *server.Config) { c.Engine.TrustedProxies = proxies }))
		must(t, build(t, func(c *server.Config) {
			c.Engine.TrustedProxies = proxies
			c.Auth.HTTP.TrustedProxies = []string{"10.0.0.0/8"}
			c.Auth.HTTP.CloudflareProxies = []string{"173.245.48.0/20"}
		}))
		must(t, build(t, func(c *server.Config) { c.Auth.HTTP.DirectPeerIP = true }))
	})
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
