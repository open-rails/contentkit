package versions

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Archive import limits: images per archive and expanded bytes.
const (
	maxImportPages    = 2000
	maxImportExpanded = 2 << 30
)

// digestMeta records an imported page's SHA-256, so a repeated run skips it
// before the worker places it.
const digestMeta = "sha256"

// permanent is an archive no retry can expand.
type permanent struct{ error }

// expand ingests v's attached archives (Publish.Import), in manifest order,
// as pages appended in the entries' natural order, retaining each source ZIP.
// Pages already there (a repeated run) are skipped; an archive that cannot be
// expanded is marked failed.
func (s *Service) expand(ctx context.Context, t Target) error {
	v, err := s.one(ctx, t)
	if err != nil {
		return err
	}
	k, err := s.published(v)
	if err != nil || k.Publish.Import == "" {
		return err
	}
	unlock, err := media.PGLocker(s.o.Pool).Lock(ctx, "contentkit:versions:import:"+s.o.Tenant+":"+t.Version)
	if err != nil {
		return err
	}
	defer unlock()
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	for _, f := range imports(k, m) {
		if f.Blob == "" {
			return ErrNotReady
		}
		err := s.importArchive(ctx, k, ref, f)
		var p permanent
		if errors.As(err, &p) {
			s.o.Logger.WarnContext(ctx, "versions: archive import failed", "ref", ref.String(), "path", f.Path, "error", p.error)
			if _, err := s.m.Manifests.EditExisting(ctx, ref, func(m *media.Manifest) error {
				if i := m.Find(f.Path); i >= 0 && m.Files[i].Key() == f.Key() {
					m.Files[i].Failed = media.NewFailure(m.Files[i], p.error)
				}
				return nil
			}); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}
		meta := maps.Clone(f.Meta)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["archive_completed_sha256"] = pageDigest(f)
		if _, err := s.m.Uploads.CommitChecked(ctx, system, ref, uuid.NewString(), []media.Op{{Op: media.OpPut, Path: f.Path, Blob: f.Blob, Edit: f.Edit, Meta: meta}}, func(m *media.Manifest) error {
			i := m.Find(f.Path)
			if i < 0 || m.Files[i].Key() != f.Key() {
				return ErrArchiveChanged
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) importArchive(ctx context.Context, k *media.Kind, ref contentref.ContentRef, f media.File) error {
	operation, _ := f.Meta[ArchiveOperation].(string)
	expected, _ := f.Meta[ArchiveSHA256].(string)
	if operation != "" {
		digest, ok := layout.BlobDigest(f.Blob)
		if !ok || hex.EncodeToString(digest) != expected {
			return permanent{errors.New("archive checksum does not match its publishing declaration")}
		}
	}
	item, err := s.reg.Item(ref)
	if err != nil {
		return err
	}
	key, err := item.Blob(f.Blob)
	if err != nil {
		return permanent{err}
	}
	tmp, err := s.download(ctx, key)
	if errors.Is(err, media.ErrNotFound) {
		return permanent{err}
	} else if err != nil {
		return err
	}
	defer os.Remove(tmp)
	zr, err := zip.OpenReader(tmp)
	if err != nil {
		return permanent{fmt.Errorf("not a zip archive: %w", err)}
	}
	defer zr.Close()
	var entries []*zip.File
	var expanded uint64
	for _, e := range zr.File {
		base := path.Base(e.Name)
		if e.FileInfo().IsDir() || strings.HasPrefix(base, ".") || strings.Contains(e.Name, "__MACOSX/") || !imageName(base) {
			continue
		}
		entries = append(entries, e)
		expanded += e.UncompressedSize64
	}
	switch {
	case len(entries) == 0:
		return permanent{errors.New("the archive holds no images")}
	case len(entries) > maxImportPages:
		return permanent{fmt.Errorf("the archive holds %d images; at most %d", len(entries), maxImportPages)}
	case expanded > maxImportExpanded:
		return permanent{fmt.Errorf("the archive expands to %d bytes; at most %d", expanded, maxImportExpanded)}
	}
	uploaded := make(map[*zip.File]int, len(entries))
	for i, e := range entries {
		uploaded[e] = i + 1
	}
	slices.SortStableFunc(entries, func(a, b *zip.File) int {
		if c := naturalCompare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	sourceNames := make([]string, len(entries))
	for i, entry := range entries {
		sourceNames[i] = path.Base(entry.Name)
	}
	cleanedNames := media.CleanPageBatchNames(sourceNames)
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if err != nil {
		return err
	}
	dir := strings.TrimSuffix(k.Publish.Pages, "{name}")
	names := map[string]bool{}
	archiveHash := pageDigest(f)
	if archiveHash == "" {
		archiveHash = expected
	}
	existing := map[int]media.File{}
	for _, f := range m.Files {
		if f.IsUpload() && inPattern(k.Publish.Pages, f.Path) {
			name := pageName(k, f)
			names[name] = true
			names[k.NameOf(f.Path)] = true
			pm := pageMeta(f)
			if pm.ArchiveSHA256 == archiveHash {
				existing[pm.SourceIndex] = f
			}
		}
	}
	up := k.Uploads[slices.IndexFunc(k.Uploads, func(u media.Upload) bool { return u.Path == k.Publish.Pages })]
	expectedPages := map[string]string{}
	for index, e := range entries {
		body, err := readEntry(e, up.MaxBytes)
		if err != nil {
			return err
		}
		typ := http.DetectContentType(body)
		if !slices.Contains(up.Types, typ) {
			return permanent{fmt.Errorf("%s: pages take no %s", e.Name, typ)}
		}
		raw := sha256.Sum256(body)
		sum := hex.EncodeToString(raw[:])
		if previous, ok := existing[index+1]; ok {
			if pageDigest(previous) != sum || pageMeta(previous).SourceEntry != e.Name {
				return permanent{ErrArchiveChanged}
			}
			expectedPages[previous.Path] = sum
			continue
		}
		name, span, flags := plannedPageName(cleanedNames[index], names)
		if cleanedNames[index] != sourceNames[index] {
			flags = append(flags, "source_name_cleaned")
		}
		pm := pageMetadata{SchemaVersion: 1, Name: name, SourceName: path.Base(e.Name), SourceEntry: e.Name, ArchiveSHA256: archiveHash, SourceIndex: index + 1, UploadIndex: uploaded[e], SourceSHA256: sum, Span: span, Flags: flags}
		metaFile := media.File{Meta: map[string]any{digestMeta: sum}}
		writePageMeta(&metaFile, pm)
		ext := strings.ToLower(path.Ext(e.Name))
		target := dir + name + ext
		expectedPages[target] = sum

		end := math.MaxInt
		if _, err := s.m.Uploads.Ingest(ctx, system, media.IngestRequest{Ref: ref, Path: target, Type: typ,
			Body: bytes.NewReader(body), Size: int64(len(body)), Meta: metaFile.Meta, Index: &end}); err != nil {
			if _, refused := media.AsUploadError(err); refused {
				return permanent{fmt.Errorf("%s: %w", e.Name, err)}
			}
			return err
		}
	}
	if operation != "" {
		_, err := s.m.Manifests.EditExisting(ctx, ref, func(m *media.Manifest) error {
			pages := archivePages(k, m)
			if len(pages) != len(expectedPages) {
				return permanent{ErrArchiveChanged}
			}
			for _, page := range pages {
				if expectedPages[page.Path] != page.SHA256 {
					return permanent{ErrArchiveChanged}
				}
			}
			var receipts map[string]archiveReceipt
			if existing := m.Meta[archiveReceipts]; existing != nil {
				b, err := json.Marshal(existing)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(b, &receipts); err != nil {
					return err
				}
			}
			if receipts == nil {
				receipts = map[string]archiveReceipt{}
			}
			receipts[operation] = archiveReceipt{SHA256: expected, Pages: pages}
			if m.Meta == nil {
				m.Meta = map[string]any{}
			}
			m.Meta[archiveReceipts] = receipts
			return nil
		})
		return err
	}
	return nil
}

func readEntry(e *zip.File, limit int64) ([]byte, error) {
	rc, err := e.Open()
	if err != nil {
		return nil, permanent{fmt.Errorf("%s: %w", e.Name, err)}
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, permanent{fmt.Errorf("%s: %w", e.Name, err)}
	}
	if int64(len(body)) > limit {
		return nil, permanent{fmt.Errorf("%s is over %d bytes", e.Name, limit)}
	}
	return body, nil
}

func imageName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".avif":
		return true
	}
	return false
}

// uniqueName keeps page names unique: "001.png" becomes "001-2.png".
func uniqueName(taken map[string]bool, name string) string {
	if !taken[name] {
		return name
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 2; ; n++ {
		if c := stem + "-" + strconv.Itoa(n) + ext; !taken[c] {
			return c
		}
	}
}

// naturalCompare orders "p2" before "p10".
func naturalCompare(a, b string) int {
	for a != "" && b != "" {
		ca, cb := a[0], b[0]
		if isDigit(ca) && isDigit(cb) {
			na, ra := digits(a)
			nb, rb := digits(b)
			if len(na) != len(nb) {
				return len(na) - len(nb)
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			a, b = ra, rb
			continue
		}
		if la, lb := lower(ca), lower(cb); la != lb {
			return int(la) - int(lb)
		}
		a, b = a[1:], b[1:]
	}
	return len(a) - len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func digits(s string) (string, string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return strings.TrimLeft(s[:i], "0"), s[i:]
}

// download copies an object to a temp file the caller removes.
func (s *Service) download(ctx context.Context, key string) (string, error) {
	rc, _, err := s.m.Manifests.Store().Get(ctx, key, media.GetOptions{})
	if err != nil {
		return "", err
	}
	defer rc.Close()
	f, err := os.CreateTemp("", "contentkit-import-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), f.Close()
}
