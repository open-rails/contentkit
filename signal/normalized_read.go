package signal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/open-rails/contentkit/contentref"
)

// MaxNormalizationIntervals bounds query-local availability data. Exceeding
// it is an explicit error, never a truncated or silently reclassified ranking.
const MaxNormalizationIntervals = 100_000

func (st *Store) normalizedPopular(ctx context.Context, tenant, kind string, candidates []string, opts PopularOptions) ([]PopularHit, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	n := *opts.Normalization
	if !n.Availability.Known {
		return nil, fmt.Errorf("signal: trusted activity availability is unknown")
	}
	if n.PriorWeight == 0 {
		n.PriorWeight = 20
	}
	if n.PriorWeight < 0 || math.IsInf(n.PriorWeight, 0) || math.IsNaN(n.PriorWeight) {
		return nil, fmt.Errorf("signal: invalid normalization prior")
	}
	if len(n.Availability.Intervals) > MaxNormalizationIntervals {
		return nil, &LimitError{Field: "normalization availability intervals", Limit: MaxNormalizationIntervals, Got: len(n.Availability.Intervals)}
	}
	if len(n.Availability.Intervals) == 0 {
		return []PopularHit{}, nil
	}
	if n.Quality.EngagementWeight == 0 {
		n.Quality = NormalizationPriors{Engagement: .4, EngagementWeight: 10, Completion: .3, CompletionWeight: 10, Approval: .75, ApprovalWeight: 5, RevisitWeight: 10}
	}
	table, err := ext.NewTable("contentkit_activity_intervals", ext.Column("interval_id", "String"), ext.Column("cohort_id", "String"), ext.Column("content_id", "String"),
		ext.Column("start_at", "DateTime64(6, 'UTC')"), ext.Column("end_at", "DateTime64(6, 'UTC')"), ext.Column("audience", "String"), ext.Column("tiers", "Array(String)"))
	if err != nil {
		return nil, err
	}
	for _, interval := range n.Availability.Intervals {
		if len(candidates) > 0 && !slices.Contains(candidates, interval.ContentID) {
			continue
		}
		from, until := interval.From, interval.Until
		if !opts.Window.From.IsZero() && from.Before(opts.Window.From) {
			from = opts.Window.From
		}
		if until.IsZero() || !opts.Window.To.IsZero() && until.After(opts.Window.To) {
			until = opts.Window.To
		}
		if until.IsZero() {
			until = n.Availability.Until
		}
		if from.IsZero() || !from.Before(until) {
			continue
		}
		if interval.Audience != "public" && len(interval.AudienceTiers) == 0 {
			return nil, fmt.Errorf("signal: audience %q has no known eligibility", interval.Audience)
		}
		tiers := interval.AudienceTiers
		if tiers == nil {
			tiers = []string{}
		}
		cohort, _ := json.Marshal(struct {
			From, Until string
			Audience    string
			Tiers       []string
		}{from.UTC().Format(time.RFC3339Nano), until.UTC().Format(time.RFC3339Nano), interval.Audience, tiers})
		if err := table.Append(interval.ID, fmt.Sprintf("%x", sha256.Sum256(cohort)), interval.ContentID, from.UTC(), until.UTC(), interval.Audience, tiers); err != nil {
			return nil, err
		}
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithExternalTable(table))
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > MaxNormalizationIntervals {
		limit = MaxNormalizationIntervals
	}
	// Contexts are immutable receipts. Measurements use the winning event
	// revision; its earliest trusted receipt fixes the audience across retries.
	query := fmt.Sprintf(`WITH
intervals AS (SELECT * FROM contentkit_activity_intervals),
cohorts AS (SELECT DISTINCT cohort_id,start_at,end_at,audience,tiers FROM intervals),
population AS (
 SELECT i.cohort_id, uniqExact(tuple(p.subject_kind,p.subject)) AS eligible
 FROM cohorts i CROSS JOIN %s.population_observations p
	 WHERE p.tenant=? AND p.content_kind=? AND %s AND p.observed_at>=i.start_at AND p.observed_at<i.end_at
  AND p.observed_at>=(SELECT min(start_at) FROM intervals) AND p.observed_at<(SELECT max(end_at) FROM intervals)
  AND (?='' OR p.domain=?) AND (?='' OR p.language=?)
  AND (i.audience='public' OR (p.eligibility_known AND hasAny(p.eligible_tiers,i.tiers)))
 GROUP BY i.cohort_id
),
canonical AS (
 SELECT content_id,subject_kind,subject,signal_type,event_id,
  argMax(tuple(occurred_at,duration_s,progress,progress_max,value,score,completed),version) AS c,
  argMin(JSONExtractRaw(payload,'%s'),tuple(JSONExtractString(payload,'%s','observed_at'),version)) AS a,
  parseDateTime64BestEffortOrNull(JSONExtractString(a,'observed_at'),6,'UTC') AS observed_at
 FROM %s.signals
 WHERE tenant=? AND content_kind=? AND content_version_id='' AND %s
  AND occurred_at>=toDateTime((SELECT min(start_at) FROM intervals))
  AND occurred_at<toDateTime((SELECT max(end_at) FROM intervals))+INTERVAL 1 SECOND
  AND content_id IN (SELECT content_id FROM intervals)
 GROUP BY content_id,subject_kind,subject,signal_type,event_id
),
readers AS (
 SELECT i.interval_id,i.content_id,c.subject_kind,c.subject,
  count() AS views,countIf(c.c.7) AS completed,sum(toFloat64(c.c.6)) AS engagement
 FROM intervals i INNER JOIN canonical c ON i.content_id=c.content_id
 WHERE c.signal_type='view' AND JSONExtractString(c.a,'viewer_access')='full'
  AND c.observed_at>=i.start_at AND c.observed_at<i.end_at
  AND (i.audience='public' OR (JSONExtractBool(c.a,'eligibility_known')
    AND hasAny(JSONExtract(c.a,'eligible_tiers','Array(String)'),i.tiers)))
  AND (?='' OR JSONExtractString(c.a,'domain')=?) AND (?='' OR JSONExtractString(c.a,'language')=?)
 GROUP BY i.interval_id,i.content_id,c.subject_kind,c.subject
),
feedback AS (
 SELECT i.interval_id,c.subject_kind,c.subject,sum(c.c.5) AS value
 FROM intervals i INNER JOIN canonical c ON i.content_id=c.content_id
 WHERE c.signal_type!='view' AND c.c.1>=i.start_at AND c.c.1<i.end_at AND c.c.5!=0
 GROUP BY i.interval_id,c.subject_kind,c.subject
),
measured AS (
 SELECT r.interval_id,count() AS viewers,countIf(r.completed>0) AS completers,
  sum(greatest(0.,least(1.,r.engagement/(100.*r.views)))) AS engagement,
  countIf(r.views>1) AS returning,countIf(f.value>0) AS positive,countIf(f.value<0) AS negative
 FROM readers r LEFT JOIN feedback f ON r.interval_id=f.interval_id AND r.subject_kind=f.subject_kind AND r.subject=f.subject
 GROUP BY r.interval_id
),
segments AS (
 SELECT i.content_id,toFloat64(dateDiff('microsecond',i.start_at,i.end_at)) AS duration,
  coalesce(p.eligible,0) AS eligible,coalesce(m.viewers,0) AS viewers,
  if(eligible>0,toFloat64(viewers)/eligible,0.) AS raw_rate,
  if(eligible>0,toFloat64(viewers)/(eligible+%g),0.) AS rate,
  (coalesce(m.engagement,0)+%g)/ (viewers+%g) AS engage,
  (coalesce(m.completers,0)+%g)/(viewers+%g) AS finish,
  (coalesce(m.positive,0)+%g)/(coalesce(m.positive,0)+coalesce(m.negative,0)+%g) AS approve,
  coalesce(m.returning,0)/(viewers+%g) AS revisit
 FROM intervals i LEFT JOIN population p ON p.cohort_id=i.cohort_id LEFT JOIN measured m ON m.interval_id=i.interval_id
),
normalized AS (
 SELECT content_id,sum(duration*raw_rate)/sum(duration) AS normalized_raw_rate,
  sum(duration*rate)/sum(duration) AS normalized_rate,
  sum(duration*rate*engage)/sum(duration) AS normalized_engagement,
  sum(duration*rate*finish)/sum(duration) AS normalized_completion,
  sum(duration*rate*approve)/sum(duration) AS normalized_approval,
  sum(duration*rate*revisit)/sum(duration) AS normalized_revisit,
  max(viewers>eligible) AS invalid_population
 FROM segments GROUP BY content_id
)
SELECT content_id,normalized_raw_rate,normalized_rate,normalized_engagement,normalized_completion,normalized_approval,normalized_revisit,
 invalid_population,(%s) AS rank_score
FROM normalized WHERE normalized_rate>0 OR invalid_population ORDER BY rank_score DESC,content_id ASC LIMIT ?%s`,
		st.db, st.notErased(), accessPayloadKey, accessPayloadKey, st.db, st.notErased(),
		n.PriorWeight, n.Quality.Engagement*n.Quality.EngagementWeight, n.Quality.EngagementWeight,
		n.Quality.Completion*n.Quality.CompletionWeight, n.Quality.CompletionWeight,
		n.Quality.Approval*n.Quality.ApprovalWeight, n.Quality.ApprovalWeight, n.Quality.RevisitWeight,
		opts.RankExpr, finalSettings+", max_execution_time=15, max_memory_usage=268435456")
	if strings.TrimSpace(opts.RankExpr) == "" {
		return nil, fmt.Errorf("signal: normalized ranking requires a policy expression")
	}
	rows, err := st.conn.Query(ctx, query, tenant, kind, n.Scope.Domain, n.Scope.Domain, n.Scope.Language, n.Scope.Language,
		tenant, kind, n.Scope.Domain, n.Scope.Domain, n.Scope.Language, n.Scope.Language, limit)
	if err != nil {
		return nil, fmt.Errorf("signal: normalized popularity: %w", err)
	}
	var hits []PopularHit
	for rows.Next() {
		var id string
		var m NormalizationMetrics
		var score float64
		var invalid bool
		if err := rows.Scan(&id, &m.RawRate, &m.Rate, &m.Engagement, &m.Completion, &m.Approval, &m.Revisit, &invalid, &score); err != nil {
			rows.Close()
			return nil, err
		}
		if invalid {
			rows.Close()
			return nil, fmt.Errorf("signal: trusted readers exceed observed eligible population")
		}
		hits = append(hits, PopularHit{ContentRef: contentref.New(tenant, kind, id), Score: score, ContentMetrics: ContentMetrics{Normalization: &m}})
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(hits) == 0 {
		return hits, err
	}
	refs := make([]ContentRef, len(hits))
	for i := range hits {
		refs[i] = hits[i].ContentRef
	}
	raw, err := st.Metrics(ctx, tenant, refs, opts.Window)
	if err != nil {
		return nil, err
	}
	for i := range hits {
		m := hits[i].Normalization
		hits[i].ContentMetrics = raw[hits[i].Key()]
		hits[i].Normalization = m
	}
	return hits, nil
}
