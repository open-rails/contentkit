package signal

import (
	"context"
	"encoding/json"
	"time"
)

// readingFixture describes intentional loaded page coverage in native tests.
// Progress is a count of explicit first-page observations, not a last-position
// inference in production. The measured reader's high-index/offscreen/zero-time
// cases are tested independently against Measurement and the browser hook.
type readingFixture struct {
	ContentRef
	Subject     Subject
	Type        string
	EventID     string
	Revision    uint64
	OccurredAt  time.Time
	DurationS   uint32
	Progress    uint32
	ProgressMax uint32
	Value       float64
	Score       int16
	Completed   bool
	Resume      string
	Payload     map[string]any
	Access      *AccessContext
	Measurement *Measurement
}

const TypeView = "view"
const TypePreview = "preview"

func (f readingFixture) native() Signal {
	out := Signal{ContentRef: f.ContentRef, Subject: f.Subject, Type: f.Type, EventID: f.EventID, Revision: f.Revision, OccurredAt: f.OccurredAt, Value: f.Value, Resume: f.Resume, Payload: f.Payload, Access: f.Access, Measurement: f.Measurement}
	if f.Type != TypeView && f.Type != TypePreview {
		return out
	}
	total := max(f.ProgressMax, f.Progress, 1)
	covered := min(max(f.Progress, 1), 128)
	active := min(max(f.DurationS*1000, covered*1000), MaxEngagementMS)
	pages := make([]PageEngagement, covered)
	for i := range pages {
		pages[i] = PageEngagement{Index: uint32(i), ActiveMS: active / covered, Ranges: []CoverageRange{{From: 0, To: 10000}}}
	}
	measured, err := MeasurePages(pages, total, active)
	if err != nil {
		panic(err)
	}
	version := f.Version()
	if version == "" {
		version = "fixture-edition"
	}
	measured.VersionID, measured.VisitID, measured.SegmentID = version, f.EventID, f.EventID
	out.ContentRef = out.ContentRef.WithVersion(version)
	out.Type = TypeEngagement
	out.Measurement = &measured
	out.Revision = max(f.Revision, 1)
	if out.Access == nil {
		out.Access = &AccessContext{ObservedAt: f.OccurredAt, ContentAccess: "public", ViewerAccess: "full", Audience: "public", EligibilityKnown: true}
	}
	if f.Type == TypePreview {
		copy := *out.Access
		copy.ViewerAccess = "preview"
		out.Access = &copy
	}
	return out
}
func (f readingFixture) WithAttribution(a Attribution) readingFixture {
	f.Payload = f.native().WithAttribution(a).Payload
	return f
}
func (f readingFixture) Attribution() Attribution     { return f.native().Attribution() }
func (f readingFixture) validate(tenant string) error { return f.native().validate(tenant) }
func recordFixtureSignals(st *Store, ctx context.Context, tenant string, fixtures []readingFixture) error {
	events := make([]Signal, 0, len(fixtures))
	for _, f := range fixtures {
		if (f.Type == TypeView || f.Type == TypePreview) && f.Progress == 0 {
			continue
		}
		events = append(events, f.native())
	}
	return st.RecordSignals(ctx, tenant, events)
}

func insertFixtureSegment(ctx context.Context, conn Conn, tenant string, event Signal) error {
	m := event.Measurement
	access, err := json.Marshal(event.Access)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(struct {
		Pages   []PageEngagement `json:"pages,omitempty"`
		Watched []WatchedRange   `json:"watched,omitempty"`
	}{m.Pages, m.Watched})
	if err != nil {
		return err
	}
	keys, values := make([]uint32, 0, len(m.Attention)), make([]uint64, 0, len(m.Attention))
	for _, pair := range m.Attention {
		keys = append(keys, uint32(pair[0]))
		values = append(values, pair[1])
	}
	q := `INSERT INTO ` + testDB + `.engagement_segments
 (tenant,content_kind,content_id,content_version_id,subject_kind,subject,receipt_id,revision,visit_id,segment_id,occurred_at,
 active_ms,covered_units,unit_attention,unit_total,unit_scale,resume,access_context,measurement)
 VALUES (?,?,?,?,?,?,?,?,?,?,toDateTime64(?,6,'UTC'),?,?,mapFromArrays(CAST(?,'Array(UInt32)'),CAST(?,'Array(UInt64)')),?,?,?,?,?)`
	return conn.Exec(ctx, q, tenant, event.ContentKind, event.ContentID, event.Version(), event.Subject.Kind(), event.Subject.Key(), event.EventID, event.Revision, m.VisitID, m.SegmentID,
		event.OccurredAt.UTC().Format("2006-01-02 15:04:05.000000"), m.ActiveMS, m.Units, keys, values, m.Total, m.Scale, event.Resume, string(access), string(raw))
}
