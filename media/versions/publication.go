package versions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// A publication is an immutable reader snapshot of completed file delivery.
// The item publisher reconciles storage before replacing this snapshot.
const (
	kindPages    = "pages"
	kindPlayback = "playback"
)

// pageCount is a publication row's page count. Publications written before
// pages were always an array hold null for a version without pages.
const pageCount = `CASE jsonb_typeof(source->'pages') WHEN 'array' THEN jsonb_array_length(source->'pages') ELSE 0 END`

// pagesSnapshot is a version's pages as a publication freezes them.
type pagesSnapshot struct {
	Pages    []page    `json:"pages"`
	Segments []segment `json:"segments,omitempty"`
}

type page struct {
	Name string `json:"name,omitempty"`
	// The stored Source already includes the original blob digest. Keep this
	// transient value out of snapshot JSON so existing publications stay valid.
	SourceHash string      `json:"-"`
	Span       *PageSpan   `json:"span,omitempty"`
	Path       string      `json:"path"`   // the page upload
	Source     string      `json:"source"` // its blob and edit (File.Key)
	Files      []rendition `json:"files"`  // Publish.Renditions, in order
}

// rendition identifies an existing immutable file in the version's item.
type rendition struct {
	Preset string  `json:"preset,omitempty"`
	Path   string  `json:"path"`
	Blob   string  `json:"blob"`
	Type   string  `json:"type"`
	Size   int64   `json:"size"`
	W      int     `json:"w,omitempty"`
	H      int     `json:"h,omitempty"`
	Dur    float64 `json:"dur,omitempty"`
}

// playbackSnapshot is a version's audio as a publication freezes it.
type playbackSnapshot struct {
	Tracks []track `json:"tracks"`
}

type track struct {
	Name   string    `json:"name"`   // the audio upload's {name}
	Upload string    `json:"upload"` // its path
	Source string    `json:"source"` // its blob and edit
	File   rendition `json:"file"`   // its M4A, at {dir}{name}.m4a in the playback item
}

// errFailed is a version whose pages or audio failed to process: nothing
// publishes until an edit fixes them.
var errFailed = errors.New("versions: media failed to process")

func private(k *media.Kind, name string) *media.Private {
	for i := range k.Private {
		if k.Private[i].Name == name {
			return &k.Private[i]
		}
	}
	return nil
}

func public(k *media.Kind, name string) *media.Public {
	for i := range k.Public {
		if k.Public[i].Name == name {
			return &k.Public[i]
		}
	}
	return nil
}

// imports are the attached archives Publish.Import has yet to expand.
func imports(k *media.Kind, m *media.Manifest) []media.File {
	var out []media.File
	for _, f := range m.Files {
		if k.Publish.Import != "" && f.IsUpload() && !f.Unattached && f.Fail() == nil && (f.Meta["archive_completed_sha256"] == nil || f.Meta["archive_completed_sha256"] != pageDigest(f)) && inPattern(k.Publish.Import, f.Path) {
			out = append(out, f)
		}
	}
	return out
}

// pagesOf is m's pages ready to publish.
func pagesOf(k *media.Kind, m *media.Manifest) ([]page, error) {
	if len(imports(k, m)) > 0 {
		return nil, ErrNotReady
	}
	out := []page{} // never nil: snapshots store pages as an array, which SQL counts
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached || !inPattern(k.Publish.Pages, f.Path) {
			continue
		}
		if !media.PageIncluded(f) {
			continue
		}
		if f.Fail() != nil {
			return nil, fmt.Errorf("%w: %s", errFailed, f.Path)
		}
		if f.Blob == "" {
			return nil, ErrNotReady
		}
		if pageMeta(f).SchemaVersion > 0 && strings.HasPrefix(f.Type, "image/") && !f.Gone && f.Fail() == nil && media.PageAnalysisFits(f.Meta) && k.PageAnalysisCapacity(m) > 0 && !f.HasPageAnalysis(media.PageAnalysisRecipe) {
			return nil, ErrNotReady
		}
		p := page{Path: f.Path, Source: f.Key(), SourceHash: k.PageID(f), Name: pageName(k, f), Span: pageMeta(f).Span}
		for _, name := range k.Publish.Renditions {
			pre := private(k, name)
			outs := m.Outputs(f.Path, name)
			if slices.Contains(f.Pending, name) || len(outs) != 1 {
				return nil, ErrNotReady
			}
			o := outs[0]
			if o.Path != k.OutputPath(pre, f.Path) || o.Blob == "" || o.Gone || o.Type != "image/webp" || o.Size <= 0 || o.W <= 0 || o.H <= 0 {
				return nil, fmt.Errorf("%w: %s %s", ErrNotReady, f.Path, name)
			}
			p.Files = append(p.Files, rendition{Preset: name, Path: o.Path, Blob: o.Blob, Type: o.Type, Size: o.Size, W: o.W, H: o.H})
		}
		out = append(out, p)
	}
	return out, nil
}

// tracksOf is m's audio ready to publish.
func tracksOf(k *media.Kind, m *media.Manifest) ([]track, error) {
	out := []track{}
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached || !inPattern(k.Publish.Playback, f.Path) {
			continue
		}
		if f.Fail() != nil && !k.AudioUpload(f) {
			continue // a failed track is left out, not waited for
		}
		if f.Blob == "" {
			return nil, ErrNotReady
		}
		name := k.NameOf(f.Path)
		t := track{Name: name, Upload: f.Path, Source: f.Key()}
		if k.AudioUpload(f) {
			t.File = rendition{Path: f.Path, Blob: f.Blob, Type: f.Type, Size: f.Size, Dur: f.Dur}
		}
		// Retained conversions stay usable; no new converted outputs are made.
		for _, o := range m.Files {
			if o.From == f.Path && strings.HasSuffix(o.Path, "/audio.m4a") && strings.HasPrefix(o.Type, "audio/") && o.Blob != "" && !o.Gone && o.Size > 0 {
				t.File = rendition{Path: o.Path, Blob: o.Blob, Type: o.Type, Size: o.Size, Dur: o.Dur}
			}
		}
		if t.File.Blob == "" {
			return nil, ErrNotReady
		}
		out = append(out, t)
	}
	return out, nil
}

// lockSource serializes a version's publication with source edits and imports.
func (s *Service) lockSource(ctx context.Context, v Target) (func(), error) {
	return LockSource(ctx, s.o.Pool, s.o.Tenant, v.Version)
}

// LockSource serializes source selection with catalog segment writes. Acquire
// it before a SQL transaction/row lock; manifest commits may notify the catalog.
func LockSource(ctx context.Context, pool *pgxpool.Pool, tenant, version string) (func(), error) {
	if tenant == "" || version == "" {
		return nil, ErrInvalid
	}
	return LockSources(ctx, pool, tenant, []string{version})
}

// pubRow is one publication.
type pubRow struct {
	ID     string
	State  string
	Source []byte
}

func (s *Service) current(ctx context.Context, q querier, v Target, kind string) (*pubRow, error) {
	p, err := s.readPub(ctx, q, "p.version_id = $2 AND p.kind = $3 AND p.state = 'current'", v.Version, kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *Service) readPub(ctx context.Context, q querier, where string, args ...any) (*pubRow, error) {
	p := &pubRow{}
	err := q.QueryRow(ctx, `SELECT p.id,p.state,p.source FROM `+s.t.publications+` p WHERE p.tenant_id=$1 AND `+where, append([]any{s.o.Tenant}, args...)...).Scan(&p.ID, &p.State, &p.Source)
	return p, err
}

// Retiring metadata never touches a source file or creates a storage job.
func (s *Service) retireTx(ctx context.Context, tx pgx.Tx, _ *media.Kind, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `DELETE FROM `+s.t.publications+` WHERE tenant_id=$1 AND id=ANY($2)`, s.o.Tenant, ids)
	return err
}

// prepareTx records v's publication of kind showing source: the current one
// when it already does (true), else the one being built, replacing a build
// of another source.
func (s *Service) prepareTx(ctx context.Context, tx pgx.Tx, k *media.Kind, v Target, kind string, source any) (string, bool, error) {
	body, err := json.Marshal(source)
	if err != nil {
		return "", false, err
	}
	rows, err := tx.Query(ctx, `SELECT id, state, source = $4::jsonb FROM `+s.t.publications+`
WHERE tenant_id = $1 AND version_id = $2 AND kind = $3`, s.o.Tenant, v.Version, kind, body)
	if err != nil {
		return "", false, err
	}
	var stale []string
	for rows.Next() {
		var id, state string
		var same bool
		if err := rows.Scan(&id, &state, &same); err != nil {
			rows.Close()
			return "", false, err
		}
		if same {
			rows.Close()
			return id, state == "current", nil
		}
		if state == "building" {
			stale = append(stale, id)
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if err := s.retireTx(ctx, tx, k, stale); err != nil {
		return "", false, err
	}
	id := contentref.NewID()
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.publications+` (tenant_id, id, version_id, kind, source) VALUES ($1, $2, $3, $4, $5)`,
		s.o.Tenant, id, v.Version, kind, body); err != nil {
		return "", false, err
	}
	return id, false, nil
}

// promoteTx makes build id current, retiring the one it replaces; false when
// the build is gone (an edit retired it meanwhile). Promoting the current one
// changes nothing, so publishers that raced to build it agree.
func (s *Service) promoteTx(ctx context.Context, tx pgx.Tx, k *media.Kind, v Target, kind, id string) (bool, error) {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM `+s.t.publications+` WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, s.o.Tenant, id).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil || state == "current" {
		return err == nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM `+s.t.publications+` WHERE tenant_id = $1 AND version_id = $2 AND kind = $3 AND state = 'current'`,
		s.o.Tenant, v.Version, kind)
	if err != nil {
		return false, err
	}
	old, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, err
	}
	// A republication with more pages than the one it replaces added pages
	// (new chapters in the same edition): cards mark the work updated.
	if kind == kindPages && len(old) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` SET pages_added_at = now() WHERE tenant_id = $1 AND id = $2
 AND (SELECT `+pageCount+` FROM `+s.t.publications+` WHERE tenant_id = $1 AND id = $3) >
     (SELECT max(`+pageCount+`) FROM `+s.t.publications+` WHERE tenant_id = $1 AND id = ANY($4))`,
			s.o.Tenant, v.Version, id, old); err != nil {
			return false, err
		}
	}
	if err := s.retireTx(ctx, tx, k, old); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE `+s.t.publications+` SET state = 'current', ready_at = now() WHERE tenant_id = $1 AND id = $2`, s.o.Tenant, id)
	return err == nil, err
}

// sameBlob reports two allocations of the same bytes.
func sameBlob(a, b string) bool {
	da, okA := layout.BlobDigest(a)
	db, okB := layout.BlobDigest(b)
	return a == b || okA && okB && bytes.Equal(da, db)
}

// withdrawStaleTx withdraws v's publications showing pages or audio m no
// longer has, as uploaded: a removal, a replacement or an edit. It reports
// whether the pages snapshot was withdrawn.
func (s *Service) withdrawStaleTx(ctx context.Context, tx pgx.Tx, k *media.Kind, v Target, m *media.Manifest) (bool, error) {
	have := map[string]string{}
	blobs := map[string]bool{}
	for _, blob := range m.Blobs() {
		blobs[blob] = true
	}
	for _, f := range m.Files {
		if f.IsUpload() && !f.Unattached {
			have[f.Path] = f.Key()
		}
	}
	rows, err := tx.Query(ctx, `SELECT id, kind, source FROM `+s.t.publications+` WHERE tenant_id = $1 AND version_id = $2`, s.o.Tenant, v.Version)
	if err != nil {
		return false, err
	}
	type row struct {
		id, kind string
		source   []byte
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.kind, &x.source)
		return x, err
	})
	if err != nil {
		return false, err
	}
	var stale []string
	pages := false
	for _, p := range all {
		gone := false
		if p.kind == kindPages {
			var snap pagesSnapshot
			if err := json.Unmarshal(p.source, &snap); err != nil {
				return false, err
			}
			for _, pg := range snap.Pages {
				gone = gone || have[pg.Path] != pg.Source
				for _, file := range pg.Files {
					gone = gone || !blobs[file.Blob]
				}
			}
			pages = pages || gone
		} else {
			var snap playbackSnapshot
			if err := json.Unmarshal(p.source, &snap); err != nil {
				return false, err
			}
			for _, t := range snap.Tracks {
				gone = gone || have[t.Upload] != t.Source || !blobs[t.File.Blob]
			}
		}
		if gone {
			stale = append(stale, p.id)
		}
	}
	return pages, s.retireTx(ctx, tx, k, stale)
}
