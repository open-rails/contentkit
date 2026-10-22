package versions

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media"
)

// MediaPending is what keeps a version's media from going live: files still
// processing in its manifest, or a publication not yet current for them.
type MediaPending struct {
	Target
	Reason string `json:"reason"`
}

// PendingTx reports the versions whose media is not ready to go live. A
// version without uploads, or of a kind without versioned media, is ready.
// It reads manifests and q's publications and mutates nothing. Uploads a
// client has not committed yet are invisible here; editors guard those.
func (s *Service) PendingTx(ctx context.Context, q pgx.Tx, targets []Target) ([]MediaPending, error) {
	if len(targets) == 0 || s.m.Manifests == nil {
		return nil, nil
	}
	vs, err := s.versions(ctx, q, targets)
	if err != nil {
		return nil, err
	}
	var out []MediaPending
	for _, t := range targets {
		v, ok := vs[t]
		if !ok {
			continue
		}
		k, err := s.published(v)
		if err != nil {
			continue
		}
		reason, err := s.pending(ctx, q, k, t)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			out = append(out, MediaPending{Target: t, Reason: reason})
		}
	}
	return out, nil
}

func (s *Service) pending(ctx context.Context, q querier, k *media.Kind, t Target) (string, error) {
	m, err := s.manifest(ctx, k, t)
	if err != nil {
		return "", err
	}
	if m.Deleted || !slices.ContainsFunc(m.Files, func(f media.File) bool { return f.IsUpload() && !f.Unattached }) {
		return "", nil
	}
	pages, err := pagesOf(k, m)
	if reason, err := notReady(err, "pages are still processing"); reason != "" || err != nil {
		return reason, err
	}
	if ok, err := s.currentFor(ctx, q, t, kindPages, pagesSnapshot{Pages: pages}); err != nil || !ok {
		return "pages are still being published", err
	}
	if k.Publish.Playback == "" {
		return "", nil
	}
	tracks, err := tracksOf(k, m)
	if reason, err := notReady(err, "playback media is still processing"); reason != "" || err != nil {
		return reason, err
	}
	if len(tracks) == 0 {
		// Nothing to play: only a current publication still offering removed tracks waits.
		pb, err := s.current(ctx, q, t, kindPlayback)
		if err != nil || pb == nil {
			return "", err
		}
		var snap playbackSnapshot
		if err := json.Unmarshal(pb.Source, &snap); err != nil || len(snap.Tracks) == 0 {
			return "", err
		}
		return "playback is still being published", nil
	}
	if ok, err := s.currentFor(ctx, q, t, kindPlayback, playbackSnapshot{Tracks: tracks}); err != nil || !ok {
		return "playback is still being published", err
	}
	return "", nil
}

// notReady turns a build precondition into a reason; other errors pass.
func notReady(err error, processing string) (string, error) {
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, errFailed):
		return "a file failed to process: " + strings.TrimPrefix(err.Error(), errFailed.Error()+": "), nil
	case errors.Is(err, ErrNotReady):
		return processing, nil
	}
	return "", err
}

// currentFor reports whether source is v's current publication of kind.
func (s *Service) currentFor(ctx context.Context, q querier, v Target, kind string, source any) (bool, error) {
	body, err := json.Marshal(source)
	if err != nil {
		return false, err
	}
	var ok bool
	err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.publications+`
WHERE tenant_id = $1 AND version_id = $2 AND kind = $3 AND state = 'current' AND source = $4::jsonb)`, s.o.Tenant, v.Version, kind, body).Scan(&ok)
	return ok, err
}
