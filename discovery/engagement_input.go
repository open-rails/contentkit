package discovery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

type PageEngagement = signal.PageEngagement
type WatchedRange = signal.WatchedRange

// EngagementSnapshot is one cumulative measured segment of a visit. Page
// changes stay local; clients checkpoint about every two minutes and on exit.
// Opening a route, preloading an image, and seeking are not measurements.
type EngagementSnapshot struct {
	ContentID     string           `json:"content_id"`
	VersionID     string           `json:"version_id,omitempty"`
	VisitID       string           `json:"visit_id"`
	OriginClickID string           `json:"origin_click_id,omitempty"`
	SegmentID     string           `json:"segment_id"`
	Revision      uint64           `json:"revision"`
	ActivityToken string           `json:"activity_token"`
	ActiveMS      uint32           `json:"active_ms"`
	Resume        string           `json:"resume,omitempty"`
	Pages         []PageEngagement `json:"pages,omitempty"`
	Watched       []WatchedRange   `json:"watched,omitempty"`
}

// EngagementBatch names the stable browser identity only for a guest. The
// server resolves signed-in subjects and checks every receipt against them.
type EngagementBatch struct {
	Session   string               `json:"session,omitempty"`
	Snapshots []EngagementSnapshot `json:"snapshots"`
}

// RecordEngagement records measured intervals under their original trusted
// access receipt. Short positive partials are retained; person qualification
// happens after aggregation, so two legitimate 600ms partials are not lost.
func (s *Service) RecordEngagement(ctx context.Context, actor access.Actor, in EngagementBatch) error {
	if !s.o.MeasuredEngagement {
		return ErrNoSignals
	}
	if err := s.signals(); err != nil {
		return err
	}
	if len(in.Snapshots) == 0 || len(in.Snapshots) > maxSignals {
		return fmt.Errorf("%w: 1-%d engagement snapshots", ErrInvalid, maxSignals)
	}
	subject, err := s.subject(actor, in.Session)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(in.Snapshots))
	for _, x := range in.Snapshots {
		if contentref.ValidateID(x.ContentID) != nil || x.VersionID != "" && contentref.ValidateID(x.VersionID) != nil ||
			strings.TrimSpace(x.VisitID) == "" || strings.TrimSpace(x.SegmentID) == "" || x.Revision == 0 ||
			x.ActivityToken == "" || identifiers(x.VisitID, x.SegmentID, x.Resume, x.OriginClickID) != nil {
			return fmt.Errorf("%w: engagement needs a content, stable visit/segment, positive revision and original activity receipt", ErrInvalid)
		}
		ids = append(ids, x.ContentID)
	}
	targets, err := s.targets(ctx, ids)
	if err != nil {
		return err
	}
	refs := make([]contentref.ContentRef, 0, len(targets))
	for id, target := range targets {
		refs = append(refs, contentref.New(s.o.Tenant, target.kind, id))
	}
	visible, err := s.o.Resolver.Resolve(ctx, refs, actor)
	if err != nil {
		return err
	}
	out := make([]signal.Signal, 0, len(in.Snapshots))
	for _, x := range in.Snapshots {
		target, ok := targets[x.ContentID]
		ref := contentref.New(s.o.Tenant, target.kind, x.ContentID)
		if !ok || !visible[ref.Key()].Visible {
			return fmt.Errorf("%w: engagement content", ErrNotFound)
		}
		receipt, err := s.readActivityReceipt(x.ActivityToken, subject, ActivityTarget{ContentID: x.ContentID, VersionID: x.VersionID})
		if err != nil {
			if expiredActivity(err) {
				continue
			}
			return err
		}
		facts, ok := target.versions[receipt.ResolvedVersion]
		if !ok || receipt.ResolvedVersion == "" {
			return fmt.Errorf("%w: engagement version", ErrNotFound)
		}
		// Permit clock/timer rounding, not hours of invented active attention.
		elapsed := s.now().Sub(receipt.Access.ObservedAt) + time.Second
		if elapsed < 0 || time.Duration(x.ActiveMS)*time.Millisecond > elapsed {
			return fmt.Errorf("%w: attention exceeds the observed interval", ErrInvalid)
		}
		var m signal.Measurement
		switch target.presentation {
		case "reader":
			if len(x.Watched) != 0 || facts.pages == nil || *facts.pages <= 0 {
				return fmt.Errorf("%w: measured reader pages are required", ErrInvalid)
			}
			m, err = signal.MeasurePages(x.Pages, uint32(*facts.pages), x.ActiveMS)
		case "post":
			if len(x.Watched) != 0 {
				return fmt.Errorf("%w: a post has page coverage", ErrInvalid)
			}
			m, err = signal.MeasurePages(x.Pages, 1, x.ActiveMS)
		case "player":
			if len(x.Pages) != 0 || facts.duration == nil || *facts.duration <= 0 || *facts.duration > 86_400_000 {
				return fmt.Errorf("%w: played timeline ranges are required", ErrInvalid)
			}
			m, err = signal.MeasurePlayback(x.Watched, uint32(*facts.duration), x.ActiveMS)
		default:
			return fmt.Errorf("%w: this content kind has no measured reader", ErrInvalid)
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		m.VersionID, m.VisitID, m.SegmentID = receipt.ResolvedVersion, x.VisitID, x.SegmentID
		m.OriginClickID = x.OriginClickID
		sig := signal.Signal{ContentRef: ref.WithVersion(receipt.ResolvedVersion), Subject: subject,
			Type: signal.TypeEngagement, EventID: receipt.ID, Revision: x.Revision, OccurredAt: receipt.Access.ObservedAt,
			Resume: x.Resume, Access: &receipt.Access, Measurement: &m}
		if receipt.Access.ViewerAccess != "full" {
			continue
		}
		out = append(out, sig)
	}
	return s.o.Hub.RecordSignals(ctx, out)
}
