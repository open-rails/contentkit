package signal

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// LegacyReading restores an existing bookmark/progress fact. It carries no
// attendance, coverage or quality evidence and never contributes to readership,
// completion statistics, popularity or recommendation seeds.
type LegacyReading struct {
	ContentRef
	Subject     Subject
	FirstReadAt time.Time
	LastReadAt  time.Time
	Progress    uint32
	ProgressMax uint32
	Completed   bool
	Resume      string
	Revision    uint64
}

func (st *Store) ImportLegacyReading(ctx context.Context, tenant string, in []LegacyReading) error {
	if len(in) > MaxSignalsPerBatch {
		return &LimitError{Field: "legacy reading records", Limit: MaxSignalsPerBatch, Got: len(in)}
	}
	for _, row := range in {
		if err := checkRef(tenant, row.ContentRef); err != nil {
			return err
		}
		if err := row.Subject.Validate(); err != nil {
			return err
		}
		if row.FirstReadAt.IsZero() {
			row.FirstReadAt = row.LastReadAt
		}
		if row.LastReadAt.IsZero() || row.LastReadAt.Unix() < 0 || row.LastReadAt.Unix() > (1<<32)-1 || row.FirstReadAt.Unix() < 0 || row.FirstReadAt.After(row.LastReadAt) || row.Progress > row.ProgressMax {
			return fmt.Errorf("signal: invalid legacy reading time or progress")
		}
		if err := checkIdentifiers("Subject", row.Subject.Key()); err != nil {
			return err
		}
		if err := checkLen("Resume", row.Resume, MaxResumeBytes); err != nil {
			return err
		}
	}
	rows := make([]LegacyReading, 0, len(in)*2)
	for _, row := range in {
		rows = append(rows, row)
		if row.Version() != "" {
			work := row
			work.ContentRef = row.ContentRef.Content()
			rows = append(rows, work)
		}
	}
	const partSize = 64
	for start := 0; start < len(rows); start += partSize {
		end := min(start+partSize, len(rows))
		parts := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*14)
		for _, row := range rows[start:end] {
			if row.FirstReadAt.IsZero() {
				row.FirstReadAt = row.LastReadAt
			}
			parts = append(parts, `SELECT ? AS tenant,? AS subject_kind,? AS subject,? AS content_kind,? AS content_id,? AS content_version_id,
 toDateTime(?,'UTC') AS first_seen_at,toDateTime(?,'UTC') AS last_seen_at,toDateTime64(?,6,'UTC') AS last_precise,
 toUInt32(?) AS progress,toUInt32(?) AS progress_max,? AS completed,? AS resume,toUInt64(?) AS revision`)
			args = append(args, tenant, row.Subject.Kind(), row.Subject.Key(), row.ContentKind, row.ContentID, row.Version(),
				row.FirstReadAt.UTC(), row.LastReadAt.UTC(), row.LastReadAt.UTC().Format("2006-01-02 15:04:05.000000"), row.Progress, row.ProgressMax, row.Completed, row.Resume, row.Revision)
		}
		q := fmt.Sprintf(`INSERT INTO %s.legacy_reading_state
 (tenant,subject_kind,subject,content_kind,content_id,content_version_id,first_seen_at,last_seen_at,progress,progress_max,completed,resume,version)
 SELECT tenant,subject_kind,subject,content_kind,content_id,content_version_id,first_seen_at,last_seen_at,progress,progress_max,completed,resume,
 bitOr(bitShiftLeft(toUInt256(toUnixTimestamp64Micro(last_precise)),128),bitOr(bitShiftLeft(toUInt256(revision),64),toUInt256(cityHash64(first_seen_at,last_seen_at,progress,progress_max,completed,resume))))
 FROM (%s) WHERE %s`, st.db, strings.Join(parts, " UNION ALL "), st.notErased())
		if err := st.conn.Exec(ctx, q, args...); err != nil {
			return fmt.Errorf("signal: import legacy reading state: %w", err)
		}
	}
	return nil
}
