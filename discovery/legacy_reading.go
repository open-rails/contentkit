package discovery

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

type ImportLegacyReading struct {
	ContentID   string
	VersionID   string
	Subject     signal.Subject
	FirstReadAt *time.Time
	LastReadAt  time.Time
	Progress    uint32
	ProgressMax uint32
	Completed   bool
	Resume      string
	Revision    uint64
}

// ImportLegacyReading preserves migration bookmarks without fabricating actual
// reading events. It is called only by the authorized bulk importer.
func (s *Service) ImportLegacyReading(ctx context.Context, in []ImportLegacyReading) error {
	if err := s.signals(); err != nil {
		return err
	}
	if len(in) == 0 {
		return nil
	}
	ids := make([]string, len(in))
	for i, row := range in {
		if err := contentref.ValidateID(row.ContentID); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		ids[i] = row.ContentID
	}
	targets, err := s.targets(ctx, ids)
	if err != nil {
		return err
	}
	out := make([]signal.LegacyReading, 0, len(in))
	for _, row := range in {
		target, ok := targets[row.ContentID]
		if !ok {
			return ErrNotFound
		}
		if row.VersionID != "" {
			if _, ok := target.versions[row.VersionID]; !ok {
				return ErrNotFound
			}
		}
		first := row.LastReadAt
		if row.FirstReadAt != nil {
			first = *row.FirstReadAt
		}
		out = append(out, signal.LegacyReading{ContentRef: contentref.NewVersion(s.o.Tenant, target.kind, row.ContentID, row.VersionID), Subject: row.Subject,
			FirstReadAt: first, LastReadAt: row.LastReadAt, Progress: row.Progress, ProgressMax: row.ProgressMax, Completed: row.Completed, Resume: row.Resume, Revision: row.Revision})
	}
	for start := 0; start < len(out); start += signal.MaxSignalsPerBatch {
		if err := s.o.Hub.ImportLegacyReading(ctx, out[start:min(start+signal.MaxSignalsPerBatch, len(out))]); err != nil {
			return err
		}
	}
	return nil
}
