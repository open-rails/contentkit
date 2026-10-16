package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/signal"
)

// maxAccessHistory bounds both the historical query result and the external
// interval table sent to the signal plane. Exceeding it is an explicit error,
// never a truncated ranking with invented missing availability.
const maxAccessHistory = 100_000

type accessHistoryState struct {
	Kind      string       `json:"kind"`
	LiveFrom  *time.Time   `json:"live_from"`
	Deleted   bool         `json:"deleted"`
	Listing   string       `json:"listing"`
	Languages []string     `json:"languages"`
	Level     access.Level `json:"level"`
	Tiers     []string     `json:"tiers"`
	Group     string       `json:"group_id"`
	FreeAfter *time.Time   `json:"free_after"`
}

type accessHistoryRow struct {
	id, content, version string
	at                   time.Time
	state                accessHistoryState
}

// Availability returns only immutable catalog history. It never treats a
// current rule as evidence of how content was accessible before adoption.
func (s *Service) Availability(ctx context.Context, kind string, window signal.Window, scope signal.ActivityScope) (signal.Availability, error) {
	out := signal.Availability{}
	if err := window.Validate(); err != nil {
		return out, err
	}
	history := pgx.Identifier{s.o.Schema, "content_access_history"}.Sanitize()
	startTable := pgx.Identifier{s.o.Schema, "content_access_history_start"}.Sanitize()
	var start time.Time
	err := s.o.Pool.QueryRow(ctx, `SELECT started_at FROM `+startTable+` WHERE tenant_id=$1`, s.o.Tenant).Scan(&start)
	if err == pgx.ErrNoRows {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	from, until := start.UTC(), s.now().UTC()
	if window.From.After(from) {
		from = window.From
	}
	if !window.To.IsZero() && window.To.Before(until) {
		until = window.To
	}
	if !from.Before(until) {
		return out, nil
	}
	out.From, out.Until, out.Known = from, until, true
	rows, err := s.o.Pool.Query(ctx, `WITH candidates AS (
 SELECT DISTINCT version_id FROM `+history+` WHERE tenant_id=$1 AND snapshot->>'kind'=$2
), prior AS (
 SELECT DISTINCT ON (h.version_id) h.id,h.content_id,h.version_id,h.occurred_at,h.snapshot
 FROM `+history+` h JOIN candidates c USING(version_id)
 WHERE h.tenant_id=$1 AND h.occurred_at<=$3 ORDER BY h.version_id,h.occurred_at DESC,h.id DESC
), recent AS (
 SELECT h.id,h.content_id,h.version_id,h.occurred_at,h.snapshot FROM `+history+` h JOIN candidates c USING(version_id)
 WHERE h.tenant_id=$1 AND h.occurred_at>$3 AND h.occurred_at<$4
) SELECT id::text,content_id,version_id,occurred_at,snapshot FROM (
 SELECT * FROM prior UNION ALL SELECT * FROM recent
) states ORDER BY states.version_id,states.occurred_at,states.id LIMIT $5`, s.o.Tenant, kind, from, until, maxAccessHistory+1)
	if err != nil {
		return out, err
	}
	var states []accessHistoryRow
	for rows.Next() {
		var row accessHistoryRow
		var raw []byte
		if err := rows.Scan(&row.id, &row.content, &row.version, &row.at, &raw); err != nil {
			rows.Close()
			return out, err
		}
		if err := json.Unmarshal(raw, &row.state); err != nil {
			rows.Close()
			return out, err
		}
		states = append(states, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(states) > maxAccessHistory {
		return out, fmt.Errorf("%w: access history exceeds %d intervals", ErrInvalid, maxAccessHistory)
	}
	var intervals []signal.AccessInterval
	for i, row := range states {
		end := until
		if i+1 < len(states) && states[i+1].version == row.version && states[i+1].at.Before(end) {
			end = states[i+1].at
		}
		begin := row.at
		if begin.Before(from) {
			begin = from
		}
		if row.state.Kind != kind || row.state.Deleted || row.state.LiveFrom == nil || row.state.Listing != "listed" ||
			scope.Language != "" && !slices.Contains(row.state.Languages, scope.Language) {
			continue
		}
		if begin.Before(*row.state.LiveFrom) {
			begin = *row.state.LiveFrom
		}
		if !begin.Before(end) {
			continue
		}
		rule := access.Rule{Level: row.state.Level, Tiers: row.state.Tiers, Group: row.state.Group, FreeAfter: row.state.FreeAfter}
		appendPart := func(a, b time.Time) {
			_, audience, tiers := activityAudience(rule, a)
			if a.Before(b) {
				intervals = append(intervals, signal.AccessInterval{ContentID: row.content, From: a, Until: b, Audience: audience, AudienceTiers: tiers})
			}
		}
		if rule.FreeAfter != nil && rule.FreeAfter.After(begin) && rule.FreeAfter.Before(end) {
			appendPart(begin, *rule.FreeAfter)
			appendPart(*rule.FreeAfter, end)
		} else {
			appendPart(begin, end)
		}
	}
	out.Intervals = unionAccessIntervals(intervals)
	return out, nil
}

// A work's simultaneous editions form one full-read audience: public wins;
// otherwise the configured tiers are ORed. Unsupported ACLs remain unknown
// unless a public edition independently opens the work to every reader.
func unionAccessIntervals(in []signal.AccessInterval) []signal.AccessInterval {
	slices.SortFunc(in, func(a, b signal.AccessInterval) int {
		if n := strings.Compare(a.ContentID, b.ContentID); n != 0 {
			return n
		}
		return a.From.Compare(b.From)
	})
	out := make([]signal.AccessInterval, 0, len(in))
	type change struct {
		at       time.Time
		interval signal.AccessInterval
		delta    int
	}
	for first := 0; first < len(in); {
		last := first + 1
		for last < len(in) && in[last].ContentID == in[first].ContentID {
			last++
		}
		changes := make([]change, 0, 2*(last-first))
		for _, interval := range in[first:last] {
			changes = append(changes, change{interval.From, interval, 1}, change{interval.Until, interval, -1})
		}
		slices.SortFunc(changes, func(a, b change) int { return a.at.Compare(b.at) })
		public, unknown := 0, 0
		tiers := map[string]int{}
		previous := changes[0].at
		for i := 0; i < len(changes); {
			at := changes[i].at
			if previous.Before(at) && (public > 0 || unknown == 0 && len(tiers) > 0) {
				next := signal.AccessInterval{ContentID: in[first].ContentID, From: previous, Until: at, Audience: "public"}
				if public == 0 {
					for tier := range tiers {
						next.AudienceTiers = append(next.AudienceTiers, tier)
					}
					slices.Sort(next.AudienceTiers)
					next.Audience = "tier:" + strings.Join(next.AudienceTiers, ",")
				}
				if len(out) > 0 && out[len(out)-1].ContentID == next.ContentID && out[len(out)-1].Audience == next.Audience && out[len(out)-1].Until.Equal(next.From) {
					out[len(out)-1].Until = next.Until
				} else {
					out = append(out, next)
				}
			}
			for i < len(changes) && changes[i].at.Equal(at) {
				c := changes[i]
				switch {
				case c.interval.Audience == "public":
					public += c.delta
				case len(c.interval.AudienceTiers) == 0:
					unknown += c.delta
				default:
					for _, tier := range c.interval.AudienceTiers {
						tiers[tier] += c.delta
						if tiers[tier] == 0 {
							delete(tiers, tier)
						}
					}
				}
				i++
			}
			previous = at
		}
		first = last
	}
	for i := range out {
		s := &out[i]
		hash := sha256.Sum256([]byte(s.ContentID + "\x00" + s.Audience + "\x00" + s.From.Format(time.RFC3339Nano) + "\x00" + s.Until.Format(time.RFC3339Nano)))
		s.ID = hex.EncodeToString(hash[:])
	}
	return out
}
