package discoverystore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/signal"
)

// History pages the contents the signed-in reader engaged with, most
// recent first they already know. Current domain,
// publication, preferences and access rules still apply.
func (s *Service) History(ctx context.Context, o discovery.HistoryOptions) (discovery.ContentPage, error) {
	empty := discovery.ContentPage{Contents: []discovery.Card{}}
	if err := s.signals(); err != nil {
		return empty, err
	}
	if !o.Actor.Member() {
		return empty, fmt.Errorf("%w: history belongs to a signed-in reader", ErrInvalid)
	}
	status := signal.HistoryStatus(o.Status)
	switch status {
	case signal.HistoryAny, signal.HistorySeen, signal.HistoryInProgress, signal.HistoryCompleted:
	default:
		return empty, fmt.Errorf("%w: status %q is seen, in_progress or completed", ErrInvalid, o.Status)
	}
	if o.Kind != "" {
		if _, ok := s.o.Kinds.Kinds[o.Kind]; !ok {
			return empty, fmt.Errorf("%w: kind %q is not configured", ErrInvalid, o.Kind)
		}
	}
	offset := 0
	if o.Cursor != "" {
		n, err := strconv.Atoi(o.Cursor)
		if err != nil || n < 0 {
			return empty, fmt.Errorf("%w: cursor", ErrInvalid)
		}
		offset = n
	}
	limit := pageLimit(o.Limit, 24)
	rows, err := s.o.Hub.History(ctx, signal.Subject{UserID: o.Actor.AccountID()}, signal.HistoryOptions{ContentKind: o.Kind, Status: status, Limit: limit + 1, Offset: offset})
	if err != nil {
		return empty, err
	}
	next := ""
	if len(rows) > limit {
		rows, next = rows[:limit], strconv.Itoa(offset+limit)
	}
	q, err := s.compileKnown(ctx, o.Reader, Filter{Kind: o.Kind})
	if err != nil {
		return empty, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if _, ok := s.o.Kinds.Kinds[r.ContentKind]; ok {
			ids = append(ids, r.ContentID)
		}
	}
	picks, err := s.pickAmong(ctx, q, ids)
	if err != nil {
		return empty, err
	}
	cards, err := s.cards(ctx, q, picks, cardOptions{chips: o.CardEntities})
	if err != nil {
		return empty, err
	}
	out := discovery.ContentPage{Contents: cards, NextCursor: next}
	if o.Summary && offset == 0 {
		// Bound the caller's own selection, never another subject's state. Large
		// histories are explicitly lower-bound summaries rather than invented totals.
		const summaryLimit = 100_000
		history, err := s.o.Hub.History(ctx, signal.Subject{UserID: o.Actor.AccountID()}, signal.HistoryOptions{ContentKind: o.Kind, Status: status, Limit: summaryLimit})
		if err != nil {
			return empty, err
		}
		all := make([]string, 0, len(history))
		for _, row := range history {
			all = append(all, row.ContentID)
		}
		sort, err := s.parseSort(ctx, discovery.SortNewest, Filter{Kind: o.Kind})
		if err != nil {
			return empty, err
		}
		out.Summary, err = s.listingSummary(ctx, Filter{Kind: o.Kind}, q, sort, &summaryCandidates{visible: all, relaxed: all, truncated: len(history) == summaryLimit})
		if err != nil {
			return empty, err
		}
	}
	return out, nil
}

// ForgetHistory erases the signed-in reader's signals for one content item,
// one kind, or every kind.
func (s *Service) ForgetHistory(ctx context.Context, actor access.Actor, kind, contentID string) error {
	if err := s.signals(); err != nil {
		return err
	}
	if !actor.Member() {
		return fmt.Errorf("%w: history belongs to a signed-in reader", ErrInvalid)
	}
	subject := signal.Subject{UserID: actor.AccountID()}
	if contentID != "" {
		kinds, err := s.kindsOf(ctx, []string{contentID})
		if err != nil {
			return err
		}
		k, ok := kinds[contentID]
		if !ok || (kind != "" && kind != k) {
			return ErrNotFound
		}
		return s.o.Hub.Forget(ctx, subject, k, contentID)
	}
	ks := []string{kind}
	if kind == "" {
		ks = s.kindNames()
	} else if _, ok := s.o.Kinds.Kinds[kind]; !ok {
		return fmt.Errorf("%w: kind %q is not configured", ErrInvalid, kind)
	}
	for _, k := range ks {
		if err := s.o.Hub.Forget(ctx, subject, k, ""); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) kindsOf(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT id, kind FROM `+s.t.items+` WHERE tenant_id = $1 AND id = ANY($2)`, s.o.Tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		out[id] = kind
	}
	return out, rows.Err()
}

// maxSignals bounds one signals request.
const maxSignals = 100

// subject is the actor's signal subject: the person, else a hash of the
// session id, never stored raw.
func (s *Service) subject(actor access.Actor, session string) (signal.Subject, error) {
	if actor.Member() {
		return signal.Subject{UserID: actor.AccountID()}, nil
	}
	if !actor.Anonymous {
		return signal.Subject{}, auth.ErrForbidden
	}
	session = strings.TrimSpace(session)
	if session == "" || len(session) > 128 {
		return signal.Subject{}, fmt.Errorf("%w: a signed-out client names its session (1-128 characters)", ErrInvalid)
	}
	sum := sha256.Sum256([]byte(s.o.Tenant + "\x00" + session))
	return signal.Subject{AnonKey: "s_" + hex.EncodeToString(sum[:16])}, nil
}

// target is a signalled content item and version as stored.
type target struct {
	kind, presentation string
	versions           map[string]versionFacts
}

type versionFacts struct {
	pages    *int
	duration *int64
}

// RecordSignals records explicit card clicks. Loaded/visible reading attention
// is submitted separately through RecordEngagement.
func (s *Service) RecordSignals(ctx context.Context, actor access.Actor, in discovery.Signals) error {
	if err := s.signals(); err != nil {
		return err
	}
	if len(in.Signals) == 0 || len(in.Signals) > maxSignals {
		return fmt.Errorf("%w: 1-%d signals", ErrInvalid, maxSignals)
	}
	subject, err := s.subject(actor, in.Session)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(in.Signals))
	for _, x := range in.Signals {
		if err := contentref.ValidateID(x.ContentID); err != nil {
			return fmt.Errorf("%w: content_id: %w", ErrInvalid, err)
		}
		if err := identifiers(x.EventID, x.RenderID, x.Surface); err != nil || x.EventID == "" || x.OccurredAt.IsZero() {
			return fmt.Errorf("%w: a signal needs an event_id and occurred_at, and identifiers of at most %d bytes", ErrInvalid, signal.MaxIdentifierBytes)
		}
		ids = append(ids, x.ContentID)
	}
	targets, err := s.targets(ctx, ids)
	if err != nil {
		return err
	}
	refs := make([]contentref.ContentRef, 0, len(targets))
	for id, t := range targets {
		refs = append(refs, contentref.New(s.o.Tenant, t.kind, id))
	}
	seen, err := s.o.Resolver.Resolve(ctx, refs, actor)
	if err != nil {
		return err
	}
	var out []signal.Signal
	for _, x := range in.Signals {
		t, ok := targets[x.ContentID]
		ref := contentref.New(s.o.Tenant, t.kind, x.ContentID)
		if !ok || !seen[ref.Key()].Visible {
			return fmt.Errorf("%w: content %s", ErrNotFound, x.ContentID)
		}
		_, versioned := t.versions[x.VersionID]
		if x.VersionID != "" && !versioned {
			return fmt.Errorf("%w: version %s of content %s", ErrNotFound, x.VersionID, x.ContentID)
		}
		var receipt *activityReceipt
		if x.ActivityToken != "" {
			r, err := s.readActivityReceipt(x.ActivityToken, subject, discovery.ActivityTarget{ContentID: x.ContentID, VersionID: x.VersionID})
			if err != nil {
				if expiredActivity(err) {
					continue
				}
				return err
			}
			receipt = &r
		}
		if x.Type != discovery.TypeClick {
			return fmt.Errorf("%w: only click signals are accepted; reading uses /engagement", ErrInvalid)
		}
		sig := signal.Signal{ContentRef: ref.WithVersion(x.VersionID), Subject: subject, Type: discovery.TypeClick, EventID: x.EventID, Revision: x.Revision, OccurredAt: x.OccurredAt.UTC()}
		if receipt != nil {
			sig.Access = &receipt.Access
			sig.OccurredAt = receipt.Access.ObservedAt
		}

		if x.RenderID != "" || x.Surface != "" || x.Position != 0 {
			sig = sig.WithAttribution(signal.Attribution{RenderID: x.RenderID, Surface: x.Surface, Position: x.Position})
		}
		out = append(out, sig)
	}
	return s.o.Hub.RecordSignals(ctx, out)
}

// identifiers checks client strings against the signal plane's bounds.
func identifiers(values ...string) error {
	for _, v := range values {
		if len(v) > signal.MaxIdentifierBytes {
			return fmt.Errorf("%w: a value is longer than %d bytes", ErrInvalid, signal.MaxIdentifierBytes)
		}
	}
	return nil
}

func (s *Service) targets(ctx context.Context, ids []string) (map[string]target, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT c.id, c.kind, coalesce(v.id, ''), v.pages, v.duration_ms FROM `+s.t.items+` c
 LEFT JOIN `+s.t.versions+` v ON v.tenant_id = c.tenant_id AND v.content_id = c.id WHERE c.tenant_id = $1 AND c.id = ANY($2)`, s.o.Tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]target{}
	for rows.Next() {
		var id, kind, version string
		var f versionFacts
		if err := rows.Scan(&id, &kind, &version, &f.pages, &f.duration); err != nil {
			return nil, err
		}
		t, ok := out[id]
		if !ok {
			k, configured := s.o.Kinds.Kinds[kind]
			if !configured {
				continue
			}
			t = target{kind: kind, presentation: k.Presentation, versions: map[string]versionFacts{"": {}}}
		}
		if version != "" {
			t.versions[version] = f
		}
		out[id] = t
	}
	return out, rows.Err()
}

// RecordExposures records result lists for click attribution.
func (s *Service) RecordExposures(ctx context.Context, actor access.Actor, in discovery.Exposures) error {
	if !actor.Anonymous && !actor.Member() {
		return auth.ErrForbidden
	}
	if err := s.signals(); err != nil {
		return err
	}
	if len(in.Exposures) == 0 || len(in.Exposures) > signal.MaxExposuresPerBatch {
		return fmt.Errorf("%w: 1-%d exposures", ErrInvalid, signal.MaxExposuresPerBatch)
	}
	var subject signal.Subject
	if actor.Member() || in.Session != "" {
		var err error
		if subject, err = s.subject(actor, in.Session); err != nil {
			return err
		}
	}
	var ids []string
	for _, e := range in.Exposures {
		if e.SearchQuery != "" {
			if _, ok := searchKey(e.SearchQuery); !ok {
				return fmt.Errorf("%w: search query must contain 1-%d characters", ErrInvalid, maxSearchQuery)
			}
		}
		switch signal.ExposureStage(e.Stage) {
		case signal.StageServed, signal.StageRendered, signal.StageVisible:
		default:
			return fmt.Errorf("%w: stage %q is served, rendered or visible", ErrInvalid, e.Stage)
		}
		emptySearch := e.Stage == string(signal.StageRendered) && e.Surface == signal.SurfaceSearch && e.QueryID != "" && strings.TrimSpace(e.SearchQuery) != ""
		if e.RenderID == "" || len(e.Shown) == 0 && !emptySearch || len(e.Shown) > signal.MaxShownPerExposure {
			return fmt.Errorf("%w: an exposure needs a render_id and 1-%d placements, or a completed empty search", ErrInvalid, signal.MaxShownPerExposure)
		}
		if err := identifiers(e.RenderID, e.QueryID, e.Surface, e.Ranker, e.Language); err != nil {
			return err
		}
		positions := map[uint32]bool{}
		for _, p := range e.Shown {
			if p.Position == 0 || positions[p.Position] {
				return fmt.Errorf("%w: positions are distinct and 1-based", ErrInvalid)
			}
			positions[p.Position] = true
		}
		for _, p := range e.Shown {
			if err := contentref.ValidateID(p.ContentID); err != nil {
				return fmt.Errorf("%w: content_id: %w", ErrInvalid, err)
			}
			ids = append(ids, p.ContentID)
		}
	}
	kinds, err := s.kindsOf(ctx, ids)
	if err != nil {
		return err
	}
	out := make([]signal.Exposure, 0, len(in.Exposures))
	for _, e := range in.Exposures {
		x := signal.Exposure{RenderID: e.RenderID, Stage: signal.ExposureStage(e.Stage), Revision: e.Revision, QueryID: e.QueryID, SearchQuery: e.SearchQuery, Surface: e.Surface,
			Ranker: e.Ranker, Language: e.Language, Subject: subject, OccurredAt: s.now().UTC()}
		if e.OccurredAt != nil {
			x.OccurredAt = e.OccurredAt.UTC()
		}
		for _, p := range e.Shown {
			kind, ok := kinds[p.ContentID]
			if !ok {
				return fmt.Errorf("%w: content %s", ErrNotFound, p.ContentID)
			}
			placement := signal.Placement{ContentRef: contentref.NewVersion(s.o.Tenant, kind, p.ContentID, p.VersionID), Position: p.Position}
			if p.ActivityToken != "" {
				r, err := s.readActivityReceipt(p.ActivityToken, subject, discovery.ActivityTarget{ContentID: p.ContentID, VersionID: p.VersionID})
				if err != nil {
					if expiredActivity(err) {
						continue
					}
					return err
				}
				placement.Access = &r.Access
			}
			x.Shown = append(x.Shown, placement)
		}
		if len(x.Shown) > 0 || len(e.Shown) == 0 {
			out = append(out, x)
		}
	}
	return s.o.Hub.RecordExposures(ctx, out)
}
