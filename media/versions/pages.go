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

type PageDecision struct {
	Reason     string   `json:"reason"`
	Confidence string   `json:"confidence"`
	Related    []string `json:"related,omitempty"`
}

// PageReviewEntry includes retained sources, including those not in the reader.
// Source URLs are signed and returned only to an editor of this version.
type PageReviewEntry struct {
	ID            string              `json:"id"`
	Path          string              `json:"path"`
	SourceName    string              `json:"source_name"`
	SourceEntry   string              `json:"source_entry,omitempty"`
	ArchiveSHA256 string              `json:"archive_sha256,omitempty"`
	SourceSHA256  string              `json:"source_sha256"`
	Span          *PageSpan           `json:"span,omitempty"`
	Included      bool                `json:"included"`
	Mode          string              `json:"mode"`
	Decision      PageDecision        `json:"decision"`
	Flags         []string            `json:"flags"`
	Analysis      *media.PageAnalysis `json:"analysis,omitempty"`
	PreviewURL    string              `json:"preview_url,omitempty"`
	SourceURL     string              `json:"source_url,omitempty"`
}

type PageReview struct {
	Revision string            `json:"revision"`
	Pages    []PageReviewEntry `json:"pages"`
	Selected int               `json:"selected"`
	Flagged  int               `json:"flagged"`
	Cookie   *http.Cookie      `json:"-"`
}

type PageReviewSummary struct {
	Sources  int `json:"sources"`
	Selected int `json:"selected"`
	Excluded int `json:"excluded"`
	Flagged  int `json:"flagged"`
}

type PageReviewChange struct {
	ID   string `json:"id"`
	Mode string `json:"mode"` // auto | include | exclude
}

// PageReviewPatch is an optimistic edit. Order, when present, names every
// retained source exactly once; moving a page never changes its ID.
type PageReviewPatch struct {
	Revision string             `json:"revision"`
	Pages    []PageReviewChange `json:"pages,omitempty"`
	Order    []string           `json:"order,omitempty"`
}

var ErrPageReviewConflict = errors.New("versions: page review changed; reload before saving")

type pageMetadata struct {
	SchemaVersion  int                 `json:"schema_version,omitempty"`
	Name           string              `json:"name,omitempty"`
	SourceName     string              `json:"source_name,omitempty"`
	SourceEntry    string              `json:"source_entry,omitempty"`
	ArchiveSHA256  string              `json:"archive_sha256,omitempty"`
	SourceIndex    int                 `json:"source_index,omitempty"`
	SourceSHA256   string              `json:"source_sha256,omitempty"`
	Span           *PageSpan           `json:"span,omitempty"`
	Mode           string              `json:"mode,omitempty"`
	Flags          []string            `json:"flags,omitempty"`
	Included       *bool               `json:"included,omitempty"`
	SelectionGuard *bool               `json:"selection_guard,omitempty"`
	Decision       *PageDecision       `json:"decision,omitempty"`
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

func reviewOf(k *media.Kind, m *media.Manifest) PageReview {
	files := sourcePages(k, m)
	out := PageReview{Pages: make([]PageReviewEntry, 0, len(files))}
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
		entry := PageReviewEntry{ID: pageID(k, f), Path: f.Path, SourceName: name, SourceEntry: meta.SourceEntry,
			ArchiveSHA256: meta.ArchiveSHA256, SourceSHA256: pageDigest(f), Span: meta.Span, Included: true, Mode: mode,
			Decision: PageDecision{Reason: "kept", Confidence: "high"}, Flags: slices.Clone(meta.Flags), Analysis: meta.Analysis}
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
				entry.Decision = PageDecision{Reason: "blank", Confidence: a.Confidence}
			}
		} else if meta.SchemaVersion > 0 {
			entry.Flags = append(entry.Flags, "analysis_unavailable")
		}
		if mode == "exclude" {
			entry.Included = false
			entry.Decision = PageDecision{Reason: "manual_exclude", Confidence: "exact"}
		}
		if mode == "include" {
			entry.Included = true
			entry.Decision = PageDecision{Reason: "manual_include", Confidence: "exact"}
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
				p.Decision = PageDecision{Reason: "covered_by_joined_page", Confidence: "high", Related: []string{join.ID}}
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
			p.Decision = PageDecision{Reason: "duplicate", Confidence: "exact", Related: []string{out.Pages[duplicate].ID}}
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
		covered := slices.ContainsFunc(out.Pages, func(j PageReviewEntry) bool {
			return j.Included && j.Span != nil && j.Span.To > j.Span.From && p.Span != nil && p.Span.From >= j.Span.From && p.Span.To <= j.Span.To
		})
		if !covered {
			p.Included = true
			p.Decision = PageDecision{Reason: "kept", Confidence: "high"}
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
	// A blank-only submission still completes for review; retain one source in
	// its initial reading order rather than creating an unpublishable gallery.
	if len(out.Pages) > 0 && !slices.ContainsFunc(out.Pages, func(p PageReviewEntry) bool { return p.Included }) {
		for i := range out.Pages {
			if out.Pages[i].Mode == "auto" {
				out.Pages[i].Included = true
				out.Pages[i].Decision = PageDecision{Reason: "empty_selection_fallback", Confidence: "possible"}
				out.Pages[i].Flags = append(out.Pages[i].Flags, "review_empty_selection")
				break
			}
		}
	}
	for i := range out.Pages {
		p := &out.Pages[i]
		if guard := pageMeta(files[i]).SelectionGuard; guard != nil {
			if p.Included != *guard {
				p.Decision = PageDecision{Reason: "segmented_selection_preserved", Confidence: "exact"}
			}
			p.Included = *guard
			p.Flags = append(p.Flags, "segmented_selection_preserved")
		}
		sort.Strings(p.Flags)
		p.Flags = slices.Compact(p.Flags)
		if p.Included {
			out.Selected++
		}
		if len(p.Flags) > 0 || !p.Included {
			out.Flagged++
		}
	}
	b, _ := json.Marshal(out.Pages)
	sum := sha256.Sum256(b)
	out.Revision = hex.EncodeToString(sum[:])
	return out
}

func (r PageReview) Summary() *PageReviewSummary {
	return &PageReviewSummary{Sources: len(r.Pages), Selected: r.Selected, Excluded: len(r.Pages) - r.Selected, Flagged: r.Flagged}
}

// PageReview returns all sources only to an editor, never via public plans.
func (s *Service) PageReview(ctx context.Context, actor access.Actor, t Target) (PageReview, error) {
	k, m, ref, err := s.pageReviewAccess(ctx, actor, t)
	if err != nil {
		return PageReview{}, err
	}
	out := reviewOf(k, m)
	if s.m.Reader != nil {
		byPath := map[string]int{}
		for i, p := range out.Pages {
			byPath[p.Path] = i
		}
		for offset := 0; ; {
			read, e := s.m.Reader.Read(ctx, ref, actor, media.ReadOptions{Editor: true, Offset: offset, Limit: 200})
			if e != nil {
				return PageReview{}, e
			}
			if read.Cookie != nil {
				out.Cookie = read.Cookie
			}
			for _, f := range read.Files {
				if i, ok := byPath[f.Path]; ok {
					out.Pages[i].SourceURL = f.URL
					out.Pages[i].PreviewURL = f.EditorURL
				}
				if i, ok := byPath[f.From]; ok && strings.HasPrefix(f.Type, "image/") && out.Pages[i].PreviewURL == "" {
					out.Pages[i].PreviewURL = f.URL
				}
			}
			offset += len(read.Files)
			if offset >= read.Total || len(read.Files) == 0 {
				break
			}
		}
	}
	return out, nil
}

func (s *Service) pageReviewAccess(ctx context.Context, actor access.Actor, t Target) (*media.Kind, *media.Manifest, contentref.ContentRef, error) {
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
		guardSegmentedPages(k, m, len(segments[t.Version]) > 0)
	}
	return k, m, ref, err
}

// UpdatePageReview changes only manual selection/order, leaving source and
// automatic evidence intact. A repeat with the old revision refuses cleanly.
func (s *Service) UpdatePageReview(ctx context.Context, actor access.Actor, t Target, patch PageReviewPatch) (PageReview, error) {
	k, _, ref, err := s.pageReviewAccess(ctx, actor, t)
	if err != nil {
		return PageReview{}, err
	}
	unlock, err := s.lockSource(ctx, t)
	if err != nil {
		return PageReview{}, err
	}
	defer unlock()
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if err != nil {
		return PageReview{}, err
	}
	segments, err := s.segments(ctx, s.o.Pool, []string{t.Version})
	if err != nil {
		return PageReview{}, err
	}
	segmented := len(segments[t.Version]) > 0
	m = m.Clone()
	guardSegmentedPages(k, m, segmented)
	candidate := m.Clone()
	apply := func(m *media.Manifest) error {
		r := reviewOf(k, m)
		if patch.Revision == "" || patch.Revision != r.Revision {
			return ErrPageReviewConflict
		}
		byID := map[string]int{}
		for i, f := range m.Files {
			if k.IsPageUpload(f) && !f.Unattached {
				byID[pageID(k, f)] = i
			}
		}
		seen := map[string]bool{}
		for _, change := range patch.Pages {
			i, ok := byID[change.ID]
			if !ok || seen[change.ID] {
				return fmt.Errorf("%w: unknown or repeated page %q", ErrInvalid, change.ID)
			}
			seen[change.ID] = true
			if change.Mode != "auto" && change.Mode != "include" && change.Mode != "exclude" {
				return fmt.Errorf("%w: page mode must be auto, include or exclude", ErrInvalid)
			}
			meta := pageMeta(m.Files[i])
			if meta.SelectionGuard != nil {
				if change.Mode == "include" && !*meta.SelectionGuard || change.Mode == "exclude" && *meta.SelectionGuard {
					return fmt.Errorf("%w: explicit segments must be edited before changing page selection", ErrInvalid)
				}
				if change.Mode == "auto" {
					unguarded := m.Clone()
					guardSegmentedPages(k, unguarded, false)
					automatic := pageMeta(unguarded.Files[i])
					automatic.Mode = "auto"
					writePageMeta(&unguarded.Files[i], automatic)
					for _, p := range reviewOf(k, unguarded).Pages {
						if p.ID == change.ID && p.Included != *meta.SelectionGuard {
							return fmt.Errorf("%w: explicit segments must be edited before changing page selection", ErrInvalid)
						}
					}
				}
			}
			meta.Mode = change.Mode
			meta.OverrideActor = actor.ID
			meta.OverrideAt = s.o.Now().UTC().Format(time.RFC3339Nano)
			writePageMeta(&m.Files[i], meta)
		}
		if patch.Order != nil {
			if segmented {
				for i, id := range patch.Order {
					if i >= len(r.Pages) || r.Pages[i].ID != id {
						return fmt.Errorf("%w: explicit segments must be edited before reordering pages", ErrInvalid)
					}
				}
			}
			if len(patch.Order) != len(byID) {
				return fmt.Errorf("%w: order must name all retained pages", ErrInvalid)
			}
			ordered := make([]media.File, 0, len(byID))
			seen = map[string]bool{}
			for _, id := range patch.Order {
				i, ok := byID[id]
				if !ok || seen[id] {
					return fmt.Errorf("%w: unknown or repeated page in order", ErrInvalid)
				}
				seen[id] = true
				f := m.Files[i]
				meta := pageMeta(f)
				meta.OverrideActor = actor.ID
				meta.OverrideAt = s.o.Now().UTC().Format(time.RFC3339Nano)
				writePageMeta(&f, meta)
				ordered = append(ordered, f)
			}
			n := 0
			for i, f := range m.Files {
				if k.IsPageUpload(f) && !f.Unattached {
					m.Files[i] = ordered[n]
					n++
				}
			}
		}
		persistPageSelection(k, m)
		if len(sourcePages(k, m)) > 0 && reviewOf(k, m).Selected == 0 {
			return fmt.Errorf("%w: at least one source page must remain included", ErrInvalid)
		}
		k.Normalize(m)
		return nil
	}
	if err = apply(candidate); err != nil {
		return PageReview{}, err
	}
	var ops []media.Op
	for _, f := range candidate.Files {
		if !k.IsPageUpload(f) || f.Unattached {
			continue
		}
		before, ok := m.Get(f.Path)
		if !ok {
			return PageReview{}, ErrPageReviewConflict
		}
		if revisionOfInput(before.Meta) != revisionOfInput(f.Meta) {
			blob := f.Blob
			if f.Staged != "" {
				blob = f.Staged
			}
			ops = append(ops, media.Op{Op: media.OpPut, Path: f.Path, Blob: blob, Edit: f.Edit, Meta: f.Meta, SourceName: pageMeta(f).SourceName})
		}
	}
	if patch.Order != nil {
		for index, id := range patch.Order {
			for _, f := range candidate.Files {
				if k.IsPageUpload(f) && !f.Unattached && pageID(k, f) == id {
					at := index
					ops = append(ops, media.Op{Op: media.OpMove, Path: f.Path, Index: &at})
					break
				}
			}
		}
	}
	if len(ops) > 0 {
		_, err = s.m.Uploads.CommitChecked(ctx, system, ref, uuid.NewString(), ops, func(current *media.Manifest) error {
			guarded := current.Clone()
			guardSegmentedPages(k, guarded, segmented)
			if reviewOf(k, guarded).Revision != patch.Revision {
				return ErrPageReviewConflict
			}
			return nil
		})
		if err != nil {
			return PageReview{}, err
		}
	}
	if current, _, e := s.m.Manifests.Get(ctx, ref); e == nil {
		if e = s.auditPages(ctx, k, t, current); e != nil {
			return PageReview{}, e
		}
	}
	return s.PageReview(ctx, actor, t)
}

func persistPageSelection(k *media.Kind, m *media.Manifest) {
	r := reviewOf(k, m)
	byPath := map[string]PageReviewEntry{}
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
	m, err := s.m.Manifests.EditExisting(ctx, ref, func(m *media.Manifest) error {
		before := revisionOfInput(m.Files)
		guardSegmentedPages(k, m, len(segments[t.Version]) > 0)
		taken := map[string]bool{}
		next := 0
		for _, f := range sourcePages(k, m) {
			name := pageID(k, f)
			taken[name] = true
			if _, _, end, ok := media.CanonicalPageName(name); ok && end > next {
				next = end
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
			name, from, to, numeric := media.CanonicalPageName(path.Base(f.Path))
			if !numeric || taken[name] {
				for {
					next++
					name = fmt.Sprintf("%03d", next)
					if !taken[name] {
						break
					}
				}
				meta.Flags = append(meta.Flags, "unusual_source_name")
			}
			taken[name] = true
			meta.Name = name
			if numeric {
				meta.Span = &PageSpan{From: from, To: to}
			}
			writePageMeta(&m.Files[i], meta)
		}
		persistPageSelection(k, m)
		changed = before != revisionOfInput(m.Files)
		return nil
	})
	if err == nil {
		if err = s.auditPages(ctx, k, t, m); err != nil {
			return err
		}
		if err = pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error { return s.recordFacts(ctx, tx, k, t, m) }); err != nil {
			return err
		}
		if changed && s.m.Jobs != nil {
			err = s.m.Jobs.EnqueueProcessing(ctx, media.ProcessJob{Ref: ref})
		}
		return err
	}
	return err
}

// The manifest owns the current decision; this idempotent projection is its
// durable history. A failed write is retried by the existing publication job.
func (s *Service) auditPages(ctx context.Context, k *media.Kind, t Target, m *media.Manifest) error {
	r := reviewOf(k, m)
	batch := &pgx.Batch{}
	for position, entry := range r.Pages {
		f, ok := m.Get(entry.Path)
		if !ok {
			continue
		}
		meta := pageMeta(f)
		actor := meta.OverrideActor
		if actor == "" {
			actor = "automatic"
		}
		body, err := json.Marshal(struct {
			Page     PageReviewEntry `json:"page"`
			Position int             `json:"position"`
			At       string          `json:"override_at,omitempty"`
		}{entry, position, meta.OverrideAt})
		if err != nil {
			return err
		}
		batch.Queue(`INSERT INTO `+pgxIdent(s.o.Schema, "content_page_review_events")+` (tenant_id,version_id,page_id,event_id,actor_id,decision) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, s.o.Tenant, t.Version, entry.ID, revisionOfInput(json.RawMessage(body)), actor, body)
	}
	if batch.Len() == 0 {
		return nil
	}
	return s.o.Pool.SendBatch(ctx, batch).Close()
}

// plannedPageName allocates canonical IDs before archive bytes are committed.
func plannedPageName(source string, taken map[string]bool, next *int) (string, *PageSpan, []string) {
	name, from, to, ok := media.CanonicalPageName(source)
	var span *PageSpan
	if ok {
		span = &PageSpan{From: from, To: to}
		if to > *next {
			*next = to
		}
		if !taken[name] {
			taken[name] = true
			return name, span, nil
		}
	}
	flags := []string{"unusual_source_name"}
	if ok {
		flags = []string{"duplicate_printed_number"}
	}
	for {
		*next++
		name = fmt.Sprintf("%03d", *next)
		if !taken[name] {
			taken[name] = true
			return name, span, flags
		}
	}
}

// revisionOfInput is separate from signed URLs, whose expiry must not create
// optimistic edit conflicts.
func revisionOfInput(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
