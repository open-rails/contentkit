package versions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// A version reaches viewers through publications: frozen snapshots of its
// media copied into immutable items of its kind's published kind, so a
// grant never opens originals, archives or another segment. A pages
// publication has one item per span (a segment's pages, or pages outside
// every segment); a playback publication is one item holding the version's
// audio. A publication is built beside the current one and replaces it once
// complete; an edit that removes or replaces what the current one shows
// withdraws it at once.
const (
	kindPages    = "pages"
	kindPlayback = "playback"
)

// pageCount is a publication row's page count. Publications written before
// pages were always an array hold null for a version without pages.
const pageCount = `CASE jsonb_typeof(source->'pages') WHEN 'array' THEN jsonb_array_length(source->'pages') ELSE 0 END`

// pagesSnapshot is a version's pages as a publication freezes them.
type pagesSnapshot struct {
	Pages []page `json:"pages"`
	Spans []span `json:"spans"`
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

// rendition is a finished file, at the same path in the version's item and
// in its published item.
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

// previewDir is the directory of the Previews preset's uploads.
func previewDir(k *media.Kind) string {
	return strings.TrimSuffix(public(k, k.Publish.Previews).From, "{name}")
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
	if m.Meta[forkMeta] != nil || len(imports(k, m)) > 0 {
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
	if m.Meta[forkMeta] != nil {
		return nil, ErrNotReady
	}
	a := private(k, k.Publish.Playback)
	dir := strings.TrimSuffix(a.To, "{name}/")
	out := []track{}
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached || !inPattern(a.From, f.Path) {
			continue
		}
		if f.Fail() != nil {
			continue // a failed track is left out, not waited for
		}
		if f.Blob == "" || slices.Contains(f.Pending, a.Name) {
			return nil, ErrNotReady
		}
		name := k.NameOf(f.Path)
		t := track{Name: name, Upload: f.Path, Source: f.Key()}
		for _, o := range m.Outputs(f.Path, a.Name) {
			if strings.HasSuffix(o.Path, "/audio.m4a") && o.Blob != "" && !o.Gone && o.Size > 0 {
				t.File = rendition{Path: dir + name + ".m4a", Blob: o.Blob, Type: o.Type, Size: o.Size, Dur: o.Dur}
			}
		}
		if t.File.Blob == "" {
			return nil, ErrNotReady
		}
		out = append(out, t)
	}
	return out, nil
}

// lockSource serializes a version's publication with its forks and imports.
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

// lockVersion locks v's row in tx; ErrNotFound when it is gone.
func (s *Service) lockVersion(ctx context.Context, tx pgx.Tx, v Target) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM `+s.t.versions+` WHERE tenant_id = $1 AND id = $2 AND content_id = $3 FOR UPDATE`,
		s.o.Tenant, v.Version, v.Content).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// pubRow is one publication.
type pubRow struct {
	ID       string
	State    string
	Source   []byte
	Previews *int
	Items    []itemRow
}

type itemRow struct {
	ID string
	span
}

func (s *Service) loadPub(ctx context.Context, q querier, id string) (*pubRow, error) {
	p, err := s.readPub(ctx, q, "p.id = $2", id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errRetired
	}
	return p, err
}

// current is v's current publication of kind, nil without one.
func (s *Service) current(ctx context.Context, q querier, v Target, kind string) (*pubRow, error) {
	p, err := s.readPub(ctx, q, "p.version_id = $2 AND p.kind = $3 AND p.state = 'current'", v.Version, kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// The publication and its items share one snapshot even during withdrawal.
func (s *Service) readPub(ctx context.Context, q querier, where string, args ...any) (*pubRow, error) {
	p := &pubRow{}
	var items []byte
	err := q.QueryRow(ctx, `SELECT p.id, p.state, p.source, p.previews,
 (SELECT coalesce(json_agg(json_build_object('id', i.id, 'segment', i.segment,
 'start', i.start_index, 'end', i.end_index) ORDER BY i.start_index), '[]'::json)
 FROM `+s.t.pubItems+` i WHERE i.tenant_id = p.tenant_id AND i.publication_id = p.id)
 FROM `+s.t.publications+` p WHERE p.tenant_id = $1 AND `+where,
		append([]any{s.o.Tenant}, args...)...).Scan(&p.ID, &p.State, &p.Source, &p.Previews, &items)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(items, &p.Items); err != nil {
		return nil, err
	}
	return p, nil
}

// publishedRef is the ref of a published item of k.
func (s *Service) publishedRef(k *media.Kind, id string) contentref.ContentRef {
	return contentref.New(k.NS(), k.Name+media.PublishedSuffix, id)
}

// retireTx drops publications: their rows, and their items through media's
// queued deletion.
func (s *Service) retireTx(ctx context.Context, tx pgx.Tx, k *media.Kind, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT i.id FROM `+s.t.pubItems+` i WHERE i.tenant_id = $1 AND i.publication_id = ANY($2)
UNION ALL SELECT p.id FROM `+s.t.publications+` p WHERE p.tenant_id = $1 AND p.id = ANY($2) AND p.kind = 'playback'`, s.o.Tenant, ids)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var del []media.Deletion
	for _, id := range items {
		del = append(del, media.Deletion{Ref: s.publishedRef(k, id)})
	}
	if err := s.m.Jobs.DeleteItemsTx(ctx, tx, del...); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM `+s.t.publications+` WHERE tenant_id = $1 AND id = ANY($2)`, s.o.Tenant, ids)
	return err
}

// prepareTx records v's publication of kind showing source: the current one
// when it already does (true), else the one being built, replacing a build
// of another source.
func (s *Service) prepareTx(ctx context.Context, tx pgx.Tx, k *media.Kind, v Target, kind string, source any, items []span) (string, bool, error) {
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
	for _, sp := range items {
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.pubItems+` (tenant_id, id, publication_id, segment, start_index, end_index)
VALUES ($1, $2, $3, $4, $5, $6)`, s.o.Tenant, contentref.NewID(), id, sp.Segment, sp.Start, sp.End); err != nil {
			return "", false, err
		}
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

// snapshotPages is v's pages snapshot now, with its segments' spans.
func (s *Service) snapshotPages(ctx context.Context, q querier, k *media.Kind, v Target) (pagesSnapshot, error) {
	m, err := s.manifest(ctx, k, v)
	if err != nil {
		return pagesSnapshot{}, err
	}
	pages, err := pagesOf(k, m)
	if err != nil {
		return pagesSnapshot{}, err
	}
	segs, err := s.segments(ctx, q, []string{v.Version})
	if err != nil {
		return pagesSnapshot{}, err
	}
	resolved, err := resolveChapterStarts(segs[v.Version], pages)
	if err != nil {
		return pagesSnapshot{}, err
	}
	return pagesSnapshot{Pages: pages, Spans: spans(resolved, len(pages))}, nil
}

// manifest is v's item's manifest; an empty one before its first upload.
func (s *Service) manifest(ctx context.Context, k *media.Kind, v Target) (*media.Manifest, error) {
	ref, err := s.ref(k.Name, v)
	if err != nil {
		return nil, err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return &media.Manifest{V: media.ManifestVersion}, nil
	}
	return m, err
}

// publishPages builds v's pages publication and makes it current; false
// while its items are still being placed.
func (s *Service) publishPages(ctx context.Context, k *media.Kind, v Target) (bool, error) {
	id, current, err := s.underSource(ctx, v, func(tx pgx.Tx) (string, bool, error) {
		snap, err := s.snapshotPages(ctx, tx, k, v)
		if err != nil {
			return "", false, err
		}
		return s.prepareTx(ctx, tx, k, v, kindPages, snap, snap.Spans)
	})
	if err != nil || current {
		return current, err
	}
	p, err := s.loadPub(ctx, s.o.Pool, id)
	if err != nil {
		return false, err
	}
	var snap pagesSnapshot
	if err := json.Unmarshal(p.Source, &snap); err != nil {
		return false, err
	}
	src, err := s.ref(k.Name, v)
	if err != nil {
		return false, err
	}
	complete := true
	for _, it := range p.Items {
		var files [][]rendition
		for _, pg := range snap.Pages[it.Start:it.End] {
			files = append(files, pg.Files)
		}
		done, err := s.fill(ctx, src, s.publishedRef(k, it.ID), files)
		if err != nil {
			return false, err
		}
		complete = complete && done
	}
	if !complete {
		return false, nil
	}
	_, current, err = s.underSource(ctx, v, func(tx pgx.Tx) (string, bool, error) {
		now, err := s.snapshotPages(ctx, tx, k, v)
		if err != nil {
			return "", false, err
		}
		if !sameJSON(now, snap) {
			return "", false, s.retireTx(ctx, tx, k, []string{id})
		}
		ok, err := s.promoteTx(ctx, tx, k, v, kindPages, id)
		return "", ok, err
	})
	return current, err
}

// publishPlayback builds v's playback publication and makes it current.
func (s *Service) publishPlayback(ctx context.Context, k *media.Kind, v Target) (bool, error) {
	id, current, err := s.underSource(ctx, v, func(tx pgx.Tx) (string, bool, error) {
		m, err := s.manifest(ctx, k, v)
		if err != nil {
			return "", false, err
		}
		tracks, err := tracksOf(k, m)
		if err != nil {
			return "", false, err
		}
		return s.prepareTx(ctx, tx, k, v, kindPlayback, playbackSnapshot{Tracks: tracks}, nil)
	})
	if err != nil || current {
		return current, err
	}
	p, err := s.loadPub(ctx, s.o.Pool, id)
	if err != nil {
		return false, err
	}
	var snap playbackSnapshot
	if err := json.Unmarshal(p.Source, &snap); err != nil {
		return false, err
	}
	src, err := s.ref(k.Name, v)
	if err != nil {
		return false, err
	}
	complete := true
	if len(snap.Tracks) > 0 {
		files := make([][]rendition, len(snap.Tracks))
		for i, t := range snap.Tracks {
			files[i] = []rendition{t.File}
		}
		if complete, err = s.fill(ctx, src, s.publishedRef(k, id), files); err != nil {
			return false, err
		}
	}
	if !complete {
		return false, nil
	}
	_, current, err = s.underSource(ctx, v, func(tx pgx.Tx) (string, bool, error) {
		m, err := s.manifest(ctx, k, v)
		if err != nil {
			return "", false, err
		}
		tracks, err := tracksOf(k, m)
		if err != nil {
			return "", false, err
		}
		if !sameJSON(playbackSnapshot{Tracks: tracks}, snap) {
			return "", false, s.retireTx(ctx, tx, k, []string{id})
		}
		ok, err := s.promoteTx(ctx, tx, k, v, kindPlayback, id)
		return "", ok, err
	})
	return current, err
}

// underSource runs fn in a transaction holding v's source lock and row.
func (s *Service) underSource(ctx context.Context, v Target, fn func(pgx.Tx) (string, bool, error)) (id string, ok bool, err error) {
	unlock, err := s.lockSource(ctx, v)
	if err != nil {
		return "", false, err
	}
	defer unlock()
	err = pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if err := s.lockVersion(ctx, tx, v); err != nil {
			return err
		}
		id, ok, err = fn(tx)
		return err
	})
	return id, ok, err
}

func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// fill brings a published item to files, each row at its index in its
// upload group, and reports it complete: every file there, at its position,
// with its bytes. Missing files are copied from src's outputs (OpCopy, a
// server-side copy: nothing is staged, so the worker has nothing to place)
// and ordered in one commit; a complete item takes none. A file already
// there with other bytes retires nothing here; the item is left to its
// publication's retirement.
func (s *Service) fill(ctx context.Context, src, dst contentref.ContentRef, files [][]rendition) (bool, error) {
	m, _, err := s.m.Manifests.Get(ctx, dst)
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		return false, err
	}
	if complete(m, files) {
		return true, nil
	}
	from, _, err := s.m.Manifests.Get(ctx, src)
	if errors.Is(err, media.ErrNotFound) {
		return false, fmt.Errorf("%w: %s is gone", errRetired, src)
	} else if err != nil {
		return false, err
	}
	outputs := map[string]string{} // blob -> its output's path in src
	for _, f := range from.Files {
		if !f.IsUpload() && f.Blob != "" && !f.Gone {
			outputs[f.Blob] = f.Path
		}
	}
	var copies, order []media.Op
	for i, row := range files {
		for _, r := range row {
			at := i
			order = append(order, media.Op{Op: media.OpMove, Path: r.Path, Index: &at})
			if m != nil {
				if f, ok := m.Get(r.Path); ok {
					if !sameBlob(f.Blob, r.Blob) {
						return false, fmt.Errorf("%w: %s holds other bytes at %s", errRetired, dst, r.Path)
					}
					continue
				}
			}
			path, ok := outputs[r.Blob]
			if !ok {
				return false, fmt.Errorf("%w: %s no longer holds %s", errRetired, src, r.Blob)
			}
			copies = append(copies, media.Op{Op: media.OpCopy, From: &media.CopyFrom{ID: src.ContentID, Version: src.Version(), Path: path}, To: r.Path})
		}
	}
	for batch := range slices.Chunk(append(copies, order...), 1000) {
		if m, err = s.m.Uploads.Commit(ctx, system, dst, uuid.NewString(), batch); err != nil {
			return false, err
		}
	}
	return complete(m, files), nil
}

// complete reports an item holding exactly files, placed, in order.
func complete(m *media.Manifest, files [][]rendition) bool {
	if m == nil {
		return false
	}
	n := 0
	for _, f := range m.Files {
		if f.IsUpload() {
			n++
		}
	}
	want := 0
	last := map[string]int{}
	for _, row := range files {
		for _, r := range row {
			want++
			i := m.Find(r.Path)
			if i < 0 {
				return false
			}
			f := m.Files[i]
			group := r.Path[:strings.LastIndexByte(r.Path, '/')+1]
			if prev, ok := last[group]; ok && i <= prev {
				return false
			}
			last[group] = i
			if f.Staged != "" || len(f.Pending) > 0 || f.Unattached || f.Gone || f.Fail() != nil || f.Edit != nil ||
				!sameBlob(f.Blob, r.Blob) || f.Size != r.Size {
				return false
			}
		}
	}
	return n == want
}

// sameBlob reports two allocations of the same bytes.
func sameBlob(a, b string) bool {
	da, okA := layout.BlobDigest(a)
	db, okB := layout.BlobDigest(b)
	return a == b || okA && okB && bytes.Equal(da, db)
}

// withdrawStaleTx withdraws v's publications showing pages or audio m no
// longer has, as uploaded: a removal, a replacement or an edit. It reports
// whether the pages publication went, whose previews go with it.
func (s *Service) withdrawStaleTx(ctx context.Context, tx pgx.Tx, k *media.Kind, v Target, m *media.Manifest) (bool, error) {
	have := map[string]string{}
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
		return x, r.Scan(&x.id, &x.kind, &x.source)
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
			}
			pages = pages || gone
		} else {
			var snap playbackSnapshot
			if err := json.Unmarshal(p.source, &snap); err != nil {
				return false, err
			}
			for _, t := range snap.Tracks {
				gone = gone || have[t.Upload] != t.Source
			}
		}
		if gone {
			stale = append(stale, p.id)
		}
	}
	return pages, s.retireTx(ctx, tx, k, stale)
}
