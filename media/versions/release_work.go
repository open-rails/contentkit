package versions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// A releasePlan is a frozen storage attempt. Chapter membership is derived
// from ordered virtual paths each time a new attempt is prepared.
type releasePlan struct {
	EffectiveAccess access.Rule `json:"effective_access"`
	Target
	Kind        string            `json:"kind"`
	Fingerprint string            `json:"fingerprint"`
	Sources     map[string]string `json:"sources,omitempty"` // virtual paths -> existing immutable source references
	View        *media.Manifest   `json:"view"`
	Pages       pagesSnapshot     `json:"pages"`
	Playback    playbackSnapshot  `json:"playback"`
	Copies      []media.Promotion `json:"copies"`
	Chapters    []string          `json:"chapters"`
	Previous    []string          `json:"previous"`
	Released    bool              `json:"released"`
	Retained    bool              `json:"retained,omitempty"`
	Waiting     bool              `json:"waiting"`
	NextAt      *time.Time        `json:"next_at,omitempty"`
}

type publicationCompletion interface {
	CompletePublicationTx(context.Context, pgx.Tx, Target, time.Time) error
	WithdrawPublicationTx(context.Context, pgx.Tx, Target, bool) error
	PublicationCompletedTx(context.Context, pgx.Tx, Target, string, []string, bool, time.Time) error
}

type releaseMutationContextKey struct{}

// A reader-equivalent revision can still have stale physical copies from an
// attempt superseded after storage I/O. Never skip their recorded retirement.
func (p *releasePlan) storageSettled(active *releasePlan) bool {
	if active.View == nil || len(p.Previous) != len(active.View.Deliveries) {
		return false
	}
	for _, id := range p.Previous {
		if !slices.ContainsFunc(active.View.Deliveries, func(d media.PublicationDelivery) bool { return d.ID == id && d.Ready && !d.Withdrawn }) {
			return false
		}
	}
	for _, copy := range p.Copies {
		area, _, valid := layout.SplitBlobRef(copy.Source)
		if !valid || area != copy.Area {
			return false
		}
	}
	return true
}

// Download-only renditions stay internal. A whole-item reader token must not
// bypass a host's delayed-download or personalized-watermark policy.
func downloadOnly(k *media.Kind, f media.File) bool {
	z := private(k, k.Download)
	return z != nil && z.HostOnly && z.Zip != "" && strings.HasPrefix(f.Path, z.Zip)
}

func (s *Service) publicationRequestsTable() string {
	return pgxIdent(s.o.Schema, "content_publication_requests")
}

// releaseDueTx schedules the first publication at its requested time, while
// edits of an already released item reconcile immediately (including withdrawal).
func (s *Service) releaseDueTx(ctx context.Context, tx pgx.Tx, t Target) (time.Time, error) {
	var state, parentState string
	var at, parentAt *time.Time
	var released, previouslyPublished, storageStarted bool
	err := tx.QueryRow(ctx, `SELECT v.state,c.state,v.publish_at,c.publish_at,v.released,v.release_id IS NOT NULL
 FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND v.id=$2 AND v.content_id=$3`, s.o.Tenant, t.Version, t.Content).Scan(&state, &parentState, &at, &parentAt, &released, &previouslyPublished)
	if err != nil {
		return time.Time{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.publicationRequestsTable()+`
 WHERE tenant_id=$1 AND version_id=$2 AND plan IS NOT NULL AND state<>'cancelled')`, s.o.Tenant, t.Version).Scan(&storageStarted); err != nil {
		return time.Time{}, err
	}
	if (state != "published" || parentState != "published") && !released && !previouslyPublished && !storageStarted {
		return time.Time{}, nil
	}
	due := s.o.Now()
	if !released && !storageStarted && state == "published" && parentState == "published" {
		for _, value := range []*time.Time{at, parentAt} {
			if value != nil && value.After(due) {
				due = *value
			}
		}
	}
	return due, nil
}

func (s *Service) activeRelease(ctx context.Context, q querier, t Target) (*releasePlan, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT r.result FROM `+s.t.versions+` v
 JOIN `+s.publicationRequestsTable()+` r ON r.tenant_id=v.tenant_id AND r.id=v.release_id
 WHERE v.tenant_id=$1 AND v.id=$2 AND v.content_id=$3 AND r.state IN ('retiring','completed')`, s.o.Tenant, t.Version, t.Content).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var plan releasePlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// readerManifest returns only the last completed storage view. Pending edits
// remain in the authoring manifest and cannot change an already released view.
func (s *Service) readerManifest(ctx context.Context, ref contentref.ContentRef, actor access.Actor, source *media.Manifest) (*media.Manifest, error) {
	k, err := s.reg.Kind(ref.ContentKind)
	if err != nil || !k.Versions {
		return source, err
	}
	if !source.PublicationLayoutReady() {
		return nil, ErrNotReady
	}
	active, err := s.activeRelease(ctx, s.o.Pool, Target{Content: ref.ContentID, Version: ref.Version()})
	if err != nil {
		return nil, err
	}
	if active == nil || active.View == nil {
		return &media.Manifest{V: media.ManifestVersion, Files: []media.File{}}, nil
	}
	return active.View.Clone(), nil
}

func (s *Service) planRelease(ctx context.Context, tx pgx.Tx, t Target) (*releasePlan, error) {
	var kind, state, parentState string
	var at, parentAt *time.Time
	var rule access.Rule
	err := tx.QueryRow(ctx, `SELECT c.kind,v.state,c.state,v.publish_at,c.publish_at,v.access
 FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND v.id=$2 AND v.content_id=$3`, s.o.Tenant, t.Version, t.Content).Scan(&kind, &state, &parentState, &at, &parentAt, &rule)
	if err != nil {
		return nil, err
	}
	k, err := s.reg.Kind(kind)
	if err != nil {
		return nil, err
	}
	ref, err := s.ref(kind, t)
	if err != nil {
		return nil, err
	}
	m, _, err := s.m.Manifests.GetTx(ctx, tx, ref)
	if errors.Is(err, media.ErrNotFound) {
		m, err = &media.Manifest{V: media.ManifestVersion, Files: []media.File{}}, nil
	}
	if err != nil {
		return nil, err
	}
	if !m.PublicationLayoutReady() {
		return nil, media.ErrPublicationLayoutRequired
	}
	// GetTx returns a shared cached manifest. The projection's public/track
	// references are remapped after copying; never mutate that cached source.
	m = m.Clone()
	now := s.o.Now()
	itemDue := now
	for _, value := range []*time.Time{at, parentAt} {
		if value != nil && value.After(itemDue) {
			itemDue = *value
		}
	}
	live := state == "published" && parentState == "published" && !m.Deleted && !itemDue.After(now)
	plan := &releasePlan{Target: t, Kind: kind, View: m.Clone(), Sources: map[string]string{}, Pages: pagesSnapshot{Pages: []page{}}, Playback: playbackSnapshot{Tracks: []track{}}}
	plan.EffectiveAccess = rule
	plan.EffectiveAccess.Releases = nil
	for _, release := range rule.Releases {
		if release.At.After(now) {
			if live && (plan.NextAt == nil || release.At.Before(*plan.NextAt)) {
				next := release.At
				plan.NextAt = &next
			}
		} else {
			plan.EffectiveAccess.Releases = append(plan.EffectiveAccess.Releases, release)
		}
	}
	plan.View.Files, plan.View.Deliveries = nil, nil
	plan.View.Receipt = nil
	if state == "published" && parentState == "published" && itemDue.After(now) {
		plan.NextAt = &itemDue
	}
	prior, err := s.activeRelease(ctx, tx, t)
	if err != nil {
		return nil, err
	}
	if prior != nil && prior.View != nil {
		// applyDelivery keeps only aliases whose served destinations the new
		// delivery actually owns, including unchanged HLS index references.
		plan.View.Deliveries = slices.Clone(prior.View.Deliveries)
	}
	for _, delivery := range m.Deliveries {
		if !delivery.Withdrawn {
			plan.Previous = append(plan.Previous, delivery.ID)
		}
	}
	if prior != nil && prior.Released && !m.Deleted && (state == "deleted" || parentState == "deleted") &&
		(state == "published" || state == "deleted") && (parentState == "published" || parentState == "deleted") {
		// Retention freezes exactly the completed snapshot, including chapter
		// membership and source revisions. Authoring edits and future deadlines
		// cannot add files while either this edition or its parent is deleted.
		prior.Copies, prior.Previous = nil, plan.Previous
		prior.Waiting, prior.NextAt, prior.Retained = false, nil, true
		prior.EffectiveAccess = access.Rule{Level: access.PPV}
		prior.Fingerprint = "retained:" + strings.TrimPrefix(prior.Fingerprint, "retained:")
		pin := func(blob, area string) error {
			if blob == "" {
				return nil
			}
			sourceArea, _, valid := layout.SplitBlobRef(blob)
			if !valid || sourceArea != layout.AreaPublic && sourceArea != layout.AreaPrivate {
				return fmt.Errorf("%w: retained publication contains unserved media", ErrInvalid)
			}
			copy := media.Promotion{Source: blob, Area: area}
			if !slices.Contains(prior.Copies, copy) {
				prior.Copies = append(prior.Copies, copy)
			}
			return nil
		}
		for _, f := range prior.View.Files {
			if !f.IsUpload() || k.AudioUpload(f) || k.ServeOriginals && f.Type != "application/zip" {
				if err := pin(f.Blob, layout.AreaPrivate); err != nil {
					return nil, err
				}
			}
			if f.Track != nil {
				if err := pin(f.Track.Index, layout.AreaPrivate); err != nil {
					return nil, err
				}
			}
			// Configured public previews remain public, just as for a paid
			// edition. Ordinary reader files move to private before completion.
			for _, pub := range f.Public {
				for _, blob := range pub.Blobs {
					if err := pin(blob, layout.AreaPublic); err != nil {
						return nil, err
					}
				}
			}
		}
		return prior, nil
	}
	served := map[string]bool{}
	retained := map[string]bool{}
	var ordering []page
	if k.Publish != nil {
		for _, f := range sourcePages(k, m) {
			if media.PageIncluded(f) {
				ordering = append(ordering, page{Path: f.Path, Source: f.Key(), SourceHash: k.PageID(f)})
			}
		}
		groups, err := s.segments(ctx, tx, []string{t.Version})
		if err != nil {
			return nil, err
		}
		holds := standIns(groups[t.Version], sourcePages(k, m))
		chapters, err := resolveChapterStarts(readableStarts(groups[t.Version], sourcePages(k, m)), ordering)
		if err != nil {
			return nil, err
		}
		dueOf := func(chapter *segment) time.Time {
			if chapter != nil && chapter.ReleasedAt == nil && chapter.ReleaseAt != nil && chapter.ReleaseAt.After(itemDue) {
				return *chapter.ReleaseAt
			}
			return itemDue
		}
		wake := func(due time.Time) {
			if state == "published" && parentState == "published" && due.After(now) && (plan.NextAt == nil || due.Before(*plan.NextAt)) {
				plan.NextAt = &due
			}
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
			due := dueOf(chapter)
			if !live || due.After(now) {
				wake(due)
				continue
			}
			selected := map[string]bool{}
			for _, p := range ordering[span.Start:span.End] {
				selected[p.Path] = true
				for _, held := range holds[p.Path] {
					if at := dueOf(&held); at.After(now) {
						delete(selected, p.Path)
						wake(at)
					}
				}
			}
			part := m.Clone()
			part.Files = slices.DeleteFunc(part.Files, func(f media.File) bool {
				return f.IsUpload() && !selected[f.Path] || !f.IsUpload() && !selected[f.From]
			})
			pages, readyErr := pagesOf(k, part)
			if readyErr != nil {
				if !errors.Is(readyErr, ErrNotReady) && !errors.Is(readyErr, errFailed) {
					return nil, readyErr
				}
				plan.Waiting = true
				// Keep the prior published pages still inside this span. New or
				// replaced bytes wait, without hiding the previously released set.
				if prior != nil {
					for _, p := range prior.Pages.Pages {
						if selected[p.Path] {
							pages = append(pages, p)
							served[p.Path] = true
							retained[p.Path] = true
						}
					}
				}
			} else {
				for path := range selected {
					served[path] = true
				}
				if chapter != nil {
					plan.Chapters = append(plan.Chapters, chapter.ID)
				}
			}
			start := len(plan.Pages.Pages)
			plan.Pages.Pages = append(plan.Pages.Pages, pages...)
			if chapter != nil && len(pages) > 0 {
				g := *chapter
				g.Start, g.End = start, len(plan.Pages.Pages)
				if g.ReleasedAt == nil {
					g.ReleasedAt = &now
				}
				plan.Pages.Segments = append(plan.Pages.Segments, g)
			}
		}
	}
	initial := live && (k.Publish == nil || len(plan.Pages.Pages) > 0 || len(ordering) == 0 && len(imports(k, m)) == 0)
	if live && k.Publish != nil && len(imports(k, m)) > 0 {
		plan.Waiting = true
	}
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached || f.Gone || k.IsPageUpload(f) {
			continue
		}
		if initial && f.Blob != "" && f.Fail() == nil {
			served[f.Path] = true
		} else if live && (f.Staged != "" || len(f.Pending) > 0) {
			plan.Waiting = true
		}
	}
	area := layout.AreaPrivate
	if rule.Free(now) {
		area = layout.AreaPublic
	}
	// Fingerprinting ignores allocation moves but includes ordered source
	// revisions, chapter boundaries and desired areas. A move's own callback
	// therefore cannot schedule an endless sequence of identical copies.
	var identity []any
	for _, f := range m.Files {
		if f.IsUpload() && !f.Unattached {
			plan.Sources[f.Path] = f.Key()
			identity = append(identity, []any{f.Path, f.Key(), f.Staged, f.Gone, served[f.Path]})
		}
	}
	add := func(blob, destination string) error {
		if blob == "" {
			return nil
		}
		digest, valid := layout.BlobDigest(blob)
		if !valid {
			return fmt.Errorf("%w: non-immutable publication source", ErrInvalid)
		}
		for _, previous := range plan.Copies {
			if previous.Source == blob && previous.Area == destination {
				return nil
			}
		}
		current, _, _ := layout.SplitBlobRef(blob)
		if destination != layout.AreaFuture || current != destination {
			plan.Copies = append(plan.Copies, media.Promotion{Source: blob, Area: destination})
		}
		identity = append(identity, []string{hex.EncodeToString(digest), destination})
		return nil
	}
	for _, file := range m.Files {
		f := file
		if f.Unattached || f.Gone {
			continue
		}
		if retained[f.Path] || retained[f.From] {
			continue
		}
		if f.IsUpload() {
			for i := range f.Public {
				pub := &f.Public[i]
				if !pub.Ready() {
					continue
				}
				destination := layout.AreaFuture
				if served[f.Path] {
					destination = layout.AreaPublic
				}
				for _, blob := range pub.Blobs {
					if err := add(blob, destination); err != nil {
						return nil, err
					}
				}
			}
			if served[f.Path] {
				// Uploaded originals stay internal; only declared audio is
				// copied for direct reader playback.
				if k.AudioUpload(f) || k.ServeOriginals && f.Type != "application/zip" {
					if previous, ok := m.Delivered(f.Blob, area); ok {
						f.Blob = previous
					} else if prior != nil && prior.View != nil {
						for path, source := range prior.Sources {
							if source != file.Key() {
								continue
							}
							if previous, ok := prior.View.Get(path); ok {
								previousArea, _, valid := layout.SplitBlobRef(previous.Blob)
								if valid && previousArea == area {
									f.Blob = previous.Blob
									break
								}
							}
						}
					}
					if err := add(f.Blob, area); err != nil {
						return nil, err
					}
				} else {
					// Preserve the internal source reference: public rendition
					// ownership is checked against its source/edit key. Reader
					// grants reject originals independently of this snapshot.
					f.Staged, f.Editor = "", nil
				}
				for j := range f.Public {
					if f.Public[j].Source == file.Key() {
						f.Public[j].Source = f.Key()
					}
				}
				plan.View.Files = append(plan.View.Files, f)
			}
			continue
		}
		destination := layout.AreaFuture
		if served[f.From] && f.Fail() == nil && !downloadOnly(k, f) {
			if spec := private(k, f.Preset); spec != nil && spec.HostOnly {
				continue
			}
			destination = area
		}
		if err := add(f.Blob, destination); err != nil {
			return nil, err
		}
		if f.Track != nil {
			if err := add(f.Track.Index, destination); err != nil {
				return nil, err
			}
		}
		if destination != layout.AreaFuture {
			plan.View.Files = append(plan.View.Files, f)
		}
	}
	if prior != nil && prior.View != nil {
		for _, f := range prior.View.Files {
			if !retained[f.Path] && !retained[f.From] {
				continue
			}
			plan.View.Files = append(plan.View.Files, f)
			if !f.IsUpload() || k.AudioUpload(f) {
				if err := add(f.Blob, area); err != nil {
					return nil, err
				}
			}
			for _, pub := range f.Public {
				for _, blob := range pub.Blobs {
					if err := add(blob, layout.AreaPublic); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if k.Publish != nil && k.Publish.Playback != "" {
		tracks, err := tracksOf(k, plan.View)
		if err == nil {
			plan.Playback.Tracks = tracks
		}
	}
	plan.Released = initial || len(plan.Pages.Pages) > 0
	var boundaries []any
	for _, chapter := range plan.Pages.Segments {
		boundaries = append(boundaries, []any{chapter.ID, chapter.Position, chapter.Title, chapter.StartPage, chapter.Start, chapter.End, chapter.ReleaseAt})
	}
	// A scheduled policy can change eligibility while all objects remain in
	// private/. Its completed policy must still advance the active revision.
	encoded, err := json.Marshal([]any{identity, boundaries, plan.Chapters, state, parentState, rule, plan.EffectiveAccess, plan.Released})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	plan.Fingerprint = hex.EncodeToString(sum[:])
	return plan, nil
}

func (p *releasePlan) applyDelivery(delivery *media.PublicationDelivery) {
	lookup := map[string]string{}
	for _, f := range delivery.Files {
		area, _, _ := layout.SplitBlobRef(f.Blob)
		if area == layout.AreaPrivate || area == layout.AreaPublic {
			lookup[f.Source] = f.Blob
		}
	}
	replace := func(blob string) string {
		if current, ok := lookup[blob]; ok {
			return current
		}
		return blob
	}
	for i := range p.View.Files {
		f := &p.View.Files[i]
		priorSource := f.Key()
		f.Blob = replace(f.Blob)
		if f.Track != nil {
			f.Track.Index = replace(f.Track.Index)
		}
		for j := range f.Public {
			// ServeOriginals can remap the upload itself to identical served
			// bytes; its verified rendition ownership follows that reference.
			if f.Public[j].Source == priorSource {
				f.Public[j].Source = f.Key()
			}
			for n := range f.Public[j].Blobs {
				f.Public[j].Blobs[n] = replace(f.Public[j].Blobs[n])
			}
		}
	}
	for i := range p.Pages.Pages {
		for j := range p.Pages.Pages[i].Files {
			p.Pages.Pages[i].Files[j].Blob = replace(p.Pages.Pages[i].Files[j].Blob)
		}
	}
	for i := range p.Playback.Tracks {
		p.Playback.Tracks[i].File.Blob = replace(p.Playback.Tracks[i].File.Blob)
	}
	committed := *delivery
	committed.Files = slices.DeleteFunc(slices.Clone(delivery.Files), func(f media.DeliveryFile) bool {
		area, _, _ := layout.SplitBlobRef(f.Blob)
		return area != layout.AreaPrivate && area != layout.AreaPublic
	})
	// Immutable HLS indexes still name the preparation's original references.
	// Carry their aliases forward only when this delivery explicitly owns the
	// previous served destination; withdrawn or unrelated bytes gain no alias.
	for _, previous := range p.View.Deliveries {
		if !previous.Ready || previous.Withdrawn {
			continue
		}
		for _, file := range previous.Files {
			blob, copied := lookup[file.Blob]
			if !copied || slices.ContainsFunc(committed.Files, func(f media.DeliveryFile) bool { return f.Source == file.Source }) {
				continue
			}
			file.Blob = blob
			committed.Files = append(committed.Files, file)
		}
	}
	p.View.Deliveries = []media.PublicationDelivery{committed}
}

func (s *Service) runRelease(ctx context.Context, id string) (resultErr error) {
	var t Target
	err := s.o.Pool.QueryRow(ctx, `SELECT content_id,version_id FROM `+s.publicationRequestsTable()+` WHERE tenant_id=$1 AND id=$2::uuid`, s.o.Tenant, id).Scan(&t.Content, &t.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	unlock, err := s.lockSource(ctx, t)
	if err != nil {
		return err
	}
	defer unlock()
	// A newer intent cannot declare the item settled while an older active
	// swap still has served objects awaiting retirement. Continue those exact
	// obligations under the same item owner before preparing another move.
	rows, err := s.o.Pool.Query(ctx, `SELECT id::text,result FROM `+s.publicationRequestsTable()+`
 WHERE tenant_id=$1 AND version_id=$2 AND state='retiring' AND id<>$3::uuid ORDER BY created_at,id`, s.o.Tenant, t.Version, id)
	if err != nil {
		return err
	}
	type retirement struct {
		ID     string
		Result []byte
	}
	prior, err := pgx.CollectRows(rows, pgx.RowToStructByPos[retirement])
	if err != nil {
		return err
	}
	for _, pending := range prior {
		var plan releasePlan
		if err := json.Unmarshal(pending.Result, &plan); err != nil {
			return err
		}
		if err := s.finishRetirement(ctx, pending.ID, &plan); err != nil {
			return err
		}
	}
	v, err := s.one(ctx, t)
	if err != nil {
		return err
	}
	ref, err := s.ref(v.Kind, t)
	if err != nil {
		return err
	}
	mutationCtx := context.WithValue(ctx, releaseMutationContextKey{}, true)
	if _, _, err := s.m.Manifests.Get(ctx, ref); errors.Is(err, media.ErrNotFound) {
		if _, err := s.m.Manifests.Edit(mutationCtx, ref, func(*media.Manifest) error { return nil }); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil && ctx.Err() == nil {
			_ = pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
				if err := s.lockTarget(ctx, tx, t); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `UPDATE `+s.publicationRequestsTable()+` SET state=CASE WHEN state='retiring' THEN state ELSE 'failed' END,error=$3,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2::uuid AND (current OR state='retiring') AND state<>'completed'`, s.o.Tenant, id, resultErr.Error())
				return err
			})
		}
	}()
	var plan *releasePlan
	current, retiring := false, false
	err = s.m.Manifests.WithRecoveredLock(mutationCtx, ref, func() error {
		return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
			if err := s.lockTarget(ctx, tx, t); err != nil {
				return err
			}
			var raw []byte
			var due time.Time
			var state string
			if err := tx.QueryRow(ctx, `SELECT current,state,scheduled_at,CASE WHEN state='retiring' THEN result ELSE plan END FROM `+s.publicationRequestsTable()+` WHERE tenant_id=$1 AND id=$2::uuid FOR UPDATE`, s.o.Tenant, id).Scan(&current, &state, &due, &raw); err != nil {
				return err
			}
			if state == "retiring" {
				retiring = true
				return json.Unmarshal(raw, &plan)
			}
			if !current || state == "completed" || state == "cancelled" {
				current = false
				return nil
			}
			if due.After(s.o.Now()) {
				return fmt.Errorf("%w: publication is not due", ErrNotReady)
			}
			if len(raw) > 0 && string(raw) != "null" {
				return json.Unmarshal(raw, &plan)
			}
			var err error
			plan, err = s.planRelease(ctx, tx, t)
			if err != nil {
				return err
			}
			active, err := s.activeRelease(ctx, tx, t)
			if err != nil {
				return err
			}
			if active != nil && active.Fingerprint == plan.Fingerprint && plan.storageSettled(active) {
				active.Waiting, active.NextAt = plan.Waiting, plan.NextAt
				body, err := json.Marshal(active)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE `+s.publicationRequestsTable()+` SET state='completed',result=$3,completed_at=$4,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2::uuid`, s.o.Tenant, id, body, s.o.Now()); err != nil {
					return err
				}
				current = false
				if plan.NextAt != nil {
					return s.scheduleReleaseAtTx(ctx, tx, t, *plan.NextAt)
				}
				return nil
			}
			if plan.Waiting && !plan.Released && len(plan.Copies) == 0 {
				_, err := tx.Exec(ctx, `UPDATE `+s.publicationRequestsTable()+` SET state='waiting',error='',updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2::uuid`, s.o.Tenant, id)
				current = false
				if err == nil && plan.NextAt != nil {
					err = s.scheduleReleaseAtTx(ctx, tx, t, *plan.NextAt)
				}
				return err
			}
			raw, err = json.Marshal(plan)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE `+s.publicationRequestsTable()+` SET plan=$3,state='running',error='',updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2::uuid`, s.o.Tenant, id, raw)
			return err
		})
	})
	if err != nil {
		return err
	}
	if retiring {
		return s.finishRetirement(ctx, id, plan)
	}
	if !current {
		return nil
	}
	if _, err := s.m.Manifests.PreparePromotion(mutationCtx, ref, id, plan.Copies); err != nil {
		return err
	}
	delivery, err := s.m.Manifests.Promote(mutationCtx, ref, id)
	if err != nil {
		return err
	}
	plan.applyDelivery(delivery)
	err = s.m.Manifests.WithRecoveredLock(mutationCtx, ref, func() error {
		return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
			if err := s.lockTarget(ctx, tx, t); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT current FROM `+s.publicationRequestsTable()+` WHERE tenant_id=$1 AND id=$2::uuid FOR UPDATE`, s.o.Tenant, id).Scan(&current); err != nil {
				return err
			}
			if !current {
				return nil
			}
			latest, err := s.planRelease(ctx, tx, t)
			if err != nil {
				return err
			}
			if latest.Fingerprint != plan.Fingerprint {
				current = false
				return s.scheduleReleaseAtTx(ctx, tx, t, s.o.Now())
			}
			k, err := s.reg.Kind(plan.Kind)
			if err != nil {
				return err
			}
			for _, snapshot := range []struct {
				kind  string
				value any
			}{{kindPages, plan.Pages}, {kindPlayback, plan.Playback}} {
				pubID, same, err := s.prepareTx(ctx, tx, k, t, snapshot.kind, snapshot.value)
				if err != nil {
					return err
				}
				if !same {
					if _, err := s.promoteTx(ctx, tx, k, t, snapshot.kind, pubID); err != nil {
						return err
					}
				}
			}
			at := s.o.Now()
			if len(plan.Chapters) > 0 {
				if _, err := tx.Exec(ctx, `UPDATE `+s.t.segments+` SET released_at=coalesce(released_at,$4),release_id=$5::uuid WHERE tenant_id=$1 AND version_id=$2 AND id=ANY($3)`, s.o.Tenant, t.Version, plan.Chapters, at, id); err != nil {
					return err
				}
			}
			body, err := json.Marshal(plan)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE `+s.publicationRequestsTable()+` SET result=$3,state='retiring',error='',updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2::uuid`, s.o.Tenant, id, body); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` SET release_id=$3::uuid,released_access=$4 WHERE tenant_id=$1 AND id=$2`, s.o.Tenant, t.Version, id, plan.EffectiveAccess); err != nil {
				return err
			}
			completion, ok := s.o.Facets.(publicationCompletion)
			if !ok {
				return errors.New("versions: catalog publication completion is not configured")
			}
			if err := s.recordFacts(ctx, tx, k, t, plan.View); err != nil {
				return err
			}
			if plan.Retained {
				if err := completion.WithdrawPublicationTx(ctx, tx, t, true); err != nil {
					return err
				}
			} else if plan.Released {
				if err := completion.CompletePublicationTx(ctx, tx, t, at); err != nil {
					return err
				}
			} else if err := completion.WithdrawPublicationTx(ctx, tx, t, false); err != nil {
				return err
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	if !current {
		// The accepted intent changed while storage was copying. Its newer
		// request owns reconciliation; never activate this obsolete view.
		return nil
	}
	return s.finishRetirement(ctx, id, plan)
}

// finishRetirement is still the publication worker. The new verified view is
// active while its superseded allocations are retired. A failure retains this
// phase and retries it without recopying or breaking the active reader view.
func (s *Service) finishRetirement(ctx context.Context, id string, plan *releasePlan) error {
	ref, err := s.ref(plan.Kind, plan.Target)
	if err != nil {
		return err
	}
	mutationCtx := context.WithValue(ctx, releaseMutationContextKey{}, true)
	for _, old := range plan.Previous {
		if old != id {
			if err := s.m.Manifests.WithdrawPromotion(mutationCtx, ref, old); err != nil {
				return err
			}
		}
	}
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if err := s.lockTarget(ctx, tx, plan.Target); err != nil {
			return err
		}
		var current bool
		var state string
		if err := tx.QueryRow(ctx, `SELECT current,state FROM `+s.publicationRequestsTable()+` WHERE tenant_id=$1 AND id=$2::uuid FOR UPDATE`, s.o.Tenant, id).Scan(&current, &state); err != nil {
			return err
		}
		if state == "completed" {
			return nil
		}
		if state != "retiring" {
			return errRetired
		}
		at := s.o.Now()
		if _, err := tx.Exec(ctx, `UPDATE `+s.publicationRequestsTable()+` SET state='completed',completed_at=$3,error=NULL,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2::uuid`, s.o.Tenant, id, at); err != nil {
			return err
		}
		completion, ok := s.o.Facets.(publicationCompletion)
		if !ok {
			return errors.New("versions: catalog publication completion is not configured")
		}
		if err := completion.PublicationCompletedTx(ctx, tx, plan.Target, id, plan.Chapters, plan.Released, at); err != nil {
			return err
		}
		// The automatic cover is selected only from the now-active pages.
		// Its normal metadata/image work is idempotent and cannot select a
		// future chapter merely because its preparation has finished.
		if err := s.enqueueTx(ctx, tx, publishArgs{Content: plan.Content, Version: plan.Version}); err != nil {
			return err
		}
		if current && plan.NextAt != nil {
			return s.scheduleReleaseAtTx(ctx, tx, plan.Target, *plan.NextAt)
		}
		return nil
	})
}
