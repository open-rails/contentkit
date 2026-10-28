package signalstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/open-rails/contentkit/contentref"
	signal "github.com/open-rails/contentkit/signal"
)

// PrepareDailyPopularity reduces canonical receipts to anonymous work/day
// counts. Eligibility is deliberately whole-day: public wins if any edition
// was public that day; otherwise known tier audiences are unioned. A person
// eligible at any observation that UTC day belongs to that day's audience.
//
// The ordinary ranking worker calls this once per kind/scope and union of its
// due bucket windows. Today and yesterday cover the 24-hour activity receipt
// lifetime. Older imports/corrections require rebuild=true. Failures return to
// that caller; partial derived state is retried, never a reason to lose raw data.
// Callers must not prepare one scope concurrently: the refresh job is unique
// fleet-wide, and two builds of one day would share its staging partition.
func (st *Store) PrepareDailyPopularity(ctx context.Context, tenant, kind string, availability signal.Availability, scope signal.ActivityScope, rebuild bool) (signal.DailyPopularityPreparation, error) {
	var report signal.DailyPopularityPreparation
	if err := dailyPopularityScope(tenant, kind, scope); err != nil {
		return report, err
	}
	if !availability.Known || availability.From.IsZero() || !availability.From.Before(availability.Until) {
		return report, nil
	}
	from, until := utcDay(availability.From), utcDay(availability.Until)
	// Coverage begins at the first fully instrumented UTC day. This does not
	// discard partial content launch days inside the trusted history.
	if availability.From.After(from) {
		from = from.AddDate(0, 0, 1)
	}
	if availability.Until.After(until) {
		until = until.AddDate(0, 0, 1)
	}
	prepared, err := st.preparedPopularityDays(ctx, tenant, kind, scope, from, until)
	if err != nil {
		return report, err
	}
	// Historical requested windows can end before now. Receipt validity is
	// tied to wall time, not the final day of a fixed calendar bucket.
	recent := utcDay(time.Now()).AddDate(0, 0, -1)
	for day := from; day.Before(until); day = day.AddDate(0, 0, 1) {
		computed, exists := prepared[day.Format(time.DateOnly)]
		// A provisional day gets one final build after its end plus the
		// 24-hour receipt lifetime, even if the worker missed that rollover.
		if !rebuild && day.Before(recent) && exists && !computed.Before(day.AddDate(0, 0, 2)) {
			report.Reused++
			continue
		}
		if err := st.preparePopularityDay(ctx, tenant, kind, scope, day, availability.Intervals); err != nil {
			return report, err
		}
		report.Prepared++
	}
	return report, nil
}

func dailyPopularityScope(tenant, kind string, scope signal.ActivityScope) error {
	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(kind) == "" {
		return fmt.Errorf("signal: daily popularity requires tenant and kind")
	}
	return checkIdentifiers("Tenant", tenant, "ContentKind", kind, "Domain", scope.Domain, "Language", scope.Language)
}

func (st *Store) preparedPopularityDays(ctx context.Context, tenant, kind string, scope signal.ActivityScope, from, until time.Time) (map[string]time.Time, error) {
	rows, err := st.conn.Query(ctx, `SELECT day,max(computed_at) FROM `+st.db+`.popularity_daily_prepared
 WHERE tenant=? AND content_kind=? AND domain=? AND language=? AND day>=? AND day<? GROUP BY day`, tenant, kind, scope.Domain, scope.Language, from, until)
	if err != nil {
		return nil, fmt.Errorf("signal: prepared popularity days: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var day, computed time.Time
		if err := rows.Scan(&day, &computed); err != nil {
			return nil, err
		}
		out[day.UTC().Format(time.DateOnly)] = computed
	}
	return out, rows.Err()
}

type dailyAudience struct {
	public bool
	tiers  []string
}

func popularityDayAudiences(day time.Time, intervals []signal.AccessInterval) map[string]dailyAudience {
	until := day.AddDate(0, 0, 1)
	out := map[string]dailyAudience{}
	for _, interval := range intervals {
		if !interval.From.Before(until) || !interval.Until.IsZero() && !interval.Until.After(day) {
			continue
		}
		if interval.Audience != "public" && len(interval.AudienceTiers) == 0 {
			continue // unknown ACLs have no invented eligible population
		}
		audience := out[interval.ContentID]
		audience.public = audience.public || interval.Audience == "public"
		audience.tiers = append(audience.tiers, interval.AudienceTiers...)
		out[interval.ContentID] = audience
	}
	for id, audience := range out {
		if audience.public {
			audience.tiers = []string{}
		} else {
			audience.tiers = slices.Compact(slices.Sorted(slices.Values(audience.tiers)))
		}
		out[id] = audience
	}
	return out
}

// preparePopularityDay builds a whole scope/day in staging and swaps it in, so
// each day holds exactly its latest preparation and reads need no FINAL.
func (st *Store) preparePopularityDay(ctx context.Context, tenant, kind string, scope signal.ActivityScope, day time.Time, intervals []signal.AccessInterval) error {
	audiences := popularityDayAudiences(day, intervals)
	partition := " PARTITION (?,?,?,?,toDate(?))"
	key := []any{tenant, kind, scope.Domain, scope.Language, day.Format(time.DateOnly)}
	alter := func(statement string) error {
		if err := st.conn.Exec(ctx, "ALTER TABLE "+st.db+"."+statement, key...); err != nil {
			return fmt.Errorf("signal: swap popularity day %s: %w", day.Format(time.DateOnly), err)
		}
		return nil
	}
	if err := alter("popularity_daily_staging DROP" + partition); err != nil {
		return err
	}
	if len(audiences) == 0 {
		if err := alter("popularity_daily DROP" + partition); err != nil {
			return err
		}
	} else {
		if err := st.insertPopularityDay(ctx, tenant, kind, scope, day, audiences); err != nil {
			return err
		}
		if err := alter("popularity_daily REPLACE" + partition + " FROM " + st.db + ".popularity_daily_staging"); err != nil {
			return err
		}
		if err := alter("popularity_daily_staging DROP" + partition); err != nil {
			return err
		}
	}
	if err := st.conn.Exec(ctx, `INSERT INTO `+st.db+`.popularity_daily_prepared
 (tenant,content_kind,domain,language,day,computed_at) VALUES (?,?,?,?,?,now64(6))`, tenant, kind, scope.Domain, scope.Language, day); err != nil {
		return fmt.Errorf("signal: checkpoint popularity day: %w", err)
	}
	return nil
}

func (st *Store) insertPopularityDay(ctx context.Context, tenant, kind string, scope signal.ActivityScope, day time.Time, audiences map[string]dailyAudience) error {
	table, err := ext.NewTable("contentkit_daily_audiences", ext.Column("content_id", "String"),
		ext.Column("audience", "String"), ext.Column("public", "UInt8"), ext.Column("tiers", "Array(String)"))
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(audiences))
	for id := range audiences {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		audience := audiences[id]
		name, public := "tier:"+strings.Join(audience.tiers, ","), uint8(0)
		if audience.public {
			name, public = "public", 1
		}
		if err := table.Append(id, name, public, audience.tiers); err != nil {
			return err
		}
	}
	// Every replica holds the staged day before any of them swaps it in.
	quorum, err := st.erasureQuorum(ctx)
	if err != nil {
		return err
	}
	replicated := ""
	if quorum > 1 {
		replicated = fmt.Sprintf(", insert_quorum=%d, insert_quorum_parallel=1", quorum)
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithExternalTable(table))
	until := day.AddDate(0, 0, 1)
	// Canonicalize only scalar facts needed for participation. Geometry is not
	// unioned or rescored. Signed receipt access facts are immutable across
	// revisions, so filter full access and scope before grouping receipts.
	// Edition qualification precedes work/person dedupe.
	query := fmt.Sprintf(`INSERT INTO %[1]s.popularity_daily_staging
 (tenant,content_kind,domain,language,day,content_id,readers,eligible)
 WITH canonical AS (
 SELECT content_id,content_version_id,subject_kind,subject,receipt_id,
 argMax(tuple(active_ms,notEmpty(covered_units)),version) AS fact
 FROM %[1]s.engagement_segments
 WHERE tenant=? AND content_kind=? AND occurred_at>=? AND occurred_at<? AND %[2]s
 AND JSONExtractString(access_context,'viewer_access')='full'
 AND (?='' OR JSONExtractString(access_context,'domain')=?) AND (?='' OR JSONExtractString(access_context,'language')=?)
 GROUP BY content_id,content_version_id,subject_kind,subject,receipt_id
 ), people AS (
 SELECT subject_kind,subject,groupUniqArrayArrayIf(eligible_tiers,eligibility_known) AS tiers
 FROM %[1]s.measured_population_observations
 WHERE tenant=? AND content_kind=? AND observed_at>=? AND observed_at<? AND %[2]s
 AND (?='' OR domain=?) AND (?='' OR language=?) GROUP BY subject_kind,subject
 ), qualified AS (
 SELECT r.content_id AS content_id,r.content_version_id AS content_version_id,r.subject_kind AS subject_kind,r.subject AS subject
 FROM canonical r INNER JOIN contentkit_daily_audiences a ON r.content_id=a.content_id
 INNER JOIN people p ON p.subject_kind=r.subject_kind AND p.subject=r.subject
 WHERE a.public=1 OR hasAny(p.tiers,a.tiers)
 GROUP BY r.content_id,r.content_version_id,r.subject_kind,r.subject
 HAVING sum(toUInt64(r.fact.1))>=1000 AND max(r.fact.2)>0
 ), readers AS (
 SELECT content_id,uniqExact(tuple(subject_kind,subject)) AS readers FROM qualified GROUP BY content_id
 ), cohorts AS (SELECT DISTINCT audience,public,tiers FROM contentkit_daily_audiences), population AS (
 SELECT a.audience,count() AS eligible FROM cohorts a CROSS JOIN people p
 WHERE a.public=1 OR hasAny(p.tiers,a.tiers) GROUP BY a.audience
 )
 SELECT ?,?,?,?,toDate(?),toUUID(a.content_id),coalesce(r.readers,0),coalesce(p.eligible,0)
 FROM contentkit_daily_audiences a LEFT JOIN readers r USING(content_id) LEFT JOIN population p USING(audience)
 SETTINGS max_threads=2, max_execution_time=120, max_memory_usage=1073741824,
 max_bytes_before_external_group_by=134217728, max_bytes_before_external_sort=134217728%[3]s`, st.db, st.notErased(), replicated)
	if err := st.conn.Exec(ctx, query, tenant, kind, day, until, scope.Domain, scope.Domain, scope.Language, scope.Language,
		tenant, kind, day, until, scope.Domain, scope.Domain, scope.Language, scope.Language,
		tenant, kind, scope.Domain, scope.Language, day); err != nil {
		return fmt.Errorf("signal: prepare popularity day %s: %w", day.Format(time.DateOnly), err)
	}
	return nil
}

// DailyPopularityCoverage reads the actual prepared date range, including
// checkpointed empty days. Until is exclusive; today's row is provisional.
// Preparation walks days in order, so a failed first build exposes only its
// completed prefix instead of claiming the requested full history.
func (st *Store) DailyPopularityCoverage(ctx context.Context, tenant, kind string, scope signal.ActivityScope) (signal.Window, bool, error) {
	if err := dailyPopularityScope(tenant, kind, scope); err != nil {
		return signal.Window{}, false, err
	}
	rows, err := st.conn.Query(ctx, `SELECT count(),min(day),max(day) FROM `+st.db+`.popularity_daily_prepared
 WHERE tenant=? AND content_kind=? AND domain=? AND language=?`, tenant, kind, scope.Domain, scope.Language)
	if err != nil {
		return signal.Window{}, false, fmt.Errorf("signal: daily popularity coverage: %w", err)
	}
	defer rows.Close()
	var count uint64
	var from, last time.Time
	if rows.Next() {
		if err := rows.Scan(&count, &from, &last); err != nil {
			return signal.Window{}, false, err
		}
	}
	if err := rows.Err(); err != nil {
		return signal.Window{}, false, err
	}
	if count == 0 {
		return signal.Window{}, false, nil
	}
	return Between(utcDay(from), utcDay(last).AddDate(0, 0, 1)), true, nil
}

// dailyRankSettings bound one ranking read. Days hold no duplicates, so memory
// is the per-work aggregate: about 32 MiB for a year of 100,000 works. Spilling
// does not help: the hash table outgrows the cap in one resize.
const dailyRankSettings = " SETTINGS max_threads=2, max_memory_usage=67108864"

// DailyPopular reads only prepared anonymous day rows. Available days with no
// readers contribute zero. Each available UTC day has equal weight, including
// a partial launch/access-transition day; no period-distinct or quality terms
// are reconstructed here. Scores use twelve decimal places before the ID tie
// break so parallel floating-point reductions cannot reorder equal scores.
// Raw data and the explicit rebuild remain available.
func (st *Store) DailyPopular(ctx context.Context, tenant, kind string, window signal.Window, scope signal.ActivityScope, limit int) ([]signal.PopularHit, error) {
	if err := dailyPopularityScope(tenant, kind, scope); err != nil {
		return nil, err
	}
	if err := window.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	predicate, args := windowDayPredicate(window, "day")
	params := append([]any{tenant, kind, scope.Domain, scope.Language}, args...)
	params = append(params, limit)
	query := `SELECT toString(content_id) AS id,round(avg(toFloat64(readers)/(eligible+20.)),12) AS score
 FROM ` + st.db + `.popularity_daily WHERE tenant=? AND content_kind=? AND domain=? AND language=?` + predicate + `
 GROUP BY content_id HAVING sum(readers)>0 ORDER BY score DESC,id LIMIT ?` + dailyRankSettings
	rows, err := st.conn.Query(ctx, query, params...)
	if err != nil {
		return nil, fmt.Errorf("signal: daily popularity: %w", err)
	}
	defer rows.Close()
	out := []signal.PopularHit{}
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, err
		}
		out = append(out, signal.PopularHit{ContentRef: contentref.New(tenant, kind, id), Score: score})
	}
	return out, rows.Err()
}
