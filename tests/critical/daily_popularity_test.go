package critical_test

import (
	"math"
	"strings"
	"testing"
	"time"

	signalstore "github.com/open-rails/contentkit/internal/signalstore"

	"github.com/ClickHouse/clickhouse-go/v2"
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
		rows, err := a.deps.ClickHouse.Query(t.Context(), `SELECT content_id,day,readers,eligible FROM `+a.cfg.ClickHouse.Database+`.popularity_daily WHERE tenant=? AND domain=? ORDER BY content_id,day`, tenant, scope.Domain)
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
	rows, err := a.deps.ClickHouse.Query(t.Context(), `SELECT readers,eligible FROM `+a.cfg.ClickHouse.Database+`.popularity_daily WHERE tenant=? AND domain=? AND content_id=? AND day=?`, tenant, scope.Domain, public.ContentID, day)
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
		rows, err := a.deps.ClickHouse.Query(t.Context(), `SELECT count() FROM `+a.cfg.ClickHouse.Database+`.popularity_daily WHERE tenant=? AND domain=?`, tenant, scope.Domain)
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

func TestDailyPopularityDaysAreWriteOnce(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	db := a.cfg.ClickHouse.Database
	store, err := signalstore.NewStore(a.deps.ClickHouse, db)
	must(t, err)
	ctx := t.Context()
	tenant := a.cfg.Scope.AppID.String()
	scope := signal.ActivityScope{Domain: "main", Language: "en"}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	closed, recent := today.AddDate(0, 0, -5), today.AddDate(0, 0, -1)
	id := func() string { value, err := uuid.NewV7(); must(t, err); return value.String() }
	works := []contentref.ContentRef{contentref.NewVersion(tenant, "article", id(), id()), contentref.NewVersion(tenant, "article", id(), id()), contentref.NewVersion(tenant, "article", id(), id())}
	people := []signal.Subject{{UserID: uuid.NewString()}, {UserID: uuid.NewString()}, {UserID: uuid.NewString()}, {UserID: uuid.NewString()}}
	var observations []signal.PopulationObservation
	var reads []signal.Signal
	for _, day := range []time.Time{closed, recent} {
		for _, person := range people {
			observations = append(observations, signal.PopulationObservation{ID: uuid.NewString(), ContentKind: "article", Subject: person,
				ObservedAt: day.Add(9 * time.Hour), Domain: scope.Domain, Language: scope.Language, EligibleTiers: []string{}, EligibilityKnown: true})
		}
		// Three people read the first work and one reads the second.
		for i, read := range []struct{ work, person int }{{0, 0}, {0, 1}, {0, 2}, {1, 0}} {
			m, err := signalstore.MeasurePages([]signal.PageEngagement{{Index: 0, Ranges: []signal.CoverageRange{{From: 0, To: 10000}}, ActiveMS: 2000}}, 1, 2000)
			must(t, err)
			m.VersionID, m.VisitID, m.SegmentID = works[read.work].Version(), uuid.NewString(), uuid.NewString()
			at := day.Add(10*time.Hour + time.Duration(i)*time.Minute)
			reads = append(reads, signal.Signal{ContentRef: works[read.work], Subject: people[read.person], Type: signal.TypeEngagement, EventID: uuid.NewString(), Revision: 1,
				OccurredAt: at, Measurement: &m, Access: &signal.AccessContext{ObservedAt: at, ContentAccess: "public", ViewerAccess: "full",
					PolicyRevision: "daily", Audience: "public", Domain: scope.Domain, Language: scope.Language}})
		}
	}
	must(t, store.RecordPopulation(ctx, tenant, observations))
	must(t, store.RecordSignals(ctx, tenant, reads))
	availability := signal.Availability{Known: true, From: closed, Until: today}
	for _, work := range works {
		availability.Intervals = append(availability.Intervals, signal.AccessInterval{ID: work.ContentID, ContentID: work.ContentID, From: closed, Until: today.AddDate(0, 0, 1), Audience: "public"})
	}
	scalar := func(dest any, query string, args ...any) {
		t.Helper()
		rows, err := a.deps.ClickHouse.Query(ctx, query, args...)
		must(t, err)
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("no row: %s", query)
		}
		must(t, rows.Scan(dest))
	}
	prepare := func(rebuild bool) {
		t.Helper()
		_, err := store.PrepareDailyPopularity(ctx, tenant, "article", availability, scope, rebuild)
		must(t, err)
	}
	// Plain sums without FINAL must equal one preparation of each day.
	assertExact := func(label string) {
		t.Helper()
		rows, err := a.deps.ClickHouse.Query(ctx, `SELECT day,count(),uniqExact(content_id),sum(readers),max(eligible) FROM `+db+`.popularity_daily
 WHERE tenant=? AND content_kind='article' AND domain=? AND language=? GROUP BY day ORDER BY day`, tenant, scope.Domain, scope.Language)
		must(t, err)
		days := 0
		for rows.Next() {
			var day time.Time
			var count, unique, readers, eligible uint64
			must(t, rows.Scan(&day, &count, &unique, &readers, &eligible))
			days++
			wantReaders, wantEligible := uint64(0), uint64(0)
			if day.Equal(closed) || day.Equal(recent) {
				wantReaders, wantEligible = 4, 4
			}
			if count != 3 || unique != 3 || readers != wantReaders || eligible != wantEligible {
				t.Fatalf("%s: %s rows=%d works=%d U=%d N=%d; want 3/3/%d/%d", label, day.Format(time.DateOnly), count, unique, readers, eligible, wantReaders, wantEligible)
			}
		}
		must(t, rows.Err())
		rows.Close()
		if days != 5 {
			t.Fatalf("%s: %d prepared days; want 5", label, days)
		}
		var staged uint64
		scalar(&staged, `SELECT count() FROM `+db+`.popularity_daily_staging`)
		if staged != 0 {
			t.Fatalf("%s: %d staged rows left behind", label, staged)
		}
	}
	for range 8 { // the provisional day is rebuilt and swapped every time
		prepare(false)
	}
	assertExact("repeated refreshes")
	for range 3 {
		prepare(true)
	}
	assertExact("explicit rebuilds")
	// An interrupted build's staged rows are discarded, never swapped in.
	must(t, a.deps.ClickHouse.Exec(ctx, `INSERT INTO `+db+`.popularity_daily_staging (tenant,content_kind,domain,language,day,content_id,readers,eligible)
 VALUES (?,'article',?,?,?,?,99,4)`, tenant, scope.Domain, scope.Language, recent, works[0].ContentID))
	prepare(false)
	assertExact("after an interrupted build")
	hits, err := store.DailyPopular(ctx, tenant, "article", signalstore.Between(closed, today), scope, 10)
	must(t, err)
	if len(hits) != 2 || hits[0].ContentID != works[0].ContentID || hits[0].Score != math.Round(3./24*2/5*1e12)/1e12 || hits[1].Score != math.Round(1./24*2/5*1e12)/1e12 {
		t.Fatalf("daily scores=%+v", hits)
	}

	// Sixty one-part days of a 20,000-work scope rank under the read cap.
	bulk := signal.ActivityScope{Domain: "bulk", Language: "en"}
	first := today.AddDate(0, 0, -60)
	for start := 0; start < 60; start += 30 {
		must(t, a.deps.ClickHouse.Exec(ctx, `INSERT INTO `+db+`.popularity_daily (tenant,content_kind,domain,language,day,content_id,readers,eligible)
 SETTINGS max_partitions_per_insert_block=0
 SELECT ?,'article',?,?,addDays(toDate(?),toInt32(intDiv(number,20000))),reinterpretAsUUID(MD5(toString(number%20000))),number%7,100
 FROM numbers(?,?)`, tenant, bulk.Domain, bulk.Language, first.Format(time.DateOnly), uint64(start*20000), uint64(30*20000)))
	}
	query := "ck269_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	hits, err = store.DailyPopular(clickhouse.Context(ctx, clickhouse.WithQueryID(query)), tenant, "article", signalstore.Between(first, today), bulk, 100000)
	must(t, err)
	if len(hits) == 0 {
		t.Fatal("bulk scope ranked nothing")
	}
	must(t, a.deps.ClickHouse.Exec(ctx, "SYSTEM FLUSH LOGS"))
	var read, peak uint64
	scalar(&read, `SELECT read_rows FROM system.query_log WHERE query_id=? AND type='QueryFinish'`, query)
	scalar(&peak, `SELECT toUInt64(memory_usage) FROM system.query_log WHERE query_id=? AND type='QueryFinish'`, query)
	t.Logf("60 days x 20,000 works: %d hits, %d rows read, peak %.1f MiB", len(hits), read, float64(peak)/(1<<20))
	if len(hits) != 20000 || read < 60*20000 {
		t.Fatalf("ranked %d works from %d rows; want 20,000 from all 60 days", len(hits), read)
	}
}
