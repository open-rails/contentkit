package critical_test

import (
	"slices"
	"testing"
	"time"

	signalstore "github.com/open-rails/contentkit/internal/signalstore"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

func TestSearchAttributionUsesExactOriginScope(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	store, err := signalstore.NewStore(a.deps.ClickHouse, a.cfg.ClickHouse.Database)
	must(t, err)
	tenant, otherTenant := a.cfg.Scope.AppID.String(), w.apps[1].cfg.Scope.AppID.String()
	v7 := func() string { id, err := uuid.NewV7(); must(t, err); return id.String() }
	ref := contentref.NewVersion(tenant, "article", v7(), v7())
	otherVersion := ref.WithVersion(v7())
	otherContent := contentref.NewVersion(tenant, "article", v7(), v7())
	user, otherUser := signal.Subject{UserID: uuid.NewString()}, signal.Subject{UserID: uuid.NewString()}
	at := time.Now().UTC().Truncate(time.Second)
	accessAt := func(at time.Time) *signal.AccessContext {
		return &signal.AccessContext{ObservedAt: at, ContentAccess: "public", ViewerAccess: "full", PolicyRevision: "attribution-fixture", Audience: "public", EligibilityKnown: true}
	}
	render, query, click, visit := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	must(t, store.RecordExposures(t.Context(), tenant, []signal.Exposure{{RenderID: render, QueryID: query,
		SearchQuery: "  Nebula \t Orchard  ", Surface: signal.SurfaceSearch, Ranker: "relevance", Language: "en", Subject: user,
		Stage: signal.StageVisible, Revision: 1, OccurredAt: at,
		Shown: []signal.Placement{{ContentRef: ref, Position: 7, Access: accessAt(at)}},
	}}))
	clicked := signal.Signal{ContentRef: ref, Subject: user, Type: signal.TypeClick, EventID: click, Revision: 1, OccurredAt: at}
	clicked = clicked.WithAttribution(signal.Attribution{RenderID: render, Surface: signal.SurfaceSearch, Position: 7})
	must(t, store.RecordSignals(t.Context(), tenant, []signal.Signal{clicked, clicked}))
	measured := func(ref contentref.ContentRef, subject signal.Subject, origin, visit string, active uint32) signal.Signal {
		t.Helper()
		m, err := signalstore.MeasurePages([]signal.PageEngagement{{Index: 0, Ranges: []signal.CoverageRange{{From: 0, To: 10000}}, ActiveMS: active}}, 1, active)
		must(t, err)
		m.VersionID, m.VisitID, m.SegmentID, m.OriginClickID = ref.Version(), visit, uuid.NewString(), origin
		return signal.Signal{ContentRef: ref, Subject: subject, Type: signal.TypeEngagement, EventID: uuid.NewString(), Revision: 1,
			OccurredAt: at, Measurement: &m, Access: accessAt(at)}
	}
	first := measured(ref, user, click, visit, 1000)
	second := measured(otherVersion, user, click, visit, 2000)
	// Analytics input is not authorization. These well-formed declarations are
	// retained, but exact tenant/subject/content joins must exclude them.
	foreignUser := measured(ref, otherUser, click, uuid.NewString(), 4000)
	foreignContent := measured(otherContent, user, click, uuid.NewString(), 8000)
	direct := measured(ref, user, "", uuid.NewString(), 16000)
	must(t, store.RecordSignals(t.Context(), tenant, []signal.Signal{first, second, first, second, foreignUser, foreignContent, direct}))
	foreignTenantRef := contentref.NewVersion(otherTenant, ref.ContentKind, ref.ContentID, ref.Version())
	must(t, store.RecordSignals(t.Context(), otherTenant, []signal.Signal{measured(foreignTenantRef, user, click, uuid.NewString(), 32000)}))
	page, err := store.Attribution(t.Context(), tenant, signal.AttributionOptions{Stage: signal.StageVisible, ClickLimit: 1})
	must(t, err)
	if len(page.Renders) != 1 || len(page.Renders[0].Clicks) != 1 {
		t.Fatalf("one replayed click did not remain one: %+v", page)
	}
	result := page.Renders[0]
	if result.SearchQuery != "nebula orchard" || result.QueryID != query || result.Shown[0].Position != 7 {
		t.Fatalf("query/position export changed: %+v", result)
	}
	got := result.Clicks[0]
	if got.EventID != click || !got.Exposed || len(got.Visits) != 1 {
		t.Fatalf("exact origin joined unrelated navigation: %+v", got)
	}
	joined := got.Visits[0]
	if joined.VisitID != visit || joined.ActiveMS != 3000 || joined.Coverage != 1 || !joined.Completed || len(joined.VersionIDs) != 2 || !slices.Contains(joined.VersionIDs, ref.Version()) || !slices.Contains(joined.VersionIDs, otherVersion.Version()) {
		t.Fatalf("one visit across versions duplicated or borrowed engagement: %+v", joined)
	}
	// Analytics query text has no age expiry, just like the engagement it
	// explains. The separate recent-search autocomplete list expires after
	// 180 days of last use; that rule does not remove historical attribution.
	old := at.Add(-181 * 24 * time.Hour)
	oldRender := uuid.NewString()
	must(t, store.RecordExposures(t.Context(), tenant, []signal.Exposure{{RenderID: oldRender, QueryID: uuid.NewString(), SearchQuery: "retained historical query",
		Surface: signal.SurfaceSearch, Language: "en", Subject: otherUser, Stage: signal.StageVisible, Revision: 1, OccurredAt: old,
		Shown: []signal.Placement{{ContentRef: ref, Position: 2, Access: accessAt(old)}},
	}}))
	rows, err := a.deps.ClickHouse.Query(t.Context(), "SELECT search_query FROM "+a.cfg.ClickHouse.Database+".exposures FINAL WHERE tenant=? AND render_id=?", tenant, oldRender)
	must(t, err)
	if !rows.Next() {
		rows.Close()
		t.Fatal("historical analytics exposure was removed")
	}
	var expired string
	must(t, rows.Scan(&expired))
	must(t, rows.Err())
	rows.Close()
	if expired != "retained historical query" {
		t.Fatalf("historical analytics query did not retain its source text: %q", expired)
	}
	all, err := store.Attribution(t.Context(), tenant, signal.AttributionOptions{Stage: signal.StageVisible})
	must(t, err)
	found := false
	for _, r := range all.Renders {
		if r.RenderID == oldRender {
			found = true
			if r.SearchQuery != "retained historical query" {
				t.Fatalf("historical query disappeared from attribution export: %q", r.SearchQuery)
			}
		}
	}
	if !found {
		t.Fatal("historical query exposure was absent from attribution export")
	}
}
