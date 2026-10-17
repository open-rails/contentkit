package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/signal"
)

// ActivitySigner signs domain-separated activity receipts. The media reader
// implements it with its persistent delivery key; non-media hosts may supply
// their own signer. No signing key is generated implicitly.
type ActivitySigner interface {
	SignTicket(scope string, expires time.Time) string
	VerifyTicket(ticket, scope string) error
}

// ActivityTarget is the content a client is about to display or consume.
type ActivityTarget struct {
	ContentID string `json:"content_id"`
	VersionID string `json:"version_id,omitempty"`
}

type ActivityContextsInput struct {
	Session  string           `json:"session,omitempty"`
	Language string           `json:"language,omitempty"`
	Targets  []ActivityTarget `json:"targets"`
}

// ActivityContext is an opaque, subject-bound snapshot. OccurredAt is the
// server's observation time and stays fixed across every view checkpoint.
type ActivityContext struct {
	ActivityTarget
	Token      string    `json:"token"`
	OccurredAt time.Time `json:"occurred_at"`
}

type ActivityContexts struct {
	Enabled  bool              `json:"enabled"`
	Contexts []ActivityContext `json:"contexts"`
}

type activityReceipt struct {
	ID, Tenant, Subject string
	Target              ActivityTarget
	ResolvedVersion     string
	Access              signal.AccessContext
}

const activityReceiptTTL = 24 * time.Hour

func activitySubject(s signal.Subject) string {
	h := sha256.Sum256([]byte(s.Kind() + "\x00" + s.Key()))
	return hex.EncodeToString(h[:])
}

// ActivityContexts captures eligibility at display/open time, before the
// browser queues its beacons. Its population observation includes subscribers
// browsing free content, not only subscribers opening a paid gallery.
func (s *Service) ActivityContexts(ctx context.Context, actor access.Actor, in ActivityContextsInput) (ActivityContexts, error) {
	out := ActivityContexts{Contexts: []ActivityContext{}}
	if s.o.ActivitySigner == nil {
		return out, nil
	}
	if err := s.signals(); err != nil {
		return out, err
	}
	if len(in.Targets) == 0 || len(in.Targets) > maxSignals {
		return out, fmt.Errorf("%w: 1-%d activity targets", ErrInvalid, maxSignals)
	}
	subject, err := s.subject(actor, in.Session)
	if err != nil {
		return out, err
	}
	language, err := s.displayLanguage(in.Language)
	if err != nil {
		return out, err
	}
	ids := make([]string, 0, len(in.Targets))
	for _, t := range in.Targets {
		if contentref.ValidateID(t.ContentID) != nil || t.VersionID != "" && contentref.ValidateID(t.VersionID) != nil {
			return out, fmt.Errorf("%w: invalid activity target", ErrInvalid)
		}
		ids = append(ids, t.ContentID)
	}
	facts, err := s.activityFacts(ctx, ids, language)
	if err != nil {
		return out, err
	}
	refs := make([]contentref.ContentRef, len(in.Targets))
	selected := make([]activityFact, len(in.Targets))
	items := make([]access.Item, len(in.Targets))
	for i, t := range in.Targets {
		versions := facts[t.ContentID]
		found := false
		for _, f := range versions {
			if t.VersionID == "" || t.VersionID == f.version {
				selected[i], found = f, true
				break
			}
		}
		if !found {
			return out, fmt.Errorf("%w: activity content %s", ErrNotFound, t.ContentID)
		}
		f := selected[i]
		refs[i] = contentref.NewVersion(s.o.Tenant, f.kind, t.ContentID, f.version)
		items[i] = access.Item{Content: t.ContentID, Version: f.version, Rule: f.rule.Previewed(s.o.Kinds.Kinds[f.kind]), Groups: f.groups}
	}
	visible, err := s.o.Resolver.Resolve(ctx, refs, actor)
	if err != nil {
		return out, err
	}
	decisions, err := s.o.Gate.Decide(ctx, actor, items)
	if err != nil && (!errors.Is(err, access.ErrUnavailable) || len(decisions) != len(items)) {
		return out, err
	}
	filter, filterErr := s.o.Gate.Filter(ctx, actor)
	if filterErr != nil && !errors.Is(filterErr, access.ErrUnavailable) {
		return out, filterErr
	}
	var tiers []string
	if filter != nil {
		for _, tier := range s.o.Kinds.Tiers {
			if filter.Tier(tier) {
				tiers = append(tiers, tier)
			}
		}
	}
	slices.Sort(tiers)
	domain, _ := s.o.Kinds.DomainFor(gate.Domain(ctx))
	// Match the signal store's precision so the first receipt belongs to its
	// own observed coverage interval.
	at := s.now().UTC().Truncate(time.Microsecond)
	kinds := map[string]bool{}
	for i, t := range in.Targets {
		if !visible[refs[i].Key()].Visible {
			return out, fmt.Errorf("%w: activity content %s", ErrNotFound, t.ContentID)
		}
		f, d := selected[i], decisions[i]
		kinds[f.kind] = true
		classification, audience, audienceTiers := activityAudience(f.rule, at)
		viewer := "none"
		switch {
		case d.Unknown:
			viewer = "unknown"
		case d.Allowed:
			viewer = "full"
		case d.Preview != nil:
			viewer = "preview"
		}
		// Editing authority is not genuine audience access and must not feed
		// readership. The gate verdict above intentionally ignores editor bypass.
		ruleJSON, _ := json.Marshal(f.rule)
		revision := sha256.Sum256(append([]byte(f.version+"\x00"), ruleJSON...))
		receipt := activityReceipt{ID: contentref.NewID(), Tenant: s.o.Tenant, Subject: activitySubject(subject), Target: t,
			ResolvedVersion: f.version, Access: signal.AccessContext{ObservedAt: at, ContentAccess: classification,
				ViewerAccess: viewer, PolicyRevision: hex.EncodeToString(revision[:]), Audience: audience, AudienceTiers: audienceTiers,
				EligibleTiers: tiers, EligibilityKnown: filterErr == nil && filter != nil && !filter.Unknown(), Domain: domain.Name, Language: language}}
		body, err := json.Marshal(receipt)
		if err != nil {
			return out, err
		}
		encoded := base64.RawURLEncoding.EncodeToString(body)
		token := encoded + "." + s.o.ActivitySigner.SignTicket("activity:"+encoded, at.Add(activityReceiptTTL))
		out.Contexts = append(out.Contexts, ActivityContext{ActivityTarget: t, Token: token, OccurredAt: at})
	}
	observations := make([]signal.PopulationObservation, 0, len(kinds))
	for kind := range kinds {
		observations = append(observations, signal.PopulationObservation{ID: contentref.NewID(), ContentKind: kind, Subject: subject,
			ObservedAt: at, Domain: domain.Name, Language: language, EligibleTiers: tiers,
			EligibilityKnown: filterErr == nil && filter != nil && !filter.Unknown()})
	}
	if s.o.MeasuredEngagement {
		if err := s.o.Hub.Signals().RecordPopulation(ctx, s.o.Tenant, observations); err != nil {
			return ActivityContexts{}, err
		}
	}
	out.Enabled = true
	return out, nil
}

func activityAudience(rule access.Rule, at time.Time) (string, string, []string) {
	if rule.Free(at) {
		return "public", "public", nil
	}
	if rule.Level == access.TierLevel {
		tiers := slices.Clone(rule.Tiers)
		slices.Sort(tiers)
		return "restricted", "tier:" + strings.Join(tiers, ","), tiers
	}
	return "restricted", "unknown", nil
}

func (s *Service) readActivityReceipt(token string, subject signal.Subject, target ActivityTarget) (activityReceipt, error) {
	var receipt activityReceipt
	if s.o.ActivitySigner == nil || len(token) > 8192 {
		return receipt, fmt.Errorf("%w: activity receipt unavailable", ErrInvalid)
	}
	body, signature, ok := strings.Cut(token, ".")
	if !ok {
		return receipt, fmt.Errorf("%w: invalid activity receipt", ErrInvalid)
	}
	if err := s.o.ActivitySigner.VerifyTicket(signature, "activity:"+body); err != nil {
		return receipt, fmt.Errorf("%w: activity receipt: %w", ErrInvalid, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.Tenant != s.o.Tenant ||
		receipt.Subject != activitySubject(subject) || receipt.Target != target || receipt.ID == "" || receipt.Access.ObservedAt.IsZero() {
		return activityReceipt{}, fmt.Errorf("%w: activity receipt target or subject differs", ErrInvalid)
	}
	return receipt, nil
}

func expiredActivity(err error) bool { return errors.Is(err, token.ErrExpired) }

type activityFact struct {
	kind, version string
	rule          access.Rule
	groups        []string
}

func (s *Service) activityFacts(ctx context.Context, ids []string, language string) (map[string][]activityFact, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT f.content_id,f.kind,f.version_id,c.access,v.access,f.group_ids
 FROM `+s.t.facets+` f JOIN `+s.t.items+` c ON c.tenant_id=f.tenant_id AND c.id=f.content_id
 JOIN `+s.t.versions+` v ON v.tenant_id=f.tenant_id AND v.id=f.version_id
 WHERE f.tenant_id=$1 AND f.content_id=ANY($2::text[])
 ORDER BY f.content_id, $3=ANY(f.languages) DESC, f.position, f.version_id`, s.o.Tenant, ids, language)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]activityFact{}
	for rows.Next() {
		var id string
		var f activityFact
		var content access.Rule
		var override *access.Rule
		if err := rows.Scan(&id, &f.kind, &f.version, &content, &override, &f.groups); err != nil {
			return nil, err
		}
		f.rule = access.Effective(content, override)
		out[id] = append(out[id], f)
	}
	return out, rows.Err()
}
