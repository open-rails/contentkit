package versions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// PageSpan is a printed page range; a spread is still one reader image.
type PageSpan struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type pageDecision struct {
	Reason     string   `json:"reason"`
	Confidence string   `json:"confidence"`
	Related    []string `json:"related,omitempty"`
}

// selectedPage is one retained source and the automatic or restored choice.
type selectedPage struct {
	ID           string
	Path         string
	SourceName   string
	SourceSHA256 string
	ArchiveSHA   string
	UploadIndex  int
	SourceIndex  int
	Span         *PageSpan
	Included     bool
	Mode         string
	Decision     pageDecision
	Flags        []string
	Analysis     *media.PageAnalysis
}

type pageSelection struct {
	Pages    []selectedPage
	Selected int
}

// PageNotice is a quiet note about an automatic page change: removed pages
// (blank, duplicate, or a single covered by a two-page spread) can be
// restored, and an archive reordered into natural name order can be
// dismissed. Neither ever blocks saving or publishing.
type PageNotice struct {
	ID    string       `json:"id"`
	Kind  string       `json:"kind"` // removed | reordered
	Pages []NoticePage `json:"pages"`
}

// NoticePage is one page a notice concerns. A removed page names why
// (blank, duplicate or spread); a reordered page gives its archive position
// and the position applied.
type NoticePage struct {
	ID         string `json:"id"`
	SourceName string `json:"source_name"`
	Reason     string `json:"reason,omitempty"`
	From       int    `json:"from,omitempty"`
	To         int    `json:"to,omitempty"`
	Chapter    int    `json:"chapter,omitempty"` // a removed page still named as this chapter's start
	PreviewURL string `json:"preview_url,omitempty"`
}

// PageNotices are a version's undismissed notices, for its editors.
type PageNotices struct {
	Notices []PageNotice `json:"notices"`
	Cookie  *http.Cookie `json:"-"`
}

type pageMetadata struct {
	SchemaVersion  int                 `json:"schema_version,omitempty"`
	Name           string              `json:"name,omitempty"`
	SourceName     string              `json:"source_name,omitempty"`
	SourceEntry    string              `json:"source_entry,omitempty"`
	ArchiveSHA256  string              `json:"archive_sha256,omitempty"`
	SourceIndex    int                 `json:"source_index,omitempty"`
	UploadIndex    int                 `json:"upload_index,omitempty"`
	SourceSHA256   string              `json:"source_sha256,omitempty"`
	Span           *PageSpan           `json:"span,omitempty"`
	Mode           string              `json:"mode,omitempty"`
	Flags          []string            `json:"flags,omitempty"`
	Included       *bool               `json:"included,omitempty"`
	SelectionGuard *bool               `json:"selection_guard,omitempty"`
	Decision       *pageDecision       `json:"decision,omitempty"`
	OverrideActor  string              `json:"override_actor,omitempty"`
	OverrideAt     string              `json:"override_at,omitempty"`
	Analysis       *media.PageAnalysis `json:"analysis,omitempty"`
}

func pageMeta(f media.File) pageMetadata {
	var out pageMetadata
	b, err := json.Marshal(f.Meta["page"])
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// writePageMeta preserves fields owned by the native analyzer or a newer writer.
func writePageMeta(f *media.File, value pageMetadata) {
	fields := map[string]any{}
	old, _ := json.Marshal(f.Meta["page"])
	_ = json.Unmarshal(old, &fields)
	if fields == nil {
		fields = map[string]any{}
	}
	b, _ := json.Marshal(value)
	var next map[string]any
	_ = json.Unmarshal(b, &next)
	for _, key := range []string{"mode", "override_actor", "override_at", "selection_guard"} {
		delete(fields, key)
	}
	maps.Copy(fields, next)
	f.Meta = maps.Clone(f.Meta)
	if f.Meta == nil {
		f.Meta = map[string]any{}
	}
	f.Meta["page"] = fields
}

func pageID(k *media.Kind, f media.File) string {
	return k.PageID(f)
}

// pageName is display/path metadata only. Page identity is the original hash.
func pageName(k *media.Kind, f media.File) string {
	if n := pageMeta(f).Name; n != "" {
		return n
	}
	if name := k.NameOf(f.Path); name != "" {
		return name
	}
	return strings.TrimSuffix(path.Base(f.Path), path.Ext(f.Path))
}

func sourcePages(k *media.Kind, m *media.Manifest) []media.File {
	var out []media.File
	for _, f := range m.Files {
		if k.IsPageUpload(f) && !f.Unattached {
			out = append(out, f)
		}
	}
	return out
}

// Explicit segment bounds are still expressed in the existing source slots.
// Until membership is keyed by page identity, QA cannot change those slots.
// Freeze the prior effective inclusion, rather than restoring pages excluded
// before an editor authored segments against the selected sequence.
func guardSegmentedPages(k *media.Kind, m *media.Manifest, segmented bool) {
	for i, f := range m.Files {
		if !k.IsPageUpload(f) || f.Unattached {
			continue
		}
		meta := pageMeta(f)
		if segmented {
			if meta.SelectionGuard == nil {
				included := media.PageIncluded(f)
				meta.SelectionGuard = &included
			}
		} else {
			meta.SelectionGuard = nil
		}
		writePageMeta(&m.Files[i], meta)
	}
}

func selectionOf(k *media.Kind, m *media.Manifest) pageSelection {
	files := sourcePages(k, m)
	out := pageSelection{Pages: make([]selectedPage, 0, len(files))}
	for _, f := range files {
		meta := pageMeta(f)
		name := meta.SourceName
		if name == "" {
			name = path.Base(f.Path)
		}
		mode := meta.Mode
		if mode == "" {
			mode = "auto"
		}
		entry := selectedPage{ID: pageID(k, f), Path: f.Path, SourceName: name, SourceSHA256: pageDigest(f), ArchiveSHA: meta.ArchiveSHA256,
			UploadIndex: meta.UploadIndex, SourceIndex: meta.SourceIndex, Span: meta.Span, Included: true, Mode: mode,
			Decision: pageDecision{Reason: "kept", Confidence: "high"}, Flags: slices.Clone(meta.Flags), Analysis: meta.Analysis}
		if entry.Flags == nil {
			entry.Flags = []string{}
		}
		if a := entry.Analysis; a != nil && a.SourceSHA256 == entry.SourceSHA256 && a.Recipe == media.PageAnalysisRecipe {
			entry.Flags = append(entry.Flags, a.Flags...)
			if a.Status != "complete" {
				entry.Flags = append(entry.Flags, "analysis_"+a.Status)
			}
			if a.Blank == "suspected" {
				entry.Flags = append(entry.Flags, "possibly_blank")
			}
			if a.Blank == "blank" && (a.Confidence == "exact" || a.Confidence == "high") {
				entry.Included = false
				entry.Decision = pageDecision{Reason: "blank", Confidence: a.Confidence}
			}
		} else if meta.SchemaVersion > 0 {
			entry.Flags = append(entry.Flags, "analysis_unavailable")
		}
		if mode == "exclude" {
			entry.Included = false
			entry.Decision = pageDecision{Reason: "manual_exclude", Confidence: "exact"}
		}
		if mode == "include" {
			entry.Included = true
			entry.Decision = pageDecision{Reason: "manual_include", Confidence: "exact"}
		}
		out.Pages = append(out.Pages, entry)
	}
	// A usable joined image wins before duplicate selection, so excluded/blank
	// spreads never hide their individual sources.
	for i := range out.Pages {
		p := &out.Pages[i]
		if !p.Included || p.Mode != "auto" || p.Span == nil || p.Span.From != p.Span.To {
			continue
		}
		for j := range out.Pages {
			join := &out.Pages[j]
			if !join.Included || join.Span == nil || join.Span.From == join.Span.To {
				continue
			}
			if p.Span.From >= join.Span.From && p.Span.To <= join.Span.To {
				p.Included = false
				p.Decision = pageDecision{Reason: "covered_by_joined_page", Confidence: "high", Related: []string{join.ID}}
				break
			}
		}
	}
	seen := map[string]int{}
	// Prefer a surviving joined image when it is byte-identical to an individual.
	order := make([]int, len(out.Pages))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := out.Pages[order[i]], out.Pages[order[j]]
		aj, bj := a.Span != nil && a.Span.To > a.Span.From, b.Span != nil && b.Span.To > b.Span.From
		if aj != bj {
			return aj
		}
		if c := naturalCompare(a.SourceName, b.SourceName); c != 0 {
			return c < 0
		}
		return a.ID < b.ID
	})
	for _, i := range order {
		p := &out.Pages[i]
		if !p.Included {
			continue
		}
		keys := []string{}
		if p.SourceSHA256 != "" {
			keys = append(keys, "bytes:"+p.SourceSHA256)
		}
		if a := p.Analysis; a != nil && a.Recipe == media.PageAnalysisRecipe && a.Status == "complete" && a.SourceSHA256 == p.SourceSHA256 && a.PixelSHA256 != "" {
			keys = append(keys, "pixels:"+a.PixelSHA256)
		}
		duplicate := -1
		for _, key := range keys {
			if prior, ok := seen[key]; ok {
				duplicate = prior
				break
			}
		}
		if duplicate >= 0 && p.Mode == "auto" {
			p.Included = false
			p.Decision = pageDecision{Reason: "duplicate", Confidence: "exact", Related: []string{out.Pages[duplicate].ID}}
			continue
		}
		for _, key := range keys {
			seen[key] = i
		}
	}
	// A joined image rejected as a duplicate cannot keep covering singles.
	for i := range out.Pages {
		p := &out.Pages[i]
		if p.Decision.Reason != "covered_by_joined_page" {
			continue
		}
		covered := slices.ContainsFunc(out.Pages, func(j selectedPage) bool {
			return j.Included && j.Span != nil && j.Span.To > j.Span.From && p.Span != nil && p.Span.From >= j.Span.From && p.Span.To <= j.Span.To
		})
		if !covered {
			p.Included = true
			p.Decision = pageDecision{Reason: "kept", Confidence: "high"}
		}
	}
	for i := range out.Pages {
		a := &out.Pages[i]
		if a.Span == nil || a.Span.From == a.Span.To {
			continue
		}
		for j := i + 1; j < len(out.Pages); j++ {
			b := &out.Pages[j]
			if b.Span != nil && b.Span.To > b.Span.From && a.Span.From <= b.Span.To && b.Span.From <= a.Span.To {
				a.Flags = append(a.Flags, "overlapping_joined_ranges")
				b.Flags = append(b.Flags, "overlapping_joined_ranges")
			}
		}
	}
	// A blank-only submission still publishes; retain one source in
	// its initial reading order rather than creating an unpublishable gallery.
	if len(out.Pages) > 0 && !slices.ContainsFunc(out.Pages, func(p selectedPage) bool { return p.Included }) {
		for i := range out.Pages {
			if out.Pages[i].Mode == "auto" {
				out.Pages[i].Included = true
				out.Pages[i].Decision = pageDecision{Reason: "empty_selection_fallback", Confidence: "possible"}
				break
			}
		}
	}
	for i := range out.Pages {
		p := &out.Pages[i]
		if guard := pageMeta(files[i]).SelectionGuard; guard != nil {
			if p.Included != *guard {
				p.Decision = pageDecision{Reason: "segmented_selection_preserved", Confidence: "exact"}
			}
			p.Included = *guard
			p.Flags = append(p.Flags, "segmented_selection_preserved")
		}
		sort.Strings(p.Flags)
		p.Flags = slices.Compact(p.Flags)
		if p.Included {
			out.Selected++
		}
	}
	return out
}

// pageEditorAccess opens a version's manifest for one of its editors, its
// pages' selection frozen while explicit segments name them.
func (s *Service) pageEditorAccess(ctx context.Context, actor access.Actor, t Target) (*media.Kind, *media.Manifest, contentref.ContentRef, error) {
	v, err := s.one(ctx, t)
	if err != nil {
		return nil, nil, contentref.ContentRef{}, err
	}
	k, err := s.published(v)
	if err != nil {
		return nil, nil, contentref.ContentRef{}, err
	}
	res, err := s.decide(ctx, actor, v.Kind, []Target{t})
	if err != nil {
		return nil, nil, contentref.ContentRef{}, err
	}
	if !res[t].Editor {
		return nil, nil, contentref.ContentRef{}, ErrForbidden
	}
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return nil, nil, contentref.ContentRef{}, err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if err == nil {
		segments, e := s.segments(ctx, s.o.Pool, []string{t.Version})
		if e != nil {
			return nil, nil, contentref.ContentRef{}, e
		}
		m = m.Clone()
		guardSegmentedPages(k, m, legacyPageRanges(segments[t.Version]))
	}
	return k, m, ref, err
}

// noticePageReason names an automatic removal for editors.
var noticePageReason = map[string]string{"blank": "blank", "duplicate": "duplicate", "covered_by_joined_page": "spread"}

// noticesOf derives the version's notices from its current selection; a
// changed selection is a new notice, so a dismissal never hides it.
func noticesOf(k *media.Kind, m *media.Manifest) []PageNotice {
	sel := selectionOf(k, m)
	var out []PageNotice
	var removed []NoticePage
	for _, p := range sel.Pages {
		if reason := noticePageReason[p.Decision.Reason]; reason != "" && !p.Included && p.Mode == "auto" {
			removed = append(removed, NoticePage{ID: p.ID, SourceName: p.SourceName, Reason: reason})
		}
	}
	if len(removed) > 0 {
		out = append(out, PageNotice{ID: "removed-" + revisionOfInput(removed)[:16], Kind: "removed", Pages: removed})
	}
	archives := map[string][]NoticePage{}
	var order []string
	for _, p := range sel.Pages {
		if p.ArchiveSHA == "" || p.UploadIndex == 0 || p.SourceIndex == 0 {
			continue
		}
		if _, ok := archives[p.ArchiveSHA]; !ok {
			order = append(order, p.ArchiveSHA)
		}
		archives[p.ArchiveSHA] = append(archives[p.ArchiveSHA], NoticePage{ID: p.ID, SourceName: p.SourceName, From: p.UploadIndex, To: p.SourceIndex})
	}
	for _, sha := range order {
		pages := archives[sha]
		if !slices.ContainsFunc(pages, func(p NoticePage) bool { return p.From != p.To }) {
			continue
		}
		slices.SortFunc(pages, func(a, b NoticePage) int { return a.To - b.To })
		out = append(out, PageNotice{ID: "reordered-" + revisionOfInput(pages)[:16], Kind: "reordered", Pages: pages})
	}
	return out
}

// PageNotices returns a version's undismissed notices to its editors only.
func (s *Service) PageNotices(ctx context.Context, actor access.Actor, t Target) (PageNotices, error) {
	k, m, ref, err := s.pageEditorAccess(ctx, actor, t)
	if errors.Is(err, media.ErrNotFound) {
		// A new, authorized version has no manifest until its first upload.
		return PageNotices{Notices: []PageNotice{}}, nil
	}
	if err != nil {
		return PageNotices{}, err
	}
	dismissed, err := s.dismissedNotices(ctx, t)
	if err != nil {
		return PageNotices{}, err
	}
	out := PageNotices{Notices: []PageNotice{}}
	for _, n := range noticesOf(k, m) {
		if !dismissed[n.ID] {
			out.Notices = append(out.Notices, n)
		}
	}
	if len(out.Notices) == 0 {
		return out, nil
	}
	byPath, hidden := map[string]string{}, map[string]string{}
	for _, f := range sourcePages(k, m) {
		byPath[f.Path] = pageID(k, f)
	}
	for _, p := range selectionOf(k, m).Pages {
		if !p.Included {
			hidden[p.Path] = p.ID
		}
	}
	segments, err := s.segments(ctx, s.o.Pool, []string{t.Version})
	if err != nil {
		return PageNotices{}, err
	}
	starts := map[string]int{}
	for _, g := range segments[t.Version] {
		if id := hidden[g.StartPage]; id != "" {
			starts[id] = g.Position + 1
		}
	}
	for i := range out.Notices {
		if out.Notices[i].Kind == "removed" {
			for j := range out.Notices[i].Pages {
				out.Notices[i].Pages[j].Chapter = starts[out.Notices[i].Pages[j].ID]
			}
		}
	}
	if s.m.Reader == nil {
		return out, nil
	}
	previews := map[string]string{}
	for offset := 0; ; {
		read, e := s.m.Reader.Read(ctx, ref, actor, media.ReadOptions{Editor: true, Offset: offset, Limit: 200})
		if e != nil {
			return PageNotices{}, e
		}
		if read.Cookie != nil {
			out.Cookie = read.Cookie
		}
		for _, f := range read.Files {
			if id, ok := byPath[f.Path]; ok && f.EditorURL != "" {
				previews[id] = f.EditorURL
			}
			if id, ok := byPath[f.From]; ok && strings.HasPrefix(f.Type, "image/") && previews[id] == "" {
				previews[id] = f.URL
			}
		}
		// Read includes all metadata; only its effective Limit gets URLs.
		if read.Limit <= 0 || read.Limit >= read.Total-offset || len(read.Files) == 0 {
			break
		}
		offset += read.Limit
	}
	for i := range out.Notices {
		for j := range out.Notices[i].Pages {
			out.Notices[i].Pages[j].PreviewURL = previews[out.Notices[i].Pages[j].ID]
		}
	}
	return out, nil
}

// DismissPageNotice hides a current notice; dismissing again is a no-op.
func (s *Service) DismissPageNotice(ctx context.Context, actor access.Actor, t Target, id string) error {
	k, m, _, err := s.pageEditorAccess(ctx, actor, t)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(noticesOf(k, m), func(n PageNotice) bool { return n.ID == id }) {
		return ErrNotFound
	}
	return s.dismissNotice(ctx, t, id)
}

// RestorePageNotice puts a removed notice's pages back where they were (an
// excluded source keeps its place) and clears the notice.
func (s *Service) RestorePageNotice(ctx context.Context, actor access.Actor, t Target, id string) (PageNotices, error) {
	k, current, ref, err := s.pageEditorAccess(ctx, actor, t)
	if err != nil {
		return PageNotices{}, err
	}
	notices := noticesOf(k, current)
	i := slices.IndexFunc(notices, func(n PageNotice) bool { return n.ID == id })
	if i < 0 {
		return PageNotices{}, ErrNotFound
	}
	if notices[i].Kind != "removed" {
		return PageNotices{}, fmt.Errorf("%w: only removed pages can be restored", ErrInvalid)
	}
	restore := map[string]bool{}
	for _, p := range notices[i].Pages {
		restore[p.ID] = true
	}
	unlock, err := s.lockSource(ctx, t)
	if err != nil {
		return PageNotices{}, err
	}
	defer unlock()
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if err != nil {
		return PageNotices{}, err
	}
	segments, err := s.segments(ctx, s.o.Pool, []string{t.Version})
	if err != nil {
		return PageNotices{}, err
	}
	m = m.Clone()
	guardSegmentedPages(k, m, legacyPageRanges(segments[t.Version]))
	for i, f := range m.Files {
		if !k.IsPageUpload(f) || f.Unattached || !restore[pageID(k, f)] {
			continue
		}
		meta := pageMeta(f)
		if meta.SelectionGuard != nil && !*meta.SelectionGuard {
			return PageNotices{}, fmt.Errorf("%w: edit the chapters before restoring pages", ErrInvalid)
		}
		meta.Mode = "include"
		meta.OverrideActor = actor.ID
		meta.OverrideAt = s.o.Now().UTC().Format(time.RFC3339Nano)
		writePageMeta(&m.Files[i], meta)
	}
	persistPageSelection(k, m)
	var ops []media.Op
	for _, f := range m.Files {
		if !k.IsPageUpload(f) || f.Unattached || !restore[pageID(k, f)] {
			continue
		}
		blob := f.Blob
		if f.Staged != "" {
			blob = f.Staged
		}
		ops = append(ops, media.Op{Op: media.OpPut, Path: f.Path, Blob: blob, Edit: f.Edit, Meta: f.Meta, SourceName: pageMeta(f).SourceName})
	}
	if len(ops) > 0 {
		if _, err = s.m.Uploads.CommitChecked(ctx, system, ref, uuid.NewString(), ops, nil); err != nil {
			return PageNotices{}, err
		}
	}
	if err = s.dismissNotice(ctx, t, id); err != nil {
		return PageNotices{}, err
	}
	return s.PageNotices(ctx, actor, t)
}

func (s *Service) dismissedNotices(ctx context.Context, t Target) (map[string]bool, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT notice_id FROM `+pgxIdent(s.o.Schema, "content_page_notice_dismissals")+` WHERE tenant_id=$1 AND version_id=$2`, s.o.Tenant, t.Version)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

func (s *Service) dismissNotice(ctx context.Context, t Target, id string) error {
	_, err := s.o.Pool.Exec(ctx, `INSERT INTO `+pgxIdent(s.o.Schema, "content_page_notice_dismissals")+` (tenant_id,version_id,notice_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, s.o.Tenant, t.Version, id)
	return err
}

func persistPageSelection(k *media.Kind, m *media.Manifest) {
	r := selectionOf(k, m)
	byPath := map[string]selectedPage{}
	for _, p := range r.Pages {
		byPath[p.Path] = p
	}
	for i, f := range m.Files {
		if p, ok := byPath[f.Path]; ok {
			meta := pageMeta(f)
			meta.Included = &p.Included
			meta.Decision = &p.Decision
			writePageMeta(&m.Files[i], meta)
		}
	}
}

// preparePageSelection allocates new upload IDs once. Legacy uploads whose
// metadata only contains native analysis keep their existing reader keys.
func (s *Service) preparePageSelection(ctx context.Context, k *media.Kind, t Target) error {
	unlock, err := s.lockSource(ctx, t)
	if err != nil {
		return err
	}
	defer unlock()
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return err
	}
	segments, err := s.segments(ctx, s.o.Pool, []string{t.Version})
	if err != nil {
		return err
	}
	changed := false
	_, err = s.m.Manifests.EditExisting(ctx, ref, func(m *media.Manifest) error {
		before := revisionOfInput(m.Files)
		guardSegmentedPages(k, m, legacyPageRanges(segments[t.Version]))
		taken := map[string]bool{}
		for _, f := range sourcePages(k, m) {
			if name := pageMeta(f).Name; name != "" {
				taken[name] = true
			}
		}
		for i, f := range m.Files {
			if !k.IsPageUpload(f) || f.Unattached {
				continue
			}
			meta := pageMeta(f)
			if meta.SchemaVersion == 0 || meta.Name != "" {
				continue
			}
			name, span, flags := plannedPageName(path.Base(f.Path), taken)
			meta.Name = name
			meta.Span = span
			meta.Flags = append(meta.Flags, flags...)
			writePageMeta(&m.Files[i], meta)
		}
		persistPageSelection(k, m)
		changed = before != revisionOfInput(m.Files)
		return nil
	})
	if err == nil {
		if err = pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
			if err := s.lockTarget(ctx, tx, t); err != nil {
				return err
			}
			// Not the manifest this edit wrote: a commit may have landed since.
			m, _, err := s.m.Manifests.GetTx(ctx, tx, ref)
			if errors.Is(err, media.ErrNotFound) {
				return nil
			} else if err != nil {
				return err
			}
			return s.recordFacts(ctx, tx, k, t, m)
		}); err != nil {
			return err
		}
		if changed && s.m.Jobs != nil {
			err = s.m.Jobs.EnqueueProcessing(ctx, media.ProcessJob{Ref: ref})
		}
		return err
	}
	return err
}

// plannedPageName keeps the source filename independently of order and hash
// identity. Printed ranges remain metadata for joined-page detection.
func plannedPageName(source string, taken map[string]bool) (string, *PageSpan, []string) {
	stem := media.CleanName(strings.TrimSuffix(path.Base(source), path.Ext(source)))
	name := media.UniquePageName(stem, taken)
	_, from, to, numeric := media.CanonicalPageName(source)
	var span *PageSpan
	if numeric {
		span = &PageSpan{From: from, To: to}
	}
	var flags []string
	if !numeric {
		flags = append(flags, "unusual_source_name")
	}
	if name != stem {
		flags = append(flags, "duplicate_source_name")
	}
	return name, span, flags
}

// revisionOfInput is separate from signed URLs, whose expiry must not create
// optimistic edit conflicts.
func revisionOfInput(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
