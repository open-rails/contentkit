package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

const accessPayloadKey = "contentkit_access"

func (a *AccessContext) validate() error {
	if a == nil {
		return nil
	}
	if a.ObservedAt.IsZero() {
		return fmt.Errorf("signal: access observed_at is required")
	}
	if !slices.Contains([]string{"public", "restricted", "unknown"}, a.ContentAccess) ||
		!slices.Contains([]string{"full", "preview", "none", "unknown"}, a.ViewerAccess) {
		return fmt.Errorf("signal: invalid access classification")
	}
	if err := checkIdentifiers("PolicyRevision", a.PolicyRevision, "Audience", a.Audience, "Domain", a.Domain, "Language", a.Language); err != nil {
		return err
	}
	return validateTiers(append(slices.Clone(a.AudienceTiers), a.EligibleTiers...))
}

func validateTiers(tiers []string) error {
	if len(tiers) > 128 {
		return fmt.Errorf("signal: too many eligibility tiers")
	}
	for _, tier := range tiers {
		if strings.TrimSpace(tier) == "" {
			return fmt.Errorf("signal: empty eligibility tier")
		}
		if err := checkLen("tier", tier, MaxIdentifierBytes); err != nil {
			return err
		}
	}
	return nil
}

func signalPayload(s Signal) (string, error) {
	payload := maps.Clone(s.Payload)
	if _, exists := payload[accessPayloadKey]; exists {
		return "", fmt.Errorf("signal: %s is reserved for trusted access", accessPayloadKey)
	}
	if s.Access != nil {
		if payload == nil {
			payload = map[string]any{}
		}
		payload[accessPayloadKey] = s.Access
	}
	if len(payload) == 0 {
		return "", nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("signal: marshal payload: %w", err)
	}
	if err := checkLen("Payload", string(b), MaxPayloadBytes); err != nil {
		return "", err
	}
	return string(b), nil
}

// RecordPopulation records independent activity and its event-time eligibility.
// An observation ID is immutable and reused by retries. Account erasure fences
// and removes these observations just like consumption events.
func (st *Store) RecordPopulation(ctx context.Context, tenant string, observations []PopulationObservation) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	if len(observations) > MaxSignalsPerBatch {
		return &LimitError{Field: "population observations per batch", Limit: MaxSignalsPerBatch, Got: len(observations)}
	}
	for _, o := range observations {
		if err := o.Subject.Validate(); err != nil {
			return err
		}
		if o.ID == "" || o.ContentKind == "" || o.ObservedAt.IsZero() {
			return fmt.Errorf("signal: population observation ID, content kind and observed_at are required")
		}
		if err := checkIdentifiers("ID", o.ID, "ContentKind", o.ContentKind, "Subject", o.Subject.Key(), "Domain", o.Domain, "Language", o.Language); err != nil {
			return err
		}
		if err := validateTiers(o.EligibleTiers); err != nil {
			return err
		}
	}
	const perRow = 10
	parts, args, sizes := make([]string, 0, len(observations)), make([]any, 0, perRow*len(observations)), make([]int, 0, len(observations))
	for _, o := range observations {
		tiers := slices.Clone(o.EligibleTiers)
		slices.Sort(tiers)
		tiers = slices.Compact(tiers)
		if tiers == nil {
			tiers = []string{}
		}
		parts = append(parts, "SELECT ? AS tenant, ? AS content_kind, ? AS subject_kind, ? AS subject, ? AS observation_id, toDateTime64(?,6,'UTC') AS observed_at, ? AS domain, ? AS language, ? AS eligible_tiers, ? AS eligibility_known")
		args = append(args, tenant, o.ContentKind, o.Subject.Kind(), o.Subject.Key(), o.ID, o.ObservedAt.UTC().Format("2006-01-02 15:04:05.000000"), o.Domain, o.Language, tiers, o.EligibilityKnown)
		sizes = append(sizes, inlinedSize(parts[len(parts)-1], args[len(args)-perRow:]))
	}
	for start, end := 0, 0; start < len(observations); start = end {
		end = insertEnd(sizes, start)
		query := fmt.Sprintf(`INSERT INTO %s.measured_population_observations
(tenant,content_kind,subject_kind,subject,observation_id,observed_at,domain,language,eligible_tiers,eligibility_known)
SELECT tenant,content_kind,subject_kind,subject,observation_id,observed_at,domain,language,eligible_tiers,eligibility_known
FROM (%s) WHERE %s`, st.db, strings.Join(parts[start:end], " UNION ALL "), st.notErased())
		if err := st.conn.Exec(ctx, query, args[start*perRow:end*perRow]...); err != nil {
			return fmt.Errorf("signal: record population: %w", err)
		}
		first := map[[3]string]time.Time{}
		for _, o := range observations[start:end] {
			key := [3]string{o.ContentKind, o.Domain, o.Language}
			if at, ok := first[key]; !ok || o.ObservedAt.Before(at) {
				first[key] = o.ObservedAt.UTC()
			}
		}
		for key, at := range first {
			if err := st.conn.Exec(ctx, "INSERT INTO "+st.db+".measured_population_coverage (tenant,content_kind,domain,language,started_at) SELECT ?,?,?,?,toDateTime64(?,6,'UTC')", tenant, key[0], key[1], key[2], at.Format("2006-01-02 15:04:05.000000")); err != nil {
				return fmt.Errorf("signal: record population coverage: %w", err)
			}
		}
	}
	return nil
}

// PopulationCoverage returns the start of trusted instrumentation. The
// non-personal watermark survives erasure; no activity inside a window is a
// valid empty normalized ranking rather than permission to reuse legacy data.
func (st *Store) PopulationCoverage(ctx context.Context, tenant, kind string, scope ActivityScope, window Window) (time.Time, bool, error) {
	if err := window.Validate(); err != nil {
		return time.Time{}, false, err
	}
	rows, err := st.conn.Query(ctx, "SELECT count(),min(started_at) FROM "+st.db+".measured_population_coverage WHERE tenant=? AND content_kind=? AND (?='' OR domain=?) AND (?='' OR language=?)", tenant, kind, scope.Domain, scope.Domain, scope.Language, scope.Language)
	if err != nil {
		return time.Time{}, false, err
	}
	defer rows.Close()
	var count uint64
	var at time.Time
	if rows.Next() {
		if err := rows.Scan(&count, &at); err != nil {
			return time.Time{}, false, err
		}
	}
	if count == 0 || !window.To.IsZero() && !at.Before(window.To) {
		return time.Time{}, false, rows.Err()
	}
	if !window.From.IsZero() && at.Before(window.From) {
		at = window.From
	}
	return at, true, rows.Err()
}
