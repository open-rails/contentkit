package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
)

// writeChaptersTx preserves chapter identity and the active released revision
// while its title, marker, order or pending schedule changes. Chapters are
// markers in manifest order, never a fixed list of file identities.
func (s *Store) writeChaptersTx(ctx context.Context, tx pgx.Tx, version string, chapters []Segment) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT id,position,title,coalesce(start_page,''),coalesce(start_index,0),coalesce(end_index,0),release_at
 FROM `+s.t.segments+` WHERE tenant_id=$1 AND version_id=$2 ORDER BY position FOR UPDATE`, s.tenant, version)
	if err != nil {
		return false, err
	}
	ids, markers := map[string]bool{}, map[string]string{}
	var previous []Segment
	for rows.Next() {
		var chapter Segment
		if err := rows.Scan(&chapter.ID, &chapter.Position, &chapter.Title, &chapter.StartPage, &chapter.Start, &chapter.End, &chapter.ReleaseAt); err != nil {
			rows.Close()
			return false, err
		}
		ids[chapter.ID] = true
		markers[chapterMarker(chapter.StartPage, chapter.Start, chapter.End)] = chapter.ID
		previous = append(previous, chapter)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	keep := make([]string, len(chapters))
	seen := map[string]bool{}
	for i := range chapters {
		chapter := &chapters[i]
		chapter.Title = strings.TrimSpace(chapter.Title)
		if chapter.ID == "" {
			chapter.ID = markers[chapterMarker(chapter.StartPage, chapter.Start, chapter.End)]
			if chapter.ID == "" {
				chapter.ID = contentref.NewID()
			}
		} else if !ids[chapter.ID] {
			return false, invalid("chapter %s does not belong to this version", chapter.ID)
		}
		if seen[chapter.ID] {
			return false, invalid("chapter %s is repeated", chapter.ID)
		}
		seen[chapter.ID], keep[i] = true, chapter.ID
	}
	if sameChapters(previous, chapters) {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.segments+` WHERE tenant_id=$1 AND version_id=$2 AND NOT(id=ANY($3::text[]))`, s.tenant, version, keep); err != nil {
		return false, err
	}
	// Move existing rows out of the nonnegative authored positions before a
	// reorder. Null markers avoid transient uniqueness conflicts when markers
	// change between retained chapters. The transaction restores valid rows.
	if _, err := tx.Exec(ctx, `UPDATE `+s.t.segments+` SET position=-position-1,start_index=0,end_index=1,start_page=NULL
 WHERE tenant_id=$1 AND version_id=$2`, s.tenant, version); err != nil {
		return false, err
	}
	for _, chapter := range chapters {
		var start, end any = chapter.Start, chapter.End
		var marker any
		if chapter.StartPage != "" {
			start, end, marker = nil, nil, chapter.StartPage
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.segments+` AS current
 (tenant_id,version_id,id,position,title,start_index,end_index,release_at,start_page)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
 ON CONFLICT(tenant_id,version_id,id) DO UPDATE SET position=EXCLUDED.position,title=EXCLUDED.title,
		 start_index=EXCLUDED.start_index,end_index=EXCLUDED.end_index,release_at=EXCLUDED.release_at,start_page=EXCLUDED.start_page,
 released_at=CASE WHEN current.release_at IS DISTINCT FROM EXCLUDED.release_at AND EXCLUDED.release_at>$10
                 THEN NULL ELSE current.released_at END,
 release_id=CASE WHEN current.release_at IS DISTINCT FROM EXCLUDED.release_at AND EXCLUDED.release_at>$10
                THEN NULL ELSE current.release_id END`,
			s.tenant, version, chapter.ID, chapter.Position, strings.TrimSpace(chapter.Title), start, end, chapter.ReleaseAt, marker, s.now()); err != nil {
			return false, err
		}
	}
	return true, nil
}

func chapterMarker(marker string, start, end int) string {
	if marker != "" {
		return marker
	}
	return fmt.Sprintf("\x00%d:%d", start, end)
}

func sameChapters(a, b []Segment) bool {
	if len(a) != len(b) {
		return false
	}
	for i, chapter := range a {
		other := b[i]
		if chapter.ID != other.ID || chapter.Position != other.Position || chapter.Title != other.Title ||
			chapter.StartPage != other.StartPage || chapter.Start != other.Start || chapter.End != other.End ||
			(chapter.ReleaseAt == nil) != (other.ReleaseAt == nil) {
			return false
		}
		if chapter.ReleaseAt != nil && !chapter.ReleaseAt.Equal(*other.ReleaseAt) {
			return false
		}
	}
	return true
}
