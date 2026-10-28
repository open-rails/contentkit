package critical_test

import (
	"math"
	"testing"
	"time"

	signalstore "github.com/open-rails/contentkit/internal/signalstore"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

func TestDailyParticipationPreparation(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	store, err := signalstore.NewStore(a.deps.ClickHouse, a.cfg.ClickHouse.Database)
	must(t, err)
	tenant := a.cfg.Scope.AppID.String()
	scope := signal.ActivityScope{Domain: "main", Language: "en"}
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -5)
	end := day.AddDate(0, 0, 2)
	id := func() string { value, err := uuid.NewV7(); must(t, err); return value.String() }
	public := contentref.NewVersion(tenant, "article", id(), id())
	premium := contentref.NewVersion(tenant, "article", id(), id())
	people := []signal.Subject{{UserID: uuid.NewString()}, {UserID: uuid.NewString()}, {UserID: uuid.NewString()}, {UserID: uuid.NewString()}}
	observations := []signal.PopulationObservation{}
	for i, person := range people {
		tiers := [][]string{{"gold"}, {"silver"}, {}, {"gold"}}[i]
		observations = append(observations, signal.PopulationObservation{ID: uuid.NewString(), ContentKind: "article", Subject: person,
			ObservedAt: day.Add(18 * time.Hour), Domain: scope.Domain, Language: scope.Language, EligibleTiers: tiers, EligibilityKnown: i != 3})
	}
	// Several eligible observations/tier memberships remain one person/day.
	observations = append(observations, signal.PopulationObservation{ID: uuid.NewString(), ContentKind: "article", Subject: people[0],
		ObservedAt: day.Add(20 * time.Hour), Domain: scope.Domain, Language: scope.Language, EligibleTiers: []string{"gold", "silver"}, EligibilityKnown: true})
	must(t, store.RecordPopulation(t.Context(), tenant, observations))
	measurement := func(ref contentref.ContentRef, person signal.Subject, ms uint32) signal.Signal {
		t.Helper()
		m, err := signalstore.MeasurePages([]signal.PageEngagement{{Index: 0, Ranges: []signal.CoverageRange{{From: 0, To: 10000}}, ActiveMS: ms}}, 1, ms)
		must(t, err)
		m.VersionID, m.VisitID, m.SegmentID = ref.Version(), uuid.NewString(), uuid.NewString()
		at := day.Add(10 * time.Hour)
		return signal.Signal{ContentRef: ref, Subject: person, Type: signal.TypeEngagement, EventID: uuid.NewString(), Revision: 1,
			OccurredAt: at, Measurement: &m, Access: &signal.AccessContext{ObservedAt: at, ContentAccess: "restricted", ViewerAccess: "full",
				PolicyRevision: "daily", Audience: "unknown", Domain: scope.Domain, Language: scope.Language}}
	}
	first := measurement(public, people[0], 600)
	second := measurement(public, people[0], 600)
	translated := measurement(public.WithVersion(id()), people[0], 5000)
	replaced := measurement(public, people[3], 2000)
	revision := measurement(public, people[3], 300)
	revision.EventID, revision.Measurement.VisitID, revision.Measurement.SegmentID, revision.Revision = replaced.EventID, replaced.Measurement.VisitID, replaced.Measurement.SegmentID, 2
	// These otherwise-qualified receipts belong to other immutable scopes.
	// They cannot rescue the short current revision or qualify a new reader.
	otherDomain := measurement(public, people[3], 2000)
	otherDomain.Access.Domain = "other"
	otherLanguage := measurement(public, people[2], 2000)
	otherLanguage.Access.Language = "ja"
	must(t, store.RecordSignals(t.Context(), tenant, []signal.Signal{first, second, first, translated, replaced, revision, otherDomain, otherLanguage, measurement(public, people[1], 1000),
		measurement(premium, people[0], 1000), measurement(premium, people[1], 1000), measurement(premium, people[2], 1000), measurement(premium, people[3], 1000)}))
	availability := signal.Availability{Known: true, From: day, Until: end, Intervals: []signal.AccessInterval{
		{ID: "public", ContentID: public.ContentID, From: day.Add(12 * time.Hour), Until: end, Audience: "public"},
		{ID: "gold", ContentID: premium.ContentID, From: day, Until: day.Add(12 * time.Hour), Audience: "tier:gold", AudienceTiers: []string{"gold"}},
		{ID: "silver", ContentID: premium.ContentID, From: day.Add(12 * time.Hour), Until: day.AddDate(0, 0, 1), Audience: "tier:silver", AudienceTiers: []string{"silver"}},
	}}
	report, err := store.PrepareDailyPopularity(t.Context(), tenant, "article", availability, scope, false)
	must(t, err)
	if report.Prepared != 2 || report.Reused != 0 {
		t.Fatalf("initial preparation=%+v", report)
	}
	assertRows := func(wantPublic uint64) {
		t.Helper()
		rows, err := a.deps.ClickHouse.Query(t.Context(), `SELECT content_id,day,readers,eligible FROM `+a.cfg.ClickHouse.Database+`.popularity_daily FINAL WHERE tenant=? AND domain=? ORDER BY content_id,day`, tenant, scope.Domain)
		must(t, err)
		count := 0
		for rows.Next() {
			var content string
			var at time.Time
			var readers, eligible uint64
			must(t, rows.Scan(&content, &at, &readers, &eligible))
			count++
			if content == public.ContentID {
				wantU, wantN := uint64(0), uint64(0)
				if at.Equal(day) {
					wantU, wantN = wantPublic, 4
				}
				if readers != wantU || eligible != wantN {
					t.Fatalf("public %s U=%d N=%d want %d/%d", at, readers, eligible, wantU, wantN)
				}
			} else if content == premium.ContentID && (readers != 2 || eligible != 2) {
				t.Fatalf("daily union or eligibility duplicated/lost people: U=%d N=%d", readers, eligible)
			}
		}
		must(t, rows.Err())
		rows.Close()
		if count != 3 {
			t.Fatalf("available zero-reader days missing: %d rows", count)
		}
	}
	assertRows(2)
	covered, known, err := store.DailyPopularityCoverage(t.Context(), tenant, "article", scope)
	must(t, err)
	if !known || !covered.From.Equal(day) || !covered.To.Equal(end) {
		t.Fatalf("prepared coverage=%+v known=%v", covered, known)
	}
	// The first partially instrumented day is unavailable, while a partial
	// content launch inside known history still counts in the assertion above.
	partial := availability
	partial.From = day.Add(12 * time.Hour)
	partialScope := signal.ActivityScope{Domain: "partial", Language: "en"}
	report, err = store.PrepareDailyPopularity(t.Context(), tenant, "article", partial, partialScope, false)
	must(t, err)
	if report.Prepared != 1 {
		t.Fatalf("partial instrumentation included: %+v", report)
	}
	covered, known, err = store.DailyPopularityCoverage(t.Context(), tenant, "article", partialScope)
	must(t, err)
	if !known || !covered.From.Equal(day.AddDate(0, 0, 1)) || !covered.To.Equal(end) {
		t.Fatalf("partial instrumentation coverage=%+v known=%v", covered, known)
	}
	hits, err := store.DailyPopular(t.Context(), tenant, "article", signalstore.Between(day, end), scope, 10)
	must(t, err)
	if len(hits) != 2 || hits[0].ContentID != premium.ContentID || hits[0].Score != math.Round(2./22*1e12)/1e12 || hits[1].Score != math.Round((2./24)/2*1e12)/1e12 {
		t.Fatalf("daily score/available-day mean=%+v", hits)
	}
	// A new historical receipt does not silently rewrite finalized days.
	must(t, store.RecordSignals(t.Context(), tenant, []signal.Signal{measurement(public, people[2], 1000)}))
	report, err = store.PrepareDailyPopularity(t.Context(), tenant, "article", availability, scope, false)
	must(t, err)
	if report.Prepared != 0 || report.Reused != 2 {
		t.Fatalf("closed days recomputed: %+v", report)
	}
	assertRows(2)
	// A day prepared before its receipt window closed still needs one final
	// pass after a worker restart, even though it is now older than yesterday.
	must(t, a.deps.ClickHouse.Exec(t.Context(), `ALTER TABLE `+a.cfg.ClickHouse.Database+`.popularity_daily_prepared DELETE WHERE tenant=? AND day=? SETTINGS mutations_sync=2`, tenant, day))
	must(t, a.deps.ClickHouse.Exec(t.Context(), `INSERT INTO `+a.cfg.ClickHouse.Database+`.popularity_daily_prepared
 (tenant,content_kind,domain,language,day,computed_at) VALUES(?,?,?,?,?,?)`, tenant, "article", scope.Domain, scope.Language, day, day.Add(30*time.Hour)))
	report, err = store.PrepareDailyPopularity(t.Context(), tenant, "article", availability, scope, false)
	must(t, err)
	if report.Prepared != 1 || report.Reused != 1 {
		t.Fatalf("missed final day=%+v", report)
	}
	assertRows(3)
	report, err = store.PrepareDailyPopularity(t.Context(), tenant, "article", availability, scope, false)
	must(t, err)
	if report.Prepared != 0 || report.Reused != 2 {
		t.Fatalf("finalized day was not frozen: %+v", report)
	}
	report, err = store.PrepareDailyPopularity(t.Context(), tenant, "article", availability, scope, true)
	must(t, err)
	if report.Prepared != 2 {
		t.Fatalf("explicit rebuild=%+v", report)
	}
	assertRows(3)
	// Raw erasure remains authoritative. Anonymous historical counts are kept
	// until an explicit rebuild, which excludes fenced receipts/observations.
	_, err = store.EraseSubjects(t.Context(), []string{tenant}, []signal.Subject{people[0]})
	must(t, err)
	_, err = store.PrepareDailyPopularity(t.Context(), tenant, "article", availability, scope, true)
	must(t, err)
	rows, err := a.deps.ClickHouse.Query(t.Context(), `SELECT readers,eligible FROM `+a.cfg.ClickHouse.Database+`.popularity_daily FINAL WHERE tenant=? AND domain=? AND content_id=? AND day=?`, tenant, scope.Domain, public.ContentID, day)
	must(t, err)
	if !rows.Next() {
		t.Fatal("rebuilt day missing")
	}
	var readers, eligible uint64
	must(t, rows.Scan(&readers, &eligible))
	rows.Close()
	if readers != 2 || eligible != 3 {
		t.Fatalf("explicit rebuild ignored erasure: U=%d N=%d", readers, eligible)
	}
	empty := signal.Availability{Known: true, From: end, Until: end.AddDate(0, 0, 1)}
	report, err = store.PrepareDailyPopularity(t.Context(), tenant, "article", empty, scope, false)
	must(t, err)
	if report.Prepared != 1 {
		t.Fatalf("empty day was not prepared: %+v", report)
	}
	report, err = store.PrepareDailyPopularity(t.Context(), tenant, "article", empty, scope, false)
	must(t, err)
	if report.Reused != 1 || report.Prepared != 0 {
		t.Fatalf("empty day recomputed: %+v", report)
	}
	// Edition purge preserves its work-level aggregate; full work/kind purge
	// removes the anonymous rows without changing account-erasure semantics.
	must(t, store.ForgetContents(t.Context(), tenant, []signal.ForgottenContent{{Kind: "article", ContentID: public.ContentID, VersionID: public.Version()}}))
	countDaily := func() uint64 {
		t.Helper()
		rows, err := a.deps.ClickHouse.Query(t.Context(), `SELECT count() FROM `+a.cfg.ClickHouse.Database+`.popularity_daily FINAL WHERE tenant=? AND domain=?`, tenant, scope.Domain)
		must(t, err)
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("missing daily count")
		}
		var count uint64
		must(t, rows.Scan(&count))
		return count
	}
	if got := countDaily(); got != 3 {
		t.Fatalf("edition purge removed daily work: %d rows", got)
	}
	must(t, store.ForgetContents(t.Context(), tenant, []signal.ForgottenContent{{Kind: "article", ContentID: public.ContentID}}))
	if got := countDaily(); got != 1 {
		t.Fatalf("work purge kept daily rows: %d", got)
	}
	must(t, store.PurgeContentKinds(t.Context(), tenant, []string{"article"}))
	if got := countDaily(); got != 0 {
		t.Fatalf("kind purge kept daily rows: %d", got)
	}
	_, known, err = store.DailyPopularityCoverage(t.Context(), tenant, "article", scope)
	must(t, err)
	if known {
		t.Fatal("kind purge kept preparation coverage")
	}

}
