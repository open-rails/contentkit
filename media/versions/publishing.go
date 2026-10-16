package versions

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// ArchiveOperation and ArchiveSHA256 bind an uploaded ZIP to a publishing
// operation. The import checks the placed blob's actual SHA-256 before opening
// it. Ordinary uploads that do not declare these fields retain their behavior.
const (
	ArchiveOperation   = "publishing_operation"
	ArchiveSHA256      = "publishing_sha256"
	archiveReceipts    = "publishing_archives"
	archiveCoverErrors = "publishing_cover_errors"
)

type archiveCoverFailure struct {
	SHA256 string `json:"sha256"`
	Code   string `json:"code"`
}

type archivePage struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type archiveReceipt struct {
	SHA256 string        `json:"sha256"`
	Pages  []archivePage `json:"pages"`
}

// ArchiveReadiness is the progress of the exact imported archive, not merely
// any media that happens to occupy its version.
type ArchiveReadiness struct {
	Pages     int
	Completed int
	Ready     bool
	// PublicReady additionally waits for exposure and public cover renditions.
	// A hidden draft prepares the source cover but has no public renditions yet.
	PublicReady bool
}

var (
	ErrArchiveChanged = errors.New("versions: imported archive pages were changed")
	ErrArchiveFailed  = errors.New("versions: archive or a required rendition failed")
)

func pageDigest(f media.File) string {
	if sum, ok := layout.BlobDigest(f.Blob); ok {
		return hex.EncodeToString(sum)
	}
	s, _ := f.Meta[digestMeta].(string)
	return s
}

func archivePages(k *media.Kind, m *media.Manifest) []archivePage {
	var out []archivePage
	for _, f := range m.Files {
		if f.IsUpload() && !f.Unattached && inPattern(k.Publish.Pages, f.Path) {
			out = append(out, archivePage{f.Path, pageDigest(f)})
		}
	}
	return out
}

// WithArchiveReady serializes validation and fn with the same version source
// lock as publication builds. It does not hold a database transaction while
// calling fn. fn must itself compare the catalog revisions before publishing.
func (s *Service) WithArchiveReady(ctx context.Context, t Target, operation, digest string, fn func(ArchiveReadiness) error) error {
	unlock, err := s.lockSource(ctx, t)
	if err != nil {
		return err
	}
	defer unlock()
	v, err := s.one(ctx, t)
	if err != nil {
		return err
	}
	ref, err := s.ref(v.Kind, t)
	if err != nil {
		return err
	}
	return s.m.Manifests.WithLock(ctx, ref, func() error {
		r, err := s.ArchiveStatus(ctx, t, operation, digest)
		if err != nil {
			return err
		}
		if !r.Ready {
			return ErrNotReady
		}
		return fn(r)
	})
}

// ArchiveStatus reads exact archive progress and required publication/cover
// readiness. It performs no mutation and does not assume that a past completed
// job means that the current pages still match the submitted archive.
func (s *Service) ArchiveStatus(ctx context.Context, t Target, operation, digest string) (ArchiveReadiness, error) {
	var out ArchiveReadiness
	v, err := s.one(ctx, t)
	if err != nil {
		return out, err
	}
	k, err := s.published(v)
	if err != nil {
		return out, err
	}
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return out, err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if m.Deleted {
		return out, ErrNotFound
	}
	for _, f := range m.Files {
		if f.IsUpload() && f.Fail() != nil {
			return out, ErrArchiveFailed
		}
	}
	if len(imports(k, m)) > 0 {
		return out, nil
	}
	var receipts map[string]archiveReceipt
	b, _ := json.Marshal(m.Meta[archiveReceipts])
	if err = json.Unmarshal(b, &receipts); err != nil {
		return out, fmt.Errorf("%w: missing archive receipt", ErrArchiveChanged)
	}
	receipt, ok := receipts[operation]
	if !ok || receipt.SHA256 != digest {
		return out, ErrArchiveChanged
	}
	current := archivePages(k, m)
	out.Pages = len(receipt.Pages)
	if len(current) != len(receipt.Pages) || len(current) == 0 {
		return out, ErrArchiveChanged
	}
	for i, p := range current {
		if p != receipt.Pages[i] || p.SHA256 == "" {
			return out, ErrArchiveChanged
		}
	}
	for _, f := range m.Files {
		if f.IsUpload() && !f.Unattached && inPattern(k.Publish.Pages, f.Path) && f.Blob != "" && len(f.Pending) == 0 {
			out.Completed++
		}
	}
	pages, err := pagesOf(k, m)
	if errors.Is(err, ErrNotReady) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	p, err := s.current(ctx, s.o.Pool, t, kindPages)
	if err != nil {
		return out, err
	}
	if p == nil {
		return out, nil
	}
	var snap pagesSnapshot
	if err = json.Unmarshal(p.Source, &snap); err != nil {
		return out, err
	}
	// Publication snapshots may include segment spans as well as the pages.
	if !sameJSON(snap.Pages, pages) {
		return out, nil
	}
	cover := public(k, media.CoverPreset)
	publicReady := !m.Hidden
	if cover != nil {
		ready := false
		coverPublic := false
		for _, f := range m.Files {
			if f.IsUpload() && !f.Unattached && coverPath(cover.From, f.Path) {
				if f.Blob != "" && f.W > 0 && f.H > 0 && len(f.Pending) == 0 {
					ready = true
				}
				pub, ok := f.Current(media.CoverPreset)
				if ok && pub.Ready() && len(pub.Names) > 0 {
					coverPublic = true
				}
			}
		}
		if !ready {
			if _, fallback := s.reg.DefaultImage(ref, media.CoverPreset); fallback {
				// A configured default is the same fallback Covers returns when
				// this version cannot supply an automatic cover of its own.
				coverPublic = true
			} else {
				var failed map[string]archiveCoverFailure
				b, _ := json.Marshal(m.Meta[archiveCoverErrors])
				if json.Unmarshal(b, &failed) == nil && failed[operation].SHA256 == digest {
					return out, fmt.Errorf("%w: automatic cover: %s", ErrArchiveFailed, failed[operation].Code)
				}
				return out, nil
			}
		}
		publicReady = publicReady && coverPublic
	}
	out.Completed = out.Pages
	out.Ready = true
	out.PublicReady = publicReady
	return out, nil
}

// failArchiveCover records a permanent cover failure only for publishing
// receipts that still describe these exact pages. Ordinary uploads keep their
// existing best-effort cover behavior, and a repaired cover takes precedence
// over this diagnostic on subsequent readiness reads.
func (s *Service) failArchiveCover(ctx context.Context, k *media.Kind, t Target, code string) error {
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return err
	}
	if _, fallback := s.reg.DefaultImage(ref, media.CoverPreset); fallback {
		return nil
	}
	_, err = s.m.Manifests.EditExisting(ctx, ref, func(m *media.Manifest) error {
		var receipts map[string]archiveReceipt
		b, _ := json.Marshal(m.Meta[archiveReceipts])
		if json.Unmarshal(b, &receipts) != nil || len(receipts) == 0 {
			return nil
		}
		var failed map[string]archiveCoverFailure
		b, _ = json.Marshal(m.Meta[archiveCoverErrors])
		_ = json.Unmarshal(b, &failed)
		if failed == nil {
			failed = map[string]archiveCoverFailure{}
		}
		pages := archivePages(k, m)
		for operation, receipt := range receipts {
			if receipt.SHA256 != "" && len(receipt.Pages) > 0 && sameJSON(receipt.Pages, pages) {
				failed[operation] = archiveCoverFailure{SHA256: receipt.SHA256, Code: code}
			}
		}
		if len(failed) != 0 {
			m.Meta[archiveCoverErrors] = failed
		}
		return nil
	})
	return err
}
