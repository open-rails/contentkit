// Package engagementtest constructs explicit loaded/visible page observations
// for native cross-package tests. It is not an ingestion compatibility layer.
package engagementtest

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/contentkit/signal"
)

const TypeReading = "fixture-reader"
const TypePreview = "fixture-preview"

type Observation struct {
	signal.ContentRef
	Subject     signal.Subject
	Type        string
	EventID     string
	Revision    uint64
	OccurredAt  time.Time
	DurationS   uint32
	Progress    uint32 // explicit first N page observations in this fixture
	ProgressMax uint32
	Value       float64
	Resume      string
	Payload     map[string]any
	Access      *signal.AccessContext
}

func (f Observation) Signals() []signal.Signal {
	base := signal.Signal{ContentRef: f.ContentRef, Subject: f.Subject, Type: f.Type, EventID: f.EventID, Revision: f.Revision, OccurredAt: f.OccurredAt, Resume: f.Resume, Value: f.Value, Payload: f.Payload, Access: f.Access}
	if f.Type != TypeReading && f.Type != TypePreview {
		return []signal.Signal{base}
	}
	// An open or preload with no observed pages is never engagement.
	if f.Progress == 0 {
		return nil
	}
	if f.ProgressMax < f.Progress {
		panic("engagementtest: explicit observed pages exceed the gallery length")
	}
	total, covered := f.ProgressMax, f.Progress
	active := max(uint64(f.DurationS)*1000, uint64(covered)*1000)
	version := f.Version()
	if version == "" {
		version = "fixture-edition"
	}
	var out []signal.Signal
	// Stable page chunks keep long galleries below the real snapshot limits.
	for start := uint32(0); start < covered; start += 64 {
		end := min(start+64, covered)
		ms := uint32(min(active*uint64(end-start)/uint64(covered), uint64(signal.MaxEngagementMS)))
		pages := make([]signal.PageEngagement, end-start)
		for i := range pages {
			pages[i] = signal.PageEngagement{Index: start + uint32(i), ActiveMS: ms / (end - start), Ranges: []signal.CoverageRange{{From: 0, To: 10000}}}
		}
		m, err := signal.MeasurePages(pages, total, ms)
		if err != nil {
			panic(err)
		}
		m.VersionID, m.VisitID, m.SegmentID = version, f.EventID, fmt.Sprintf("%s-%d", f.EventID, start)
		row := base
		row.ContentRef = row.WithVersion(version)
		row.Type = signal.TypeEngagement
		row.Measurement = &m
		row.Revision = max(f.Revision, 1)
		row.EventID = m.SegmentID
		if row.Access == nil {
			row.Access = &signal.AccessContext{ObservedAt: f.OccurredAt, ContentAccess: "public", ViewerAccess: "full", Audience: "public", EligibilityKnown: true}
		}
		if f.Type == TypePreview {
			copy := *row.Access
			copy.ViewerAccess = "preview"
			row.Access = &copy
		}
		out = append(out, row)
	}
	return out
}
func Native(in []Observation) []signal.Signal {
	var out []signal.Signal
	for _, row := range in {
		out = append(out, row.Signals()...)
	}
	return out
}

type Store interface {
	RecordSignals(context.Context, string, []signal.Signal) error
}

func Record(ctx context.Context, store Store, tenant string, in []Observation) error {
	rows := Native(in)
	for start := 0; start < len(rows); start += signal.MaxSignalsPerBatch {
		if err := store.RecordSignals(ctx, tenant, rows[start:min(start+signal.MaxSignalsPerBatch, len(rows))]); err != nil {
			return err
		}
	}
	return nil
}
