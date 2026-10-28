package versionstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	versions "github.com/open-rails/contentkit/media/versions"
)

// pagesSnapshot is a version's pages as a publication freezes them.
type pagesSnapshot struct {
	Pages    []page    `json:"pages"`
	Segments []segment `json:"segments,omitempty"`
}

type page struct {
	Name string `json:"name,omitempty"`
	// The stored Source already includes the original blob digest. Keep this
	// transient value out of snapshot JSON so existing publications stay valid.
	SourceHash string             `json:"-"`
	Span       *versions.PageSpan `json:"span,omitempty"`
	Path       string             `json:"path"`   // the page upload
	Source     string             `json:"source"` // its blob and edit (File.Key)
	Files      []rendition        `json:"files"`  // Publish.Renditions, in order
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

func private(k *mediastore.RegisteredKind, name string) *media.Private {
	for i := range k.Private {
		if k.Private[i].Name == name {
			return &k.Private[i]
		}
	}
	return nil
}

func public(k *mediastore.RegisteredKind, name string) *media.Public {
	for i := range k.Public {
		if k.Public[i].Name == name {
			return &k.Public[i]
		}
	}
	return nil
}

// imports are the attached archives Publish.Import has yet to expand.
func imports(k *mediastore.RegisteredKind, m *mediastore.Manifest) []media.File {
	var out []media.File
	for _, f := range m.Files {
		if k.Publish.Import != "" && f.IsUpload() && !f.Unattached && f.Fail() == nil && (f.Meta["archive_completed_sha256"] == nil || f.Meta["archive_completed_sha256"] != pageDigest(f)) && inPattern(k.Publish.Import, f.Path) {
			out = append(out, f)
		}
	}
	return out
}

// pagesOf is m's pages ready to publish.
func pagesOf(k *mediastore.RegisteredKind, m *mediastore.Manifest) ([]page, error) {
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
		if pageMeta(f).SchemaVersion > 0 && strings.HasPrefix(f.Type, "image/") && !f.Gone && f.Fail() == nil && mediastore.PageAnalysisFits(f.Meta) && k.PageAnalysisCapacity(m) > 0 && !f.HasPageAnalysis(media.PageAnalysisRecipe) {
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
func tracksOf(k *mediastore.RegisteredKind, m *mediastore.Manifest) ([]track, error) {
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
func (s *Service) lockSource(ctx context.Context, v versions.Target) (func(), error) {
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

// sameBlob reports two allocations of the same bytes.
func sameBlob(a, b string) bool {
	da, okA := layout.BlobDigest(a)
	db, okB := layout.BlobDigest(b)
	return a == b || okA && okB && bytes.Equal(da, db)
}
