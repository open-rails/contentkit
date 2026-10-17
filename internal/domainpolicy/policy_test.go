package domainpolicy_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/domainpolicy"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
)

func TestRetiredInternalMarkerStaysFailClosed(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	table := pgx.Identifier{schema, "content_groups"}.Sanitize()
	id := contentref.NewID()
	if _, err := pool.Exec(ctx, `INSERT INTO `+table+`(tenant_id,id,kind,slug,internal) VALUES('policy',$1,'tag','private',true)`, id); err != nil {
		t.Fatal(err)
	}
	domain := kinds.Domain{Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "private"}, Visible: false}, {Group: kinds.GroupRef{Kind: "tag", Slug: "unseeded"}, Visible: false}}}
	active, err := domainpolicy.Resolve(ctx, pool, table, "policy", domain, []string{"gallery"})
	if err != nil || active["gallery"].MissingHard || active.Allows("gallery", []string{id}) || !active.Allows("gallery", nil) {
		t.Fatalf("active marker: %+v %v", active, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE `+table+` SET state='deleted' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	retired, err := domainpolicy.Resolve(ctx, pool, table, "policy", domain, []string{"gallery"})
	if err != nil || !retired["gallery"].MissingHard || retired.Allows("gallery", nil) || !retired.Allows("blog", []string{id}) {
		t.Fatalf("retired marker widened policy: %+v %v", retired, err)
	}
	domain.Filters = []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "unseeded"}, Visible: false}}
	optional, err := domainpolicy.Resolve(ctx, pool, table, "policy", domain, []string{"gallery"})
	if err != nil || optional["gallery"].MissingHard || !optional.Allows("gallery", nil) {
		t.Fatalf("unseeded optional filter blocked content: %+v %v", optional, err)
	}
}

func TestInactiveInternalCannotBeShadowedByPublicReplacement(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	table := pgx.Identifier{schema, "content_groups"}.Sanitize()
	cfg := kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Groups: []string{"tag"}}}, GroupKinds: map[string]kinds.GroupKind{"tag": {Route: "tags"}}}
	legacy, err := groups.New(groups.Options{Pool: pool, Schema: schema, Tenant: "policy", Kinds: cfg})
	if err != nil {
		t.Fatal(err)
	}
	old, err := legacy.CreateGroups(ctx, []groups.GroupInput{{Kind: "tag", Slug: "private", Internal: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.DeleteGroups(ctx, []groups.GroupID{old[0].ID}); err != nil {
		t.Fatal(err)
	}
	d := kinds.Domain{Name: "main", Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "private"}, Visible: false}}}
	cfg.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"gallery"}, Domains: []kinds.Domain{d}}
	current, err := groups.New(groups.Options{Pool: pool, Schema: schema, Tenant: "policy", Kinds: cfg})
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := current.CreateGroups(ctx, []groups.GroupInput{{Kind: "tag", Slug: "private"}})
	if err != nil {
		t.Fatal(err)
	}
	assertClosed := func() {
		t.Helper()
		got, err := domainpolicy.Resolve(ctx, pool, table, "policy", d, []string{"gallery"})
		if err != nil || !got["gallery"].MissingHard || got.Allows("gallery", nil) || len(got["gallery"].Toggles) > 0 {
			t.Fatalf("inactive internal marker was shadowed: %+v %v", got, err)
		}
	}
	assertClosed()
	if _, err := current.DeleteGroups(ctx, []groups.GroupID{replacement[0].ID}); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := current.ImportGroups(ctx, tx, []groups.ImportGroup{{ID: groups.GroupID(contentref.NewID()), Kind: "tag", Slug: "private"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertClosed()
}

func TestSharedFiltersKeepRequirementsKindScoped(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	table := pgx.Identifier{schema, "content_groups"}.Sanitize()
	yaoi, restricted, furry := contentref.NewID(), contentref.NewID(), contentref.NewID()
	if _, err := pool.Exec(ctx, `INSERT INTO `+table+`(tenant_id,id,kind,slug,internal) VALUES
('policy',$1,'tag','yaoi',false),('policy',$2,'tag','restricted',true),('policy',$3,'tag','furry',false)`, yaoi, restricted, furry); err != nil {
		t.Fatal(err)
	}
	d := kinds.Domain{RequireEntities: map[string][]kinds.GroupRef{"gallery": {{Kind: "tag", Slug: "yaoi"}}}, Filters: []kinds.ContentFilter{
		{Group: kinds.GroupRef{Kind: "tag", Slug: "restricted"}, Visible: false},
		{Group: kinds.GroupRef{Kind: "tag", Slug: "furry"}, Visible: false},
		{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, Visible: true},
	}}
	resolve := func() domainpolicy.Policies {
		t.Helper()
		p, err := domainpolicy.Resolve(ctx, pool, table, "policy", d, []string{"gallery", "blog"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := resolve()
	if p.Allows("gallery", nil) || !p.Allows("gallery", []string{yaoi}) || !p.Allows("blog", nil) || len(p["blog"].Required) != 0 {
		t.Fatalf("gallery requirement leaked to blog or was not enforced: %+v", p)
	}
	for _, kind := range []string{"gallery", "blog"} {
		if p.Allows(kind, []string{yaoi, restricted}) || len(p[kind].Toggles) != 1 || p[kind].ToggleRefs[kinds.GroupRef{Kind: "tag", Slug: "furry"}] != furry {
			t.Fatalf("shared exclusion/toggle inventory is incorrect for %s: %+v", kind, p[kind])
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE `+table+` SET state='deleted' WHERE id=$1`, restricted); err != nil {
		t.Fatal(err)
	}
	p = resolve()
	for _, kind := range []string{"gallery", "blog"} {
		if !p[kind].MissingHard || p.Allows(kind, []string{yaoi}) {
			t.Fatalf("retired internal exclusion did not fail %s closed: %+v", kind, p[kind])
		}
	}
}
