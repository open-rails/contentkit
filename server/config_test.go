package server

import (
	"testing"
	"time"

	contentkit "github.com/open-rails/contentkit"
)

const sample = `
engine:
  tenant: site
  database: {schema: site_contentkit}
  content:
    languages: [en, ja]
    kinds:
      gallery: {route: g, presentation: reader, versions: {languages: one}, entities: [tag]}
    entity_kinds:
      tag: {route: tag}
  interactions:
    anonymous: {comments: true}
    limits: {comment: [{count: 5, per: 5m}]}
  media:
    base_url: https://media.site.test
    kinds:
      - name: gallery
        uploads: [{path: "originals/{name}", types: [image/png], max_bytes: 1048576, max: 10}]
    delivery: {mode: url}
  clickhouse: {database: site_signal, addr: "clickhouse:9000", username: contentkit}
auth:
  token: {issuer: https://contentkit.site.test, issued_audiences: [contentkit]}
  resource: {id: https://contentkit.site.test}
openrails: {base_url: https://billing.site.test}
s3: {bucket: media, endpoint: http://minio:9000, use_path_style: true}
routes: [content, media, upload, codes]
prefix: /api/contentkit
addr: ":8080"
drain_delay: 5s
`

// The file is plain data in every section's own field names (media's
// registry included); secrets come only from the environment.
func TestLoad(t *testing.T) {
	env := map[string]string{"CONTENTKIT_MEDIA_TOKEN_KEY": "k1:c2VjcmV0", "CONTENTKIT_OPENRAILS_API_KEY": "or_key", "CONTENTKIT_S3_SECRET_ACCESS_KEY": "s3"}
	c, err := Load([]byte(sample), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	e := c.Engine
	if e.Tenant != "site" || e.Database.Schema != "site_contentkit" || e.Content.Kinds["gallery"].Route != "g" || e.Content.EntityKinds["tag"].Route != "tag" ||
		!e.Interactions.Anonymous.Comments || e.Interactions.Limits.Comment[0].Per != 5*time.Minute || e.ClickHouse.Addr != "clickhouse:9000" {
		t.Fatalf("engine %+v", e)
	}
	m := e.Media
	if m == nil || m.BaseURL != "https://media.site.test" || len(m.Kinds) != 1 || m.Kinds[0].Uploads[0].MaxBytes != 1<<20 || m.Delivery.SigningKey != "k1:c2VjcmV0" {
		t.Fatalf("media %+v", m)
	}
	if c.Auth.Token.Issuer != "https://contentkit.site.test" || c.Auth.Resource.ID != "https://contentkit.site.test" || c.OpenRails.APIKey != "or_key" ||
		c.S3.SecretAccessKey != "s3" || !c.S3.UsePathStyle || c.DrainDelay != 5*time.Second || len(c.Routes) != 4 {
		t.Fatalf("server %+v", c)
	}
	if _, err := Load([]byte("engine: {tenant: x, unknown: 1}"), func(string) string { return "" }); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestLoadScopedAuthorization(t *testing.T) {
	c, err := Load([]byte(`
engine:
  scope:
    tenant_id: 11111111-1111-4111-8111-111111111111
    app_id: 22222222-2222-4222-8222-222222222222
authorization:
  issuer: https://accounts.example.test
  app_group_id: 22222222-2222-4222-8222-222222222222
  tenant_group_id: 11111111-1111-4111-8111-111111111111
`), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if c.Authorization == nil || c.Authorization.Issuer != "https://accounts.example.test" || c.Authorization.AppGroupID != c.Engine.Scope.AppID.String() || c.Authorization.TenantGroupID != c.Engine.Scope.TenantID.String() {
		t.Fatalf("scoped authorization was not decoded: %+v", c.Authorization)
	}
}

// Retired offer configuration must fail visibly rather than silently preserve
// an independent billing catalog in the standalone server.
func TestRetiredOfferConfigurationIsRejected(t *testing.T) {
	if _, err := Load([]byte("engine:\n  offers: {currency: USD}\n"), func(string) string { return "" }); err == nil {
		t.Fatal("retired offers configuration was accepted")
	}
	if _, err := contentkit.ParseRouteGroups([]string{"offers"}); err == nil {
		t.Fatal("retired offers route group was accepted")
	}
}
