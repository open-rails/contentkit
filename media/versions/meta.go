package versions

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
)

// readerMeta is the manifest meta key of a version's ReaderMeta.
const readerMeta = "reader"

// ReaderMeta is a version's reader metadata, kept in its item's manifest:
// scroll anchors on its pages and audio cues on pages or anchors. Pages use
// original-image hashes; audio files retain their upload names.
type ReaderMeta struct {
	SchemaVersion int             `json:"schema_version"` // 1
	Defaults      *ReaderDefaults `json:"defaults,omitempty"`
	Anchors       []Anchor        `json:"anchors,omitempty"`
	Background    []BackgroundCue `json:"background,omitempty"`
	Foreground    []ForegroundCue `json:"foreground,omitempty"`
}

type ReaderDefaults struct {
	AutoAdvanceSeconds float64 `json:"auto_advance_seconds,omitempty"`
}

// Anchor is a scroll position within a page of the vertical reader.
type Anchor struct {
	ID              string   `json:"id"`
	Page            string   `json:"page"`
	OffsetPercent   float64  `json:"offset_percent"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
}

// BackgroundCue plays a track over a range of pages or anchors.
type BackgroundCue struct {
	File       string  `json:"file"`
	FromPage   *string `json:"from_page,omitempty"`
	ToPage     *string `json:"to_page,omitempty"`
	FromAnchor *string `json:"from_anchor,omitempty"`
	ToAnchor   *string `json:"to_anchor,omitempty"`
	Loop       *bool   `json:"loop,omitempty"`
	Layer      *string `json:"layer,omitempty"`
}

// ForegroundCue plays a whole track at a page or anchor.
type ForegroundCue struct {
	File   string  `json:"file"`
	Page   *string `json:"page,omitempty"`
	Anchor *string `json:"anchor,omitempty"`
}

func readerMetaOf(m *media.Manifest) (ReaderMeta, bool) {
	var rm ReaderMeta
	raw, ok := m.Meta[readerMeta]
	if !ok {
		return rm, false
	}
	b, err := json.Marshal(raw)
	return rm, err == nil && json.Unmarshal(b, &rm) == nil && rm.SchemaVersion == 1
}

// filter keeps what names pages and files that exist: anchors on pages,
// cues on files and pages or anchors kept.
func (rm ReaderMeta) filter(pages, files map[string]bool) ReaderMeta {
	out := rm
	out.Anchors = slices.DeleteFunc(slices.Clone(rm.Anchors), func(a Anchor) bool { return !pages[a.Page] })
	anchors := map[string]bool{}
	for _, a := range out.Anchors {
		anchors[a.ID] = true
	}
	page := func(p *string) bool { return p == nil || pages[*p] }
	anchor := func(a *string) bool { return a == nil || anchors[*a] }
	out.Background = slices.DeleteFunc(slices.Clone(rm.Background), func(c BackgroundCue) bool {
		return !files[c.File] || !page(c.FromPage) || !page(c.ToPage) || !anchor(c.FromAnchor) || !anchor(c.ToAnchor)
	})
	out.Foreground = slices.DeleteFunc(slices.Clone(rm.Foreground), func(c ForegroundCue) bool {
		return !files[c.File] || !page(c.Page) || !anchor(c.Anchor)
	})
	return out
}

// hashPages upgrades old stored page-name references to their unique original
// hash in this publication. It never accepts names on new writes or changes
// stored source metadata; the next explicit save writes only canonical hashes.
func (rm ReaderMeta) hashPages(source []page) ReaderMeta {
	counts := map[string]int{}
	for _, page := range source {
		counts[sourcePageHash(page)]++
	}
	aliases := map[string]string{}
	for hash, count := range counts {
		if hash != "" && count == 1 {
			aliases[hash] = hash
		} else {
			aliases[hash] = ""
		}
	}
	for _, page := range source {
		name := page.Name
		if name == "" {
			name = strings.TrimSuffix(path.Base(page.Path), path.Ext(page.Path))
		}
		if name == "" || counts[name] > 0 {
			continue
		}
		hash := sourcePageHash(page)
		if hash == "" || counts[hash] != 1 {
			aliases[name] = ""
			continue
		}
		if previous, exists := aliases[name]; exists && previous != hash {
			aliases[name] = ""
		} else {
			aliases[name] = hash
		}
	}
	lookup := func(value string) string {
		if hash, exists := aliases[value]; exists {
			return hash
		}
		return value
	}
	pointer := func(value *string) *string {
		if value == nil {
			return nil
		}
		hash := lookup(*value)
		return &hash
	}
	out := rm
	out.Anchors = slices.Clone(rm.Anchors)
	for i := range out.Anchors {
		out.Anchors[i].Page = lookup(out.Anchors[i].Page)
	}
	out.Background = slices.Clone(rm.Background)
	for i := range out.Background {
		out.Background[i].FromPage = pointer(out.Background[i].FromPage)
		out.Background[i].ToPage = pointer(out.Background[i].ToPage)
	}
	out.Foreground = slices.Clone(rm.Foreground)
	for i := range out.Foreground {
		out.Foreground[i].Page = pointer(out.Foreground[i].Page)
	}
	return out
}

// SetReaderMeta stores v's reader metadata for an editor: every page,
// anchor and file it names must exist.
func (s *Service) SetReaderMeta(ctx context.Context, actor access.Actor, t Target, rm ReaderMeta) error {
	v, err := s.one(ctx, t)
	if err != nil {
		return err
	}
	k, err := s.published(v)
	if err != nil {
		return err
	}
	res, err := s.decide(ctx, actor, v.Kind, []Target{t})
	if err != nil {
		return err
	}
	if !res[t].Editor {
		if res[t].Visible {
			return ErrForbidden
		}
		return ErrNotFound
	}
	if rm.SchemaVersion != 1 {
		return fmt.Errorf("%w: schema_version must be 1", ErrInvalid)
	}
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return err
	}
	_, err = s.m.Manifests.Edit(ctx, ref, func(m *media.Manifest) error {
		pages, files := names(k, m)
		if kept := rm.filter(pages, files); len(kept.Anchors) != len(rm.Anchors) || len(kept.Background) != len(rm.Background) ||
			len(kept.Foreground) != len(rm.Foreground) {
			return fmt.Errorf("%w: the reader metadata names pages, anchors or audio the version does not have", ErrInvalid)
		}
		ids := map[string]bool{}
		for _, a := range rm.Anchors {
			if a.ID == "" || ids[a.ID] || a.OffsetPercent < 0 || a.OffsetPercent > 100 {
				return fmt.Errorf("%w: anchor %q", ErrInvalid, a.ID)
			}
			ids[a.ID] = true
		}
		if m.Meta == nil {
			m.Meta = map[string]any{}
		}
		m.Meta[readerMeta] = rm
		return nil
	})
	return err
}

// names are the {name}s of m's attached pages and audio uploads.
func names(k *media.Kind, m *media.Manifest) (pages, files map[string]bool) {
	pages, files = map[string]bool{}, map[string]bool{}
	duplicates := map[string]bool{}
	var audio string
	if k.Publish.Playback != "" {
		audio = private(k, k.Publish.Playback).From
	}
	for _, f := range m.Files {
		switch {
		case !f.IsUpload() || f.Unattached:
		case inPattern(k.Publish.Pages, f.Path):
			if !media.PageIncluded(f) {
				continue
			}
			id := pageID(k, f)
			if id == "" || pages[id] {
				duplicates[id] = true
			} else {
				pages[id] = true
			}
		case audio != "" && inPattern(audio, f.Path):
			files[k.NameOf(f.Path)] = true
		}
	}
	for id := range duplicates {
		delete(pages, id)
	}
	return pages, files
}
