package versions

import (
	"context"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
)

// inspectionPlan is an in-memory administrative view, never a publication.
// It lists only prepared reader derivatives that the same explicit inspection
// media request will authorize. Declared reader audio can pass through, but
// this view cannot obtain other source exports or download-only renditions.
func (s *Service) inspectionPlan(ctx context.Context, actor access.Actor, k *media.RegisteredKind, target Target) (*ReaderPlan, error) {
	if s.m.Reader == nil {
		return nil, ErrNotReady
	}
	ref, err := s.ref(k.Name, target)
	if err != nil {
		return nil, err
	}
	grant, err := s.m.Reader.Grant(resolve.WithEditorRead(ctx), ref, actor)
	if err != nil {
		return nil, err
	}
	if !grant.Resolution.ReadRestricted || grant.Editor() {
		return nil, ErrForbidden
	}
	pages, err := pagesOf(k, grant.Manifest)
	if err != nil {
		return nil, err
	}
	groups, err := s.segments(ctx, s.o.Pool, []string{target.Version})
	if err != nil {
		return nil, err
	}
	chapters, err := resolveChapterStarts(groups[target.Version], pages)
	if err != nil {
		return nil, err
	}
	active, err := s.activeRelease(ctx, s.o.Pool, target)
	if err != nil {
		return nil, err
	}
	byPosition := map[int]segment{}
	for _, chapter := range chapters {
		byPosition[chapter.Position] = chapter
	}
	count := len(pages)
	plan := &ReaderPlan{Content: target.Content, Version: target.Version, Pages: &count, Segments: []PlanSegment{}, Previews: []PlanPreview{}, Expires: grant.Expires.Unix()}
	item := &media.RefBody{Kind: ref.ContentKind, ID: ref.ContentID, Version: ref.Version()}
	readable := map[string]bool{}
	for _, span := range spans(chapters, len(pages)) {
		part := PlanSegment{Position: span.Segment, Start: span.Start, End: span.End, Inspection: true, Item: item}
		if span.Segment != nil {
			chapter := byPosition[*span.Segment]
			part.Title, part.ReleaseAt = chapter.Title, chapter.ReleaseAt
			part.Released = active != nil && active.Released && chapter.ReleasedAt != nil
		} else {
			part.Released = active != nil && active.Released
		}
		for _, source := range pages[span.Start:span.End] {
			page := PlanPage{ID: sourcePageHash(source), Name: source.Name, Span: source.Span, Files: map[string]string{}}
			if page.Name == "" {
				page.Name = k.NameOf(source.Path)
			}
			for _, rendition := range source.Files {
				file, exists := grant.Manifest.Get(rendition.Path)
				if !exists || file.Blob != rendition.Blob || !grant.Allowed(file) {
					continue
				}
				if page.W == 0 {
					page.W, page.H = rendition.W, rendition.H
				}
				page.Files[rendition.Preset] = rendition.Path
			}
			if len(page.Files) == 0 {
				return nil, ErrNotReady
			}
			readable[page.ID] = true
			part.Pages = append(part.Pages, page)
		}
		plan.Segments = append(plan.Segments, part)
	}
	playable := map[string]bool{}
	if k.Publish.Playback != "" {
		tracks, err := tracksOf(k, grant.Manifest)
		if err != nil {
			return nil, err
		}
		playback := &PlanPlayback{Item: *item, Inspection: true, Tracks: []PlanTrack{}}
		for _, track := range tracks {
			file, exists := grant.Manifest.Get(track.File.Path)
			if !exists || file.Blob != track.File.Blob || !grant.Allowed(file) {
				continue
			}
			playback.Tracks = append(playback.Tracks, PlanTrack{Name: track.Name, Path: track.File.Path, Duration: track.File.Dur})
			playable[track.Name] = true
		}
		if len(playback.Tracks) > 0 {
			plan.Playback = playback
		}
	}
	if metadata, ok := readerMetaOf(grant.Manifest); ok {
		filtered := filterReaderMeta(hashReaderPages(metadata, pages), readable, playable)
		plan.Reader = &filtered
	}
	return plan, nil
}
