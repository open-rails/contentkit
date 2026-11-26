package versionstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/internal/uuidv7"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	versions "github.com/open-rails/contentkit/media/versions"
	"github.com/riverqueue/river"
	"golang.org/x/sync/errgroup"
)

// A release is a move. Unreleased files sit in internal/, which nothing
// serves; releasing copies them to private/ (public/ for a free version's or
// a preview's pages), writes the manifest with their new areas, and leaves
// the internal/ copies retired for GC (an hour). A crash anywhere re-runs
// from the manifest: copies are idempotent, and until the manifest switches,
// readers see the old areas complete. Withdrawal is the same move back.
//
// One job per version (releaseArgs) runs it: woken by every catalog change
// that can move files and by every manifest write, snoozed until the next
// chapter or audience deadline.

// releaseMedia returns only an explicitly configured kind. Absence is an
// ordinary publication without storage; a configured runtime must be bound.
func (s *Service) releaseMedia(kind string) (*mediastore.RegisteredKind, error) {
	if _, exists := s.o.Kinds.Kinds[kind]; !exists {
		return nil, fmt.Errorf("%w: publication kind %q is not configured", ErrInvalid, kind)
	}
	if s.reg == nil {
		if s.o.MediaConfigured {
			return nil, errors.New("versions: configured publication media is not bound")
		}
		return nil, nil
	}
	k, err := s.reg.Kind(kind)
	if errors.Is(err, mediastore.ErrUnknownKind) {
		return nil, nil
	}
	return k, err
}

// releaseLeftovers is how long a release keeps the copies it moved from.
const releaseLeftovers = time.Hour

type publicationCompletion interface {
	CompletePublicationTx(context.Context, pgx.Tx, versions.Target, time.Time) error
	WithdrawPublicationTx(context.Context, pgx.Tx, versions.Target, bool) error
	PublicationCompletedTx(context.Context, pgx.Tx, versions.Target, string, []string, bool, time.Time) error
}

// releaseArgs is a version's release job.
type releaseArgs struct {
	Content string `json:"content"`
	Version string `json:"version"`
	After   int64  `json:"after,omitempty"`
}

func (releaseArgs) Kind() string { return "contentkit_publication" }

func (a releaseArgs) FollowUp(id int64) river.JobArgs {
	a.After = id
	return a
}

type releaseWorker struct {
	river.WorkerDefaults[releaseArgs]
	s *Service
}

func (s *Service) addPublicationWorker(workers *river.Workers) error {
	return river.AddWorkerSafely(workers, &releaseWorker{s: s})
}

func (w *releaseWorker) Timeout(*river.Job[releaseArgs]) time.Duration { return 30 * time.Minute }

func (w *releaseWorker) Work(ctx context.Context, job *river.Job[releaseArgs]) error {
	if err := mediastore.WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), job.Args.After); err != nil {
		return err
	}
	t := versions.Target{Content: job.Args.Content, Version: job.Args.Version}
	// Deletion and restore wake this job: keep the gateway fence in step.
	err := w.s.SyncFences(ctx, []versions.Target{t})
	var next time.Time
	if err == nil || errors.Is(err, ErrNotFound) {
		next, err = w.s.release(ctx, t, job.Attempt > 1)
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case errors.Is(err, mediastore.ErrInvalidManifest):
		return river.JobCancel(err)
	case err != nil:
		return err
	case next.IsZero():
		return nil
	}
	return river.JobSnooze(max(time.Second, next.Sub(w.s.o.Now())))
}

// SchedulePublicationTx wakes the release of each target (an empty Version:
// every version of the content) in tx: its job runs now, whatever it last
// planned for later.
func (s *Service) SchedulePublicationTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error {
	if tx == nil {
		return errors.New("versions: publication scheduling requires a transaction")
	}
	contents, vers := make([]string, len(targets)), make([]string, len(targets))
	for i, target := range targets {
		contents[i], vers[i] = target.Content, target.Version
	}
	// A draft with nothing released has nothing to publish or withdraw.
	rows, err := tx.Query(ctx, `SELECT DISTINCT v.content_id,v.id FROM `+s.t.versions+` v
 JOIN unnest($2::text[],$3::text[]) AS target(content_id,version_id)
 ON target.content_id=v.content_id AND (target.version_id='' OR target.version_id=v.id)
 JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND (v.released OR v.state='published' AND c.state='published')
 ORDER BY v.content_id,v.id`, s.o.Tenant, contents, vers)
	if err != nil {
		return err
	}
	resolved, err := pgx.CollectRows(rows, pgx.RowToStructByPos[versions.Target])
	if err != nil || len(resolved) == 0 {
		return err
	}
	client, err := s.bound()
	if err != nil {
		return err
	}
	for _, t := range resolved {
		if err := mediastore.Wake(ctx, client, tx, releaseArgs{Content: t.Content, Version: t.Version},
			river.InsertOpts{Queue: QueueFor(s.o.Tenant), MaxAttempts: 25}, s.o.Now()); err != nil {
			return err
		}
	}
	return nil
}

// releaseState is what the catalog says about a version's release.
type releaseState struct {
	Kind, State, ParentState string
	At, ParentAt             *time.Time
	Rule                     access.Rule
}

func (st releaseState) published() bool {
	return st.State == "published" && st.ParentState == "published"
}

func (st releaseState) deleted() bool {
	return st.State == "deleted" || st.ParentState == "deleted"
}

// due is when the version itself opens.
func (st releaseState) due(now time.Time) time.Time {
	due := now
	for _, at := range []*time.Time{st.At, st.ParentAt} {
		if at != nil && at.After(due) {
			due = *at
		}
	}
	return due
}

func (s *Service) releaseStateOf(ctx context.Context, q querier, t versions.Target) (releaseState, error) {
	var st releaseState
	err := q.QueryRow(ctx, `SELECT c.kind,v.state,c.state,v.publish_at,c.publish_at,v.access
 FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND v.id=$2 AND v.content_id=$3`, s.o.Tenant, t.Version, t.Content).Scan(&st.Kind, &st.State, &st.ParentState, &st.At, &st.ParentAt, &st.Rule)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

// timing is st's access as released now (only elapsed audience releases)
// and its next deadline: its own due time or a later audience release.
func (st releaseState) timing(now time.Time) (access.Rule, *time.Time) {
	rule := st.Rule
	rule.Releases = nil
	var next *time.Time
	wake := func(at time.Time) {
		if st.published() && at.After(now) && (next == nil || at.Before(*next)) {
			next = &at
		}
	}
	wake(st.due(now))
	for _, release := range st.Rule.Releases {
		if release.At.After(now) {
			wake(release.At)
		} else {
			rule.Releases = append(rule.Releases, release)
		}
	}
	return rule, next
}

// releasePlan is where a version's files belong now, and what readers see
// once they are there.
type releasePlan struct {
	Want     map[string]string // a file's path (wantTrack, wantEditor) -> the area its blob belongs in
	View     releaseView
	Released bool       // the version has anything released
	Chapters []string   // chapters released by this plan
	Next     *time.Time // the next deadline that changes the plan
	Access   access.Rule
	Before   int // pages the manifest served before this release
}

// releaseView is what a version's readers are shown: its released pages
// with their chapters, the leading preview pages, and its playback.
type releaseView struct {
	Pages    pagesSnapshot
	Preview  int
	Playback playbackSnapshot
	Next     *time.Time      // the next deadline that releases more
	Served   map[string]bool // upload paths released
}

// planRelease decides the area of every file m references. Public preset
// outputs (covers) are always public. A live version releases each due
// chapter's ready pages and, once anything is released, its other ready
// uploads' outputs and its declared audio; all of it in private/, or public/
// when the version is free (pages of a locked version's preview too). A
// released upload's master of its own goes to private/ (downloads keep
// their quota); an original never moves, so the identity recipe's masters
// stay where they are. Everything else is internal.
func (s *Service) planRelease(ctx context.Context, q querier, k *mediastore.RegisteredKind, t versions.Target, st releaseState, m *mediastore.Manifest) (*releasePlan, error) {
	now := s.o.Now()
	plan := &releasePlan{Want: map[string]string{}}
	plan.Access, plan.Next = st.timing(now)
	itemDue := st.due(now)
	live := st.published() && m.Deleted == nil && !itemDue.After(now)
	wake := func(at time.Time) {
		if st.published() && at.After(now) && (plan.Next == nil || at.Before(*plan.Next)) {
			plan.Next = &at
		}
	}
	area := layout.AreaPrivate
	if st.Rule.Free(now) {
		area = layout.AreaPublic
	}
	served := map[string]bool{} // upload paths released
	if live && k.Publish != nil {
		groups, err := s.segments(ctx, q, []string{t.Version})
		if err != nil {
			return nil, err
		}
		current := m.Clone()
		guardSegmentedPages(k, current, legacyPageRanges(groups[t.Version]))
		persistPageSelection(k, current)
		var ordering []page
		for _, f := range sourcePages(k, current) {
			if media.PageIncluded(f) {
				ordering = append(ordering, page{Path: f.Path, Source: f.Key(), SourceHash: k.PageID(f)})
			}
		}
		holds := standIns(groups[t.Version], sourcePages(k, current))
		chapters, err := resolveChapterStarts(readableStarts(groups[t.Version], sourcePages(k, current)), ordering)
		if err != nil {
			return nil, err
		}
		dueOf := func(chapter *segment) time.Time {
			if chapter != nil && chapter.ReleasedAt == nil && chapter.ReleaseAt != nil && chapter.ReleaseAt.After(itemDue) {
				return *chapter.ReleaseAt
			}
			return itemDue
		}
		for _, span := range spans(chapters, len(ordering)) {
			var chapter *segment
			if span.Segment != nil {
				for i := range chapters {
					if chapters[i].Position == *span.Segment {
						chapter = &chapters[i]
						break
					}
				}
			}
			if due := dueOf(chapter); due.After(now) {
				wake(due)
				continue
			}
			var pages []page
			for _, p := range ordering[span.Start:span.End] {
				held := false
				for _, h := range holds[p.Path] {
					if at := dueOf(&h); at.After(now) {
						held = true
						wake(at)
					}
				}
				if held {
					continue
				}
				// Each page releases once its renditions are ready; until
				// then, what an earlier release served of it stays served.
				part := current.Clone()
				part.Files = slices.DeleteFunc(part.Files, func(f media.File) bool {
					return f.IsUpload() && f.Path != p.Path || !f.IsUpload() && f.From != p.Path
				})
				ready, err := pagesOf(k, part)
				if err != nil && !errors.Is(err, ErrNotReady) && !errors.Is(err, errFailed) {
					return nil, err
				}
				if err == nil && len(ready) == 1 {
					pages = append(pages, ready[0])
					served[p.Path] = true
				} else if was, ok := servedPage(k, m, p.Path); ok {
					pages = append(pages, was)
					for _, r := range was.Files {
						plan.Want[r.Path] = layout.BlobArea(r.Blob)
					}
				}
			}
			start := len(plan.View.Pages.Pages)
			plan.View.Pages.Pages = append(plan.View.Pages.Pages, pages...)
			if chapter != nil && len(pages) > 0 {
				g := *chapter
				g.Start, g.End = start, len(plan.View.Pages.Pages)
				if g.ReleasedAt == nil {
					g.ReleasedAt = &now
					plan.Chapters = append(plan.Chapters, chapter.ID)
				}
				plan.View.Pages.Segments = append(plan.View.Pages.Segments, g)
			}
		}
	}
	initial := live && (k.Publish == nil || len(plan.View.Pages.Pages) > 0 || len(sourcePages(k, m)) == 0 && len(imports(k, m)) == 0)
	if live && !k.Readiness(m).Ready() && len(k.Missing(m)) > 0 {
		// A release without a required upload is withdrawn, not kept.
		initial, served, plan.View, plan.Want = false, map[string]bool{}, releaseView{}, map[string]string{}
	}
	if initial {
		for _, f := range m.Files {
			if f.IsUpload() && !f.Unattached && !f.Gone && f.Blob != "" && f.Fail() == nil && !k.IsPageUpload(f) && len(f.Pending) == 0 {
				served[f.Path] = true
			}
		}
	}
	previewed := map[string]bool{}
	if p := st.Rule.Previewed(s.o.Kinds.Kinds[st.Kind]).Preview; live && area == layout.AreaPrivate && p != nil {
		plan.View.Preview = min(p.Pages, len(plan.View.Pages.Pages))
		for _, pg := range plan.View.Pages.Pages[:plan.View.Preview] {
			for _, r := range pg.Files {
				previewed[r.Path] = true
			}
		}
	}
	// Areas are decided per file, not per blob: pages with identical bytes
	// in different chapters reference the same hash in different areas.
	want := func(key, blob, to string) {
		if blob == "" {
			return
		}
		if _, set := plan.Want[key]; !set {
			plan.Want[key] = to
		}
	}
	for _, f := range m.Files {
		if f.IsUpload() && !f.Gone && f.Master != nil && !f.Master.IsOriginal() {
			if served[f.Path] {
				want(wantMaster(f.Path), f.Master.Blob, layout.AreaPrivate)
			}
			want(wantMaster(f.Path), f.Master.Blob, layout.AreaInternal)
		}
		switch {
		case f.Gone || f.Blob == "":
		case k.Area(f.Preset) == layout.AreaPublic && !f.IsUpload():
			want(f.Path, f.Blob, layout.AreaPublic) // a cover is always visible
		case f.IsUpload() && served[f.Path] && k.AudioUpload(f):
			want(f.Path, f.Blob, area)
		case !f.IsUpload() && served[f.From] && f.Fail() == nil && k.ReaderOutput(m, f):
			to := area
			if previewed[f.Path] {
				to = layout.AreaPublic
			}
			want(f.Path, f.Blob, to)
			if f.Track != nil {
				want(wantTrack(f.Path), f.Track.Index, to)
			}
		}
		want(f.Path, f.Blob, layout.AreaInternal)
		if f.Track != nil {
			want(wantTrack(f.Path), f.Track.Index, layout.AreaInternal)
		}
		if f.Editor != nil {
			want(wantEditor(f.Path), f.Editor.Blob, layout.AreaInternal)
		}
	}
	if k.Publish != nil && k.Publish.Playback != "" && initial {
		if tracks, err := tracksOf(k, m); err == nil {
			plan.View.Playback.Tracks = tracks
		}
	}
	plan.Released = initial || len(plan.View.Pages.Pages) > 0
	plan.View.Served = served
	return plan, nil
}

// servedPage is page path as m serves it now: its renditions all in a
// served area.
func servedPage(k *mediastore.RegisteredKind, m *mediastore.Manifest, path string) (page, bool) {
	part := m.Clone()
	part.Files = slices.DeleteFunc(part.Files, func(f media.File) bool {
		return f.IsUpload() && f.Path != path || !f.IsUpload() && f.From != path
	})
	for i := range part.Files {
		part.Files[i].Pending = nil
		part.Files[i].Failed = nil
	}
	pages, err := pagesOf(k, part)
	if err != nil || len(pages) != 1 {
		return page{}, false
	}
	for _, r := range pages[0].Files {
		if !layout.Served(layout.BlobArea(r.Blob)) {
			return page{}, false
		}
	}
	return pages[0], true
}

// zipUpload reports an uploaded ZIP: never a reader download, as it may
// hold pages of chapters not released yet.
func zipUpload(f media.File) bool {
	return f.Type == "application/zip" || f.Type == "application/x-zip-compressed" || strings.EqualFold(path.Ext(f.Path), ".zip")
}

// uploadStem is an upload's path without its extension, as Kind.Download
// and upload rules name it.
func uploadStem(p string) string { return strings.TrimSuffix(p, path.Ext(p)) }

// release moves t's files where planRelease wants them and records the
// outcome in the catalog. It returns the next deadline. A retry may follow
// an attempt that copied files and never named them.
func (s *Service) release(ctx context.Context, t versions.Target, retry bool) (time.Time, error) {
	st, err := s.releaseStateOf(ctx, s.o.Pool, t)
	if err != nil {
		return time.Time{}, err
	}
	k, err := s.releaseMedia(st.Kind)
	if err != nil {
		return time.Time{}, err
	}
	if st.deleted() {
		// The fence closes its files; its release stays for a restore.
		return time.Time{}, nil
	}
	var plan *releasePlan
	var op string
	if k == nil {
		// A kind without media: availability and audience alone.
		now := s.o.Now()
		plan = &releasePlan{Released: st.published() && !st.due(now).After(now)}
		plan.Access, plan.Next = st.timing(now)
	} else {
		ref, err := s.ref(st.Kind, t)
		if err != nil {
			return time.Time{}, err
		}
		m, _, err := s.m.Manifests.Get(ctx, ref)
		if errors.Is(err, mediastore.ErrNotFound) {
			m = &mediastore.Manifest{V: media.ManifestVersion, Files: []media.File{}}
		} else if err != nil {
			return time.Time{}, err
		}
		if now := s.o.Now(); st.published() && !st.due(now).After(now) && k.Readiness(m).State == media.StateProcessing {
			// A release is all or nothing: while uploads still process, what
			// is served stays as it is. The manifest write that settles
			// processing wakes this job again.
			return time.Time{}, nil
		}
		if plan, err = s.planRelease(ctx, s.o.Pool, k, t, st, m); err != nil {
			return time.Time{}, err
		}
		if k.Publish != nil {
			for _, f := range sourcePages(k, m) {
				if _, ok := servedPage(k, m, f.Path); ok {
					plan.Before++
				}
			}
		}
		if retry {
			if err := s.collectOrphans(ctx, ref); err != nil {
				return time.Time{}, err
			}
		}
		var settling time.Time
		if op, settling, err = s.move(ctx, ref, m, plan.Want); err != nil {
			return time.Time{}, err
		}
		if !settling.IsZero() && (plan.Next == nil || settling.Before(*plan.Next)) {
			plan.Next = &settling
		}
	}
	return s.completeRelease(ctx, k, t, plan, op)
}

// wantTrack and wantEditor key a file's track index and editor view in a
// release plan's Want.
func wantTrack(path string) string  { return path + "\x00track" }
func wantEditor(path string) string { return path + "\x00editor" }
func wantMaster(path string) string { return path + "\x00master" }

// move copies every file whose area differs from want into its area, then
// writes the manifest naming the copies; a copy no file names any more is
// retired. A copy whose name is still being deleted (a file withdrawn hours
// ago) waits. It returns the write's op ("" when nothing moved) and when the
// next waiting move can run (zero for none).
func (s *Service) move(ctx context.Context, ref contentref.ContentRef, m *mediastore.Manifest, want map[string]string) (string, time.Time, error) {
	item, err := s.reg.Item(ref)
	if err != nil {
		return "", time.Time{}, err
	}
	var settling time.Time
	type change struct{ from, to string }
	changes := map[string]change{} // want key -> its blob's move
	copies := map[change]bool{}
	plan := func(key, blob string) {
		area, ok := want[key]
		if !ok || blob == "" || layout.BlobArea(blob) == area {
			return
		}
		to := layout.MoveRef(blob, area)
		if until, wait := s.m.Manifests.Settling(m, to, s.o.Now()); wait {
			if settling.IsZero() || until.Before(settling) {
				settling = until
			}
			return
		}
		c := change{blob, to}
		changes[key], copies[c] = c, true
	}
	for _, f := range m.Files {
		if f.Gone {
			continue
		}
		plan(f.Path, f.Blob)
		if f.Track != nil {
			plan(wantTrack(f.Path), f.Track.Index)
		}
		if f.Editor != nil {
			plan(wantEditor(f.Path), f.Editor.Blob)
		}
		if f.Master != nil && !f.Master.IsOriginal() {
			plan(wantMaster(f.Path), f.Master.Blob)
		}
	}
	if len(changes) == 0 {
		return "", settling, nil
	}
	store := s.m.Manifests.Store()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for c := range copies {
		g.Go(func() error {
			src, _ := item.Blob(c.from)
			dst, _ := item.Blob(c.to)
			if _, err := store.Copy(gctx, src, dst, media.CopyOptions{}); err != nil {
				return fmt.Errorf("versions: release %s: %w", src, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return "", time.Time{}, errors.Join(err, s.collectOrphans(ctx, ref))
	}
	// A copy withdrawn to internal/, or leaving public/, would stay readable
	// until deleted: it is due at once. Any other copy a move replaces stays
	// an hour, for readers mid-read.
	immediate := map[string]bool{}
	for c := range copies {
		if layout.BlobArea(c.to) == layout.AreaInternal || layout.BlobArea(c.from) == layout.AreaPublic && layout.BlobArea(c.to) != layout.AreaPublic {
			immediate[c.from] = true
		}
	}
	op := uuidv7.NewString()
	out, err := s.m.Manifests.EditWith(ctx, ref, mediastore.EditOptions{Existing: true, Op: op, Grace: releaseLeftovers, GraceOf: func(blob string) time.Duration {
		if immediate[blob] {
			return -1
		}
		return 0
	}}, func(cur *mediastore.Manifest) error {
		changed := false
		apply := func(key string, blob *string) {
			if c, ok := changes[key]; ok && *blob == c.from {
				*blob, changed = c.to, true
			}
		}
		for i := range cur.Files {
			f := &cur.Files[i]
			if f.Gone {
				continue
			}
			apply(f.Path, &f.Blob)
			if f.Track != nil {
				track := *f.Track
				apply(wantTrack(f.Path), &track.Index)
				f.Track = &track
			}
			if f.Editor != nil {
				editor := *f.Editor
				apply(wantEditor(f.Path), &editor.Blob)
				f.Editor = &editor
			}
			if f.Master != nil && !f.Master.IsOriginal() {
				master := *f.Master
				apply(wantMaster(f.Path), &master.Blob)
				f.Master = &master
			}
		}
		if !changed {
			return mediastore.ErrUnchanged
		}
		return nil
	})
	// Copies the manifest does not name (a failed write, a plan changed
	// meanwhile) are orphans: a collection once they are a day old.
	named := map[string]bool{}
	if err == nil {
		for _, blob := range out.Blobs() {
			named[blob] = true
		}
	}
	for c := range copies {
		if !named[c.to] {
			return op, settling, errors.Join(err, s.collectOrphans(ctx, ref))
		}
	}
	return op, settling, err
}

// collectOrphans schedules ref's collection for when copies a release may
// have left unnamed are old enough to be collected.
func (s *Service) collectOrphans(ctx context.Context, ref contentref.ContentRef) error {
	client, err := s.bound()
	if err != nil {
		return err
	}
	return mediastore.RiverGC{Client: client}.ScheduleGC(ctx, ref, s.o.Now().Add(mediastore.OrphanAge+time.Hour))
}

// completeRelease records a release's outcome in the catalog: availability,
// released chapters, the access it was released under, and the fact.
func (s *Service) completeRelease(ctx context.Context, k *mediastore.RegisteredKind, t versions.Target, plan *releasePlan, op string) (time.Time, error) {
	completion, ok := s.o.Facets.(publicationCompletion)
	if !ok {
		return time.Time{}, errors.New("versions: catalog publication completion is not configured")
	}
	err := pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if err := s.lockTarget(ctx, tx, t); err != nil {
			return err
		}
		var released bool
		if err := tx.QueryRow(ctx, `SELECT released FROM `+s.t.versions+` WHERE tenant_id=$1 AND id=$2`, s.o.Tenant, t.Version).Scan(&released); err != nil {
			return err
		}
		at := s.o.Now()
		if len(plan.Chapters) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.segments+` SET released_at=coalesce(released_at,$4) WHERE tenant_id=$1 AND version_id=$2 AND id=ANY($3)`, s.o.Tenant, t.Version, plan.Chapters, at); err != nil {
				return err
			}
		}
		if k != nil && k.Publish != nil && released && len(plan.View.Pages.Pages) > plan.Before {
			// More pages than the last release: cards mark the work updated.
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` SET pages_added_at=now() WHERE tenant_id=$1 AND id=$2`, s.o.Tenant, t.Version); err != nil {
				return err
			}
		}
		accessChanged := false
		if plan.Released {
			tag, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` SET released_access=$3 WHERE tenant_id=$1 AND id=$2
 AND released_access IS DISTINCT FROM $3::jsonb`, s.o.Tenant, t.Version, plan.Access)
			if err != nil {
				return err
			}
			accessChanged = tag.RowsAffected() > 0
		}
		switch {
		case plan.Released && !released:
			if err := completion.CompletePublicationTx(ctx, tx, t, at); err != nil {
				return err
			}
		case !plan.Released && released:
			if err := completion.WithdrawPublicationTx(ctx, tx, t, false); err != nil {
				return err
			}
		case accessChanged:
			// Listings follow the access the version is released under.
			if err := s.o.Facets.RefreshTx(ctx, tx, []string{t.Version}); err != nil {
				return err
			}
		}
		if op != "" || plan.Released != released || len(plan.Chapters) > 0 || accessChanged {
			id := op
			if id == "" {
				id = uuidv7.NewString()
			}
			return completion.PublicationCompletedTx(ctx, tx, t, id, plan.Chapters, plan.Released, at)
		}
		return nil
	})
	if err != nil || plan.Next == nil {
		return time.Time{}, err
	}
	return *plan.Next, nil
}

// releasedView is what t's readers see now: its plan's view.
func (s *Service) releasedView(ctx context.Context, k *mediastore.RegisteredKind, t versions.Target) (*releaseView, *mediastore.Manifest, error) {
	st, err := s.releaseStateOf(ctx, s.o.Pool, t)
	if err != nil {
		return nil, nil, err
	}
	ref, err := s.ref(st.Kind, t)
	if err != nil {
		return nil, nil, err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, mediastore.ErrNotFound) {
		return nil, nil, ErrNotReady
	} else if err != nil {
		return nil, nil, err
	}
	plan, err := s.planRelease(ctx, s.o.Pool, k, t, st, m)
	if err != nil {
		return nil, nil, err
	}
	// Only what the manifest already serves is shown: a page the plan wants
	// released but has not moved yet is not readable.
	view := plan.View
	view.Next, view.Served = plan.Next, maps.Clone(plan.View.Served)
	view.Pages.Pages = slices.DeleteFunc(slices.Clone(view.Pages.Pages), func(p page) bool {
		return slices.ContainsFunc(p.Files, func(r rendition) bool { return !layout.Served(layout.BlobArea(r.Blob)) })
	})
	if len(view.Pages.Pages) != len(plan.View.Pages.Pages) {
		view.Pages.Segments = nil // boundaries no longer line up; the next release restores them
		view.Preview = min(view.Preview, len(view.Pages.Pages))
	}
	view.Playback.Tracks = slices.DeleteFunc(slices.Clone(view.Playback.Tracks), func(tr track) bool {
		return !layout.Served(layout.BlobArea(tr.File.Blob))
	})
	if !plan.Released {
		return nil, m, ErrNotReady
	}
	return &view, m, nil
}

// WakeRequiredReleases wakes the release of every released version whose
// kind requires an upload: after a deploy made an upload required, the next
// run withdraws a release without it. client only inserts. It returns the
// versions woken.
func (s *Service) WakeRequiredReleases(ctx context.Context, client *river.Client[pgx.Tx]) (int, error) {
	if s.reg == nil {
		return 0, nil
	}
	var kinds []string
	for kind := range s.o.Kinds.Kinds {
		k, err := s.releaseMedia(kind)
		if err != nil {
			return 0, err
		}
		if k != nil && slices.ContainsFunc(k.Uploads, func(u media.Upload) bool { return u.Required }) {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		return 0, nil
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT v.content_id,v.id FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND v.released AND v.state<>'deleted' AND c.state<>'deleted' AND c.kind=ANY($2) ORDER BY v.id`, s.o.Tenant, kinds)
	if err != nil {
		return 0, err
	}
	targets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[versions.Target])
	if err != nil {
		return 0, err
	}
	woken := 0
	for _, t := range targets {
		ref, err := s.ref(s.kindOf(ctx, t), t)
		if err != nil {
			continue
		}
		m, _, err := s.m.Manifests.Get(ctx, ref)
		if err != nil {
			continue
		}
		k, _ := s.reg.Kind(ref.ContentKind)
		if len(k.Missing(m)) == 0 {
			continue
		}
		if err := mediastore.Wake(ctx, client, nil, releaseArgs{Content: t.Content, Version: t.Version},
			river.InsertOpts{Queue: QueueFor(s.o.Tenant), MaxAttempts: 25}, s.o.Now()); err != nil {
			return woken, err
		}
		woken++
	}
	return woken, nil
}

func (s *Service) kindOf(ctx context.Context, t versions.Target) string {
	var kind string
	_ = s.o.Pool.QueryRow(ctx, `SELECT kind FROM `+s.t.items+` WHERE tenant_id=$1 AND id=$2`, s.o.Tenant, t.Content).Scan(&kind)
	return kind
}
