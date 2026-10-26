package signal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

// rankingBatchReceipts bounds the window receipts one ranking query measures.
// A batch that still exceeds the query memory cap retries split by reader.
const rankingBatchReceipts = 20_000

// maxRankingShards bounds how finely one work's readers are split.
const maxRankingShards = 1 << 12

const rankingSettings = finalSettings + ", max_threads=2, max_execution_time=15, max_memory_usage=268435456"

type activityInterval struct {
	id, cohort, content string
	from, until         time.Time
	audience            string
	tiers               []string
}

// rankingBatch is a set of works, restricted to the readers whose subject
// hash falls in shard of shards.
type rankingBatch struct {
	intervals     []activityInterval
	shard, shards uint64
}

// intervalMetrics are sums over distinct qualified readers, so batches over
// disjoint works or disjoint readers add up to the whole.
type intervalMetrics struct {
	readers, completers, returning, positive, negative uint64
	engagement                                         float64
}

// normalizedPopular ranks exactly as one query over every interval would, in
// batches bounded by window receipts: the eligible population once per
// cohort, each batch's per-interval reader sums, then one scoring pass.
func (st *Store) normalizedPopular(ctx context.Context, tenant, kind string, opts PopularOptions) ([]PopularHit, error) {
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
	if strings.TrimSpace(opts.RankExpr) == "" {
		return nil, fmt.Errorf("signal: normalized ranking requires a policy expression")
	}
	if n.Quality.EngagementWeight == 0 {
		n.Quality = NormalizationPriors{Engagement: .4, EngagementWeight: 10, Completion: .3, CompletionWeight: 10, Approval: .75, ApprovalWeight: 5, RevisitWeight: 10}
	}
	var intervals []activityInterval
	for _, interval := range n.Availability.Intervals {
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
		intervals = append(intervals, activityInterval{id: interval.ID, cohort: fmt.Sprintf("%x", sha256.Sum256(cohort)), content: interval.ContentID,
			from: from.UTC(), until: until.UTC(), audience: interval.Audience, tiers: tiers})
	}
	if len(intervals) == 0 {
		return []PopularHit{}, nil
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, MaxNormalizationIntervals)
	eligible, err := st.cohortPopulation(ctx, tenant, kind, n.Scope, intervals)
	if err != nil {
		return nil, err
	}
	batches, err := st.rankingBatches(ctx, tenant, kind, intervals)
	if err != nil {
		return nil, err
	}
	metrics := map[string]*intervalMetrics{}
	for len(batches) > 0 {
		b := batches[len(batches)-1]
		batches = batches[:len(batches)-1]
		err := st.addIntervalMetrics(ctx, tenant, kind, n.Scope, b, metrics)
		if !overBudget(err) {
			if err != nil {
				return nil, err
			}
			continue
		}
		if b.shards*2 > maxRankingShards {
			return nil, fmt.Errorf("signal: normalized popularity: a batch exceeds the ranking budget at %d reader shards: %w", b.shards, err)
		}
		batches = append(batches, rankingBatch{intervals: b.intervals, shard: b.shard, shards: b.shards * 2},
			rankingBatch{intervals: b.intervals, shard: b.shard + b.shards, shards: b.shards * 2})
	}
	return st.scoreNormalized(ctx, tenant, kind, intervals, eligible, metrics, n, opts.RankExpr, limit)
}

// overBudget reports a query that exceeded its memory or time cap; a smaller
// batch can succeed where it failed.
func overBudget(err error) bool {
	var ex *clickhouse.Exception
	return errors.As(err, &ex) && (ex.Code == 241 || ex.Code == 159)
}

func intervalTable(intervals []activityInterval) (*ext.Table, error) {
	table, err := ext.NewTable("contentkit_activity_intervals", ext.Column("interval_id", "String"), ext.Column("cohort_id", "String"), ext.Column("content_id", "String"),
		ext.Column("start_at", "DateTime64(6, 'UTC')"), ext.Column("end_at", "DateTime64(6, 'UTC')"), ext.Column("audience", "String"), ext.Column("tiers", "Array(String)"))
	if err != nil {
		return nil, err
	}
	for _, i := range intervals {
		if err := table.Append(i.id, i.cohort, i.content, i.from, i.until, i.audience, i.tiers); err != nil {
			return nil, err
		}
	}
	return table, nil
}

func (st *Store) cohortPopulation(ctx context.Context, tenant, kind string, scope ActivityScope, intervals []activityInterval) (map[string]uint64, error) {
	seen := map[string]bool{}
	var cohorts []activityInterval
	for _, i := range intervals {
		if !seen[i.cohort] {
			seen[i.cohort] = true
			cohorts = append(cohorts, i)
		}
	}
	table, err := intervalTable(cohorts)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`WITH cohorts AS (SELECT cohort_id,start_at,end_at,audience,tiers FROM contentkit_activity_intervals)
 SELECT i.cohort_id,uniqExact(tuple(p.subject_kind,p.subject))
 FROM cohorts i CROSS JOIN %s.measured_population_observations p
 WHERE p.tenant=? AND p.content_kind=? AND %s AND p.observed_at>=i.start_at AND p.observed_at<i.end_at
 AND p.observed_at>=(SELECT min(start_at) FROM cohorts) AND p.observed_at<(SELECT max(end_at) FROM cohorts)
 AND (?='' OR p.domain=?) AND (?='' OR p.language=?)
 AND (i.audience='public' OR (p.eligibility_known AND hasAny(p.eligible_tiers,i.tiers))) GROUP BY i.cohort_id%s`, st.db, st.notErased(), rankingSettings)
	rows, err := st.conn.Query(clickhouse.Context(ctx, clickhouse.WithExternalTable(table)), query, tenant, kind, scope.Domain, scope.Domain, scope.Language, scope.Language)
	if err != nil {
		return nil, fmt.Errorf("signal: normalized popularity population: %w", err)
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var cohort string
		var eligible uint64
		if err := rows.Scan(&cohort, &eligible); err != nil {
			return nil, err
		}
		out[cohort] = eligible
	}
	return out, rows.Err()
}

// rankingBatches groups works by their window receipts. A work above the
// budget gets reader shards; works without receipts have no readers to sum.
func (st *Store) rankingBatches(ctx context.Context, tenant, kind string, intervals []activityInterval) ([]rankingBatch, error) {
	table, err := intervalTable(intervals)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`SELECT content_id,count() FROM %s.engagement_segments
 WHERE tenant=? AND content_kind=? AND content_id IN (SELECT content_id FROM contentkit_activity_intervals)
 AND occurred_at>=(SELECT min(start_at) FROM contentkit_activity_intervals) AND occurred_at<(SELECT max(end_at) FROM contentkit_activity_intervals)
 GROUP BY content_id%s`, st.db, rankingSettings)
	rows, err := st.conn.Query(clickhouse.Context(ctx, clickhouse.WithExternalTable(table)), query, tenant, kind)
	if err != nil {
		return nil, fmt.Errorf("signal: normalized popularity receipts: %w", err)
	}
	receipts := map[string]uint64{}
	for rows.Next() {
		var id string
		var count uint64
		if err := rows.Scan(&id, &count); err != nil {
			rows.Close()
			return nil, err
		}
		receipts[id] = count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byContent := map[string][]activityInterval{}
	var contents []string
	for _, i := range intervals {
		if receipts[i.content] == 0 {
			continue
		}
		if byContent[i.content] == nil {
			contents = append(contents, i.content)
		}
		byContent[i.content] = append(byContent[i.content], i)
	}
	slices.Sort(contents)
	var batches []rankingBatch
	var open []activityInterval
	var size uint64
	for _, id := range contents {
		count := receipts[id]
		if count > rankingBatchReceipts {
			shards := uint64(1)
			for shards*rankingBatchReceipts < count && shards < maxRankingShards {
				shards *= 2
			}
			for shard := range shards {
				batches = append(batches, rankingBatch{intervals: byContent[id], shard: shard, shards: shards})
			}
			continue
		}
		if size+count > rankingBatchReceipts && len(open) > 0 {
			batches = append(batches, rankingBatch{intervals: open, shards: 1})
			open, size = nil, 0
		}
		open = append(open, byContent[id]...)
		size += count
	}
	if len(open) > 0 {
		batches = append(batches, rankingBatch{intervals: open, shards: 1})
	}
	return batches, nil
}

// addIntervalMetrics measures one batch. Receipts outside the batch's span
// cannot enter an interval: a receipt's occurred_at is its immutable trusted
// access time in every revision.
func (st *Store) addIntervalMetrics(ctx context.Context, tenant, kind string, scope ActivityScope, b rankingBatch, into map[string]*intervalMetrics) error {
	table, err := intervalTable(b.intervals)
	if err != nil {
		return err
	}
	// Contexts are immutable receipts. Measurements use the winning event
	// revision; its earliest trusted receipt fixes the audience across retries.
	intervalCanonical := `SELECT ` + intervalMeasurementProjection() + `
 FROM intervals i INNER JOIN receipts receipt ON i.content_id=receipt.content_id
 WHERE JSONExtractString(receipt.a,'viewer_access')='full' AND receipt.c.1>=i.start_at AND receipt.c.1<i.end_at
 AND (i.audience='public' OR (JSONExtractBool(receipt.a,'eligibility_known') AND hasAny(JSONExtract(receipt.a,'eligible_tiers','Array(String)'),i.tiers)))
 AND (?='' OR JSONExtractString(receipt.a,'domain')=?) AND (?='' OR JSONExtractString(receipt.a,'language')=?)`
	measured := strings.TrimPrefix(measuredVersions("SELECT * FROM interval_measurements"), "WITH ")
	shard := "cityHash64(subject_kind,subject)%?=?"
	query := fmt.Sprintf(`WITH
 intervals AS (SELECT * FROM contentkit_activity_intervals),
 receipts AS (SELECT content_id,content_version_id,subject_kind,subject,receipt_id,
 argMax(tuple(occurred_at,active_ms,covered_units,unit_attention,unit_total,unit_scale,resume,revision),version) AS c,
 argMax(access_context,version) AS a,max(version) AS ver
 FROM %[1]s.engagement_segments WHERE tenant=? AND content_kind=? AND %[2]s AND %[3]s
 AND content_id IN (SELECT content_id FROM intervals)
 AND occurred_at>=(SELECT min(start_at) FROM intervals) AND occurred_at<(SELECT max(end_at) FROM intervals)
 GROUP BY content_id,content_version_id,subject_kind,subject,receipt_id),
 interval_measurements AS (%[4]s),
 %[5]s,
 qualified_versions AS (SELECT content_kind AS interval_id,content_id,subject_kind,subject,qualified,completed,attention_quality,days FROM scored),
 qualified_people AS (SELECT interval_id,content_id,subject_kind,subject,max(completed) AS completed,max(attention_quality) AS quality,
 groupUniqArrayArray(days) AS days FROM qualified_versions WHERE qualified GROUP BY interval_id,content_id,subject_kind,subject),
 feedback_source AS (SELECT content_id,subject_kind,subject,signal_type,event_id,argMax(tuple(occurred_at,value),version) AS c
 FROM %[1]s.signals WHERE tenant=? AND content_kind=? AND content_version_id='' AND signal_type NOT IN ('view','preview') AND %[2]s AND %[3]s
 AND content_id IN (SELECT content_id FROM intervals)
 GROUP BY content_id,subject_kind,subject,signal_type,event_id),
 feedback AS (SELECT i.interval_id,c.subject_kind,c.subject,sum(c.c.2) AS value
 FROM intervals i INNER JOIN feedback_source c ON i.content_id=c.content_id
 WHERE c.c.1>=i.start_at AND c.c.1<i.end_at AND c.c.2!=0 GROUP BY i.interval_id,c.subject_kind,c.subject)
SELECT r.interval_id,count(),countIf(r.completed),sum(r.quality),countIf(length(r.days)>1),countIf(f.value>0),countIf(f.value<0)
FROM qualified_people r LEFT JOIN feedback f ON r.interval_id=f.interval_id AND r.subject_kind=f.subject_kind AND r.subject=f.subject
GROUP BY r.interval_id%[6]s`, st.db, st.notErased(), shard, intervalCanonical, measured, rankingSettings)
	rows, err := st.conn.Query(clickhouse.Context(ctx, clickhouse.WithExternalTable(table)), query,
		tenant, kind, b.shards, b.shard, scope.Domain, scope.Domain, scope.Language, scope.Language, tenant, kind, b.shards, b.shard)
	if err != nil {
		return fmt.Errorf("signal: normalized popularity: %w", err)
	}
	defer rows.Close()
	batch := map[string]intervalMetrics{}
	for rows.Next() {
		var id string
		var m intervalMetrics
		if err := rows.Scan(&id, &m.readers, &m.completers, &m.engagement, &m.returning, &m.positive, &m.negative); err != nil {
			return err
		}
		batch[id] = m
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, m := range batch {
		sum := into[id]
		if sum == nil {
			sum = &intervalMetrics{}
			into[id] = sum
		}
		sum.readers += m.readers
		sum.completers += m.completers
		sum.engagement += m.engagement
		sum.returning += m.returning
		sum.positive += m.positive
		sum.negative += m.negative
	}
	return nil
}

func (st *Store) scoreNormalized(ctx context.Context, tenant, kind string, intervals []activityInterval, eligible map[string]uint64, metrics map[string]*intervalMetrics,
	n NormalizationOptions, rankExpr string, limit int) ([]PopularHit, error) {
	table, err := ext.NewTable("contentkit_activity_segments", ext.Column("content_id", "String"), ext.Column("start_at", "DateTime64(6, 'UTC')"),
		ext.Column("end_at", "DateTime64(6, 'UTC')"), ext.Column("eligible", "UInt64"), ext.Column("readers", "UInt64"), ext.Column("completers", "UInt64"),
		ext.Column("engagement", "Float64"), ext.Column("returning", "UInt64"), ext.Column("positive", "UInt64"), ext.Column("negative", "UInt64"))
	if err != nil {
		return nil, err
	}
	for _, i := range intervals {
		m := metrics[i.id]
		if m == nil {
			m = &intervalMetrics{}
		}
		if err := table.Append(i.content, i.from, i.until, eligible[i.cohort], m.readers, m.completers, m.engagement, m.returning, m.positive, m.negative); err != nil {
			return nil, err
		}
	}
	query := fmt.Sprintf(`WITH
 segments AS (
 SELECT content_id,toFloat64(dateDiff('microsecond',start_at,end_at)) AS duration,
  if(eligible>0,toFloat64(readers)/eligible,0.) AS raw_rate,
  if(eligible>0,toFloat64(readers)/(eligible+%g),0.) AS rate,
  (engagement+%g)/(readers+%g) AS engage,
  (completers+%g)/(readers+%g) AS finish,
  (positive+%g)/(positive+negative+%g) AS approve,
  returning/(readers+%g) AS revisit,
  readers>eligible AS invalid
 FROM contentkit_activity_segments
),
normalized AS (
 SELECT content_id,sum(duration*raw_rate)/sum(duration) AS normalized_raw_rate,
  sum(duration*rate)/sum(duration) AS normalized_rate,
  sum(duration*rate*engage)/sum(duration) AS normalized_engagement,
  sum(duration*rate*finish)/sum(duration) AS normalized_completion,
  sum(duration*rate*approve)/sum(duration) AS normalized_approval,
  sum(duration*rate*revisit)/sum(duration) AS normalized_revisit,
  max(invalid) AS invalid_population
 FROM segments GROUP BY content_id
)
SELECT content_id,normalized_raw_rate,normalized_rate,normalized_engagement,normalized_completion,normalized_approval,normalized_revisit,
 invalid_population,(%s) AS rank_score
FROM normalized WHERE normalized_rate>0 OR invalid_population ORDER BY rank_score DESC,content_id ASC LIMIT ?%s`,
		n.PriorWeight, n.Quality.Engagement*n.Quality.EngagementWeight, n.Quality.EngagementWeight,
		n.Quality.Completion*n.Quality.CompletionWeight, n.Quality.CompletionWeight,
		n.Quality.Approval*n.Quality.ApprovalWeight, n.Quality.ApprovalWeight, n.Quality.RevisitWeight,
		rankExpr, rankingSettings)
	rows, err := st.conn.Query(clickhouse.Context(ctx, clickhouse.WithExternalTable(table)), query, limit)
	if err != nil {
		return nil, fmt.Errorf("signal: normalized popularity: %w", err)
	}
	defer rows.Close()
	var hits []PopularHit
	for rows.Next() {
		var id string
		var m NormalizationMetrics
		var score float64
		var invalid bool
		if err := rows.Scan(&id, &m.RawRate, &m.Rate, &m.Engagement, &m.Completion, &m.Approval, &m.Revisit, &invalid, &score); err != nil {
			return nil, err
		}
		if invalid {
			return nil, fmt.Errorf("signal: trusted readers exceed observed eligible population")
		}
		hits = append(hits, PopularHit{ContentRef: contentref.New(tenant, kind, id), Score: score, Normalization: m})
	}
	return hits, rows.Err()
}

// Joined ClickHouse columns retain qualified headers when a name appears on
// both sides. Export explicit names before the generic measurement aggregation;
// otherwise content_id can bind to an outer ranking join as a free identifier.
func intervalMeasurementProjection() string {
	return `i.interval_id AS content_kind,receipt.content_id AS content_id,receipt.content_version_id AS content_version_id,
 receipt.subject_kind AS subject_kind,receipt.subject AS subject,receipt.receipt_id AS receipt_id,receipt.c AS c,receipt.ver AS ver`
}
