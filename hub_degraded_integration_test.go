package contentkit

import (
	"context"
	"slices"
	"testing"

	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

// When the per-subject view state cannot be read, personalized search serves
// the content ranking's page instead of failing search.
func TestPersonalizedSearchSurvivesSignalReadFailure(t *testing.T) {
	pool := testPG(t)
	chEnv := signaltest.FromEnv(t)
	ctx := context.Background()
	schema := keywordSchema(t, ctx, pool)
	upsertDocs(t, ctx, pool, schema,
		doc("gallery", cid(1), "en", "two factor authentication guide", nil, nil),
		doc("gallery", cid(2), "en", "two factor backup codes", nil, nil),
		doc("gallery", cid(3), "en", "two factor recovery", nil, nil),
	)
	const db = "contentkit_hub_degraded_test"
	ch := chEnv.Fresh(t, db)
	t.Cleanup(func() { chEnv.Drop(t, chEnv.Open(t, ""), db) })
	hub, err := NewEmbedded(EmbeddedConfig{PG: pool, PGSchema: schema, CH: ch, CHDatabase: db, Tenant: testTenant})
	if err != nil {
		t.Fatal(err)
	}
	opts := SearchOptions{Language: "en", ContentKinds: []string{"gallery"}, Limit: 2}
	plain, err := hub.Search(ctx, "two factor", HubSearchOptions{SearchOptions: opts})
	if err != nil {
		t.Fatal(err)
	}
	// Popularity stays readable; the subject state table does not.
	if err := ch.Exec(ctx, "DROP TABLE "+db+".subject_content_state"+chEnv.OnCluster()+" SYNC"); err != nil {
		t.Fatal(err)
	}
	pers, err := hub.Search(ctx, "two factor", HubSearchOptions{SearchOptions: opts,
		Personalize: &Personalization{Subject: signal.Subject{UserID: "u1"}, DemoteSeen: true, PopularityWindow: signal.AllTime()}})
	if err != nil {
		t.Fatalf("personalized search failed with the signal plane degraded: %v", err)
	}
	ids := func(r SearchResult) []string {
		var out []string
		for _, h := range r.Hits {
			out = append(out, h.ContentID)
		}
		return out
	}
	if !slices.Equal(ids(pers), ids(plain)) || pers.HasMore != plain.HasMore {
		t.Fatalf("fallback page %v (more %v), want the content ranking %v (more %v)", ids(pers), pers.HasMore, ids(plain), plain.HasMore)
	}
}
