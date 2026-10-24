package media

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
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

const archiveImageLimit = 2000
const archiveExpandedLimit = int64(2 << 30)

var errArchiveChanged = errors.New("media: archive source changed during ingestion")

// IngestArchives expands the configured ZIP uploads of an item. Placement
// precedes this call; generated original files are attached in one conditional,
// journaled edit before image/video processing. No catalog or HTTP callback is
// required. The original ZIP remains in the manifest.
func (m *Manifests) IngestArchives(ctx context.Context, ref contentref.ContentRef, tempDir string) error {
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	unlock, err := PGLocker(m.journal.pool).Lock(ctx, "contentkit:media:archive:"+ref.String())
	if err != nil {
		return err
	}
	defer unlock()
	current, _, err := m.Get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, source := range current.Files {
		if !source.IsUpload() || source.Unattached || source.Fail() != nil || item.Kind().archiveTo(source.Path) == "" {
			continue
		}
		digest := sourceDigest(source)
		if digest != "" && source.Meta["archive_completed_sha256"] == digest {
			continue
		}
		if source.Blob == "" {
			return uploadErr(CodeNotUploaded, "archive is still being placed")
		}
		err := m.ingestArchive(ctx, item, source, tempDir)
		if errors.Is(err, errArchiveChanged) || errors.Is(err, ErrNotFound) {
			continue
		}
		var refusal *UploadError
		if errors.As(err, &refusal) {
			if _, failure := m.EditExisting(ctx, ref, func(cur *Manifest) error {
				if i := cur.Find(source.Path); i >= 0 && cur.Files[i].Source() == source.Source() {
					cur.Files[i].Failed = NewFailure(cur.Files[i], err)
				}
				return nil
			}); failure != nil {
				return failure
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type archivePageSource struct {
	entry       *zip.File
	name, typ   string
	sum         []byte
	size        int64
	blob        string
	uploadIndex int
}

func (m *Manifests) ingestArchive(ctx context.Context, item Item, source File, tempDir string) (resultErr error) {
	if source.Admission == nil {
		return uploadErr(CodeConflict, "archive lacks a verified upload admission; recommit the ZIP through the upload API")
	}
	admission := *source.Admission
	if admission.Enforce && admission.Owner == "" {
		return uploadErr(CodeForbidden, "archive quota owner is missing; recommit the ZIP")
	}
	k := item.Kind()
	destination := k.archiveTo(source.Path)
	target := k.uploadIndex(destination)
	if target < 0 {
		return uploadErr(CodeInvalid, "archive destination is not a configured upload")
	}
	rule := k.Uploads[target]
	key, err := item.Blob(source.Blob)
	if err != nil {
		return err
	}
	rc, obj, err := m.store.Get(ctx, key, GetOptions{})
	if err != nil {
		return err
	}
	zipFile, err := os.CreateTemp(tempDir, "contentkit-archive-*")
	if err != nil {
		rc.Close()
		return err
	}
	defer os.Remove(zipFile.Name())
	rawHash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(zipFile, rawHash), io.LimitReader(archiveContextReader{ctx, rc}, source.Size+1))
	closeErr := errors.Join(rc.Close(), zipFile.Close())
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	expected, valid := layout.BlobDigest(source.Blob)
	if !valid || n != source.Size || obj.Size != source.Size || !bytes.Equal(rawHash.Sum(nil), expected) {
		return uploadErr(CodeInvalid, "archive source checksum or size differs from its manifest")
	}
	zr, err := zip.OpenReader(zipFile.Name())
	if err != nil {
		return uploadErr(CodeInvalid, "invalid ZIP archive: %v", err)
	}
	defer zr.Close()
	pages := []archivePageSource{}
	var declared uint64
	for _, entry := range zr.File {
		base := path.Base(entry.Name)
		if entry.FileInfo().IsDir() || strings.HasPrefix(base, ".") || strings.Contains(entry.Name, "__MACOSX/") || !archiveImageName(base) {
			continue
		}
		if len(pages) >= archiveImageLimit || entry.UncompressedSize64 > uint64(archiveExpandedLimit)-declared {
			return uploadErr(CodeTooLarge, "ZIP exceeds %d images or %d expanded bytes", archiveImageLimit, archiveExpandedLimit)
		}
		declared += entry.UncompressedSize64
		pages = append(pages, archivePageSource{entry: entry, uploadIndex: len(pages) + 1})
	}
	if len(pages) == 0 {
		return uploadErr(CodeInvalid, "ZIP contains no supported images")
	}
	slices.SortStableFunc(pages, func(a, b archivePageSource) int {
		if natLess(a.entry.Name, b.entry.Name) {
			return -1
		}
		if natLess(b.entry.Name, a.entry.Name) {
			return 1
		}
		return strings.Compare(a.entry.Name, b.entry.Name)
	})
	names := make([]string, len(pages))
	for i := range pages {
		names[i] = path.Base(pages[i].entry.Name)
	}
	cleaned := CleanPageBatchNames(names)
	var expanded int64
	// Complete validation precedes every blob write and manifest edit. Reading
	// one image at a time bounds memory and detects a late CRC/type failure.
	for i := range pages {
		body, err := readArchiveEntry(ctx, pages[i].entry, min(rule.MaxBytes, archiveExpandedLimit-expanded))
		if err != nil {
			return err
		}
		typ := http.DetectContentType(body)
		if err := allows(rule, typ, int64(len(body))); err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		pages[i].sum = sum[:]
		pages[i].size = int64(len(body))
		pages[i].typ = typ
		pages[i].name = cleaned[i]
		expanded += int64(len(body))
	}
	digest := hex.EncodeToString(expected)
	fingerprintInput, _ := json.Marshal(struct {
		Source, Target string
		Admission      SourceAdmission
	}{source.Blob, destination, admission})
	operation := uuid.NewSHA1(uuid.NameSpaceOID, append([]byte("contentkit:archive-ingest:v1:"+item.Ref().String()+":"), fingerprintInput...))
	mutation := &manifestMutation{commit: manifestCommit{ID: operation, Ref: item.Ref(), Folder: item.Prefix(), Actor: "contentkit:archive-ingest", Fingerprint: sha256.Sum256(fingerprintInput)}, quota: admission.Quota}
	if _, replayed, err := m.replayCommit(ctx, item, mutation.commit); err != nil || replayed {
		return err
	}
	written := []string{}
	defer func() {
		if len(written) > 0 {
			clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			cleanupErr := m.DropUnreferenced(clean, item.Ref(), Unreferenced{Blobs: written})
			if resultErr == nil {
				resultErr = cleanupErr
			}
		}
	}()
	// Allocations are recorded before PUT. The final edit adopts all or none;
	// exact cleanup/recovery owns unused and uncertain writes, never a scan.
	for i := range pages {
		blob, err := m.NewBlobs(ctx, item.Ref(), k.Area(destination, ""), [][]byte{pages[i].sum})
		if err != nil {
			return err
		}
		pages[i].blob = blob[0]
		written = append(written, blob[0])
		body, err := readArchiveEntry(ctx, pages[i].entry, rule.MaxBytes)
		if err != nil {
			return err
		}
		physical, err := item.Blob(blob[0])
		if err != nil {
			return err
		}
		if _, err = m.store.Put(ctx, physical, bytes.NewReader(body), pages[i].size, PutOptions{ContentType: pages[i].typ, ChecksumSHA256: pages[i].sum, IfNoneMatch: "*"}); err != nil {
			return err
		}
	}
	_, err = m.editOperation(ctx, item.Ref(), true, bound{project: k.Unwritten}, mutation, func(cur *Manifest) error {
		i := cur.Find(source.Path)
		if i < 0 || cur.Files[i].Source() != source.Source() {
			return errArchiveChanged
		}
		if cur.Files[i].Admission == nil || *cur.Files[i].Admission != admission {
			return errArchiveChanged
		}
		keys := make([]string, len(written))
		for i, b := range written {
			keys[i], _ = item.Blob(b)
		}
		if err := m.checkAllocationsIn(ctx, item, keys, cur.Incarnation); err != nil {
			return err
		}
		before := cur.uploadBytes()
		objects := map[string]Object{}
		for _, page := range pages {
			physical, _ := item.Blob(page.blob)
			obj, err := m.store.Head(ctx, physical)
			if err != nil {
				return err
			}
			if obj.Size != page.size {
				return uploadErr(CodeConflict, "prepared archive page changed")
			}
			objects[page.blob] = obj
		}
		runner := &opRun{k: k, m: cur, ref: item.Ref(), objects: objects, trusted: true, exempt: !admission.Enforce}
		taken := map[string]bool{}
		existing := map[int]File{}
		for _, file := range cur.Files {
			if !file.IsUpload() {
				continue
			}
			g, _, _, _, ok := k.upload(file.Path)
			if !ok || g != target {
				continue
			}
			taken[k.NameOf(file.Path)] = true
			pm, _ := pageMetadata(file.Meta)
			if name, ok := pm["name"].(string); ok {
				taken[name] = true
			}
			if pm["archive_sha256"] == digest {
				switch index := pm["source_index"].(type) {
				case int:
					existing[index] = file
				case float64:
					existing[int(index)] = file
				}
			}
		}
		for n, page := range pages {
			if old, ok := existing[n+1]; ok {
				if sourceDigest(old) != hex.EncodeToString(page.sum) {
					return uploadErr(CodeConflict, "previous imported archive page changed")
				}
				continue
			}
			stem := UniquePageName(strings.TrimSuffix(page.name, path.Ext(page.name)), taken)
			taken[stem] = true
			createID := uuid.NewSHA1(operation, []byte(strconv.Itoa(n)+":"+page.entry.Name)).String()
			if rule.Named {
				stem = NamedPrefix + createID
			}
			targetPath := strings.TrimSuffix(destination, "{name}") + stem + strings.ToLower(path.Ext(page.name))
			meta := withPageSource(nil, stem, page.entry.Name)
			pm, _ := pageMetadata(meta)
			if _, from, to, ok := CanonicalPageName(page.name); ok {
				pm["span"] = map[string]int{"from": from, "to": to}
				delete(pm, "flags")
			}
			if stem != CleanName(strings.TrimSuffix(page.name, path.Ext(page.name))) {
				flags, _ := pm["flags"].([]string)
				pm["flags"] = append(flags, "duplicate_source_name")
			}

			pm["archive_sha256"], pm["source_entry"], pm["source_sha256"] = digest, page.entry.Name, hex.EncodeToString(page.sum)
			pm["source_index"], pm["upload_index"] = n+1, page.uploadIndex
			if page.name != path.Base(page.entry.Name) {
				flags, _ := pm["flags"].([]string)
				pm["flags"] = append(flags, "source_name_cleaned")
			}
			meta["page"] = pm
			appendAt := MaxUploads
			if err := runner.apply(-1, Op{Op: OpPut, Path: targetPath, Blob: page.blob, SourceName: path.Base(page.entry.Name), CreateID: createID, Meta: meta, Index: &appendAt}); err != nil {
				return err
			}
			if j := cur.Find(targetPath); j >= 0 {
				copy := admission
				cur.Files[j].Admission = &copy
			}
		}
		i = cur.Find(source.Path)
		meta := maps.Clone(cur.Files[i].Meta)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["archive_pages_total"], meta["archive_completed_sha256"] = len(pages), digest
		cur.Files[i].Meta = meta
		if cur.uploads() > MaxUploads {
			return tooManyFiles(MaxUploads, "archive exceeds item upload limit")
		}
		mutation.effects.Settlement = Settlement{Tenant: item.Ref().TenantID, Owner: admission.Owner, Delta: cur.uploadBytes() - before, Enforce: admission.Enforce}
		return nil
	})
	return err
}

func sourceDigest(f File) string {
	if sum, ok := layout.BlobDigest(f.Blob); ok {
		return hex.EncodeToString(sum)
	}
	return ""
}
func archiveImageName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".avif":
		return true
	}
	return false
}
func readArchiveEntry(ctx context.Context, entry *zip.File, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, uploadErr(CodeTooLarge, "ZIP exceeds its expanded byte limit")
	}
	rc, err := entry.Open()
	if err != nil {
		return nil, uploadErr(CodeInvalid, "ZIP entry %s: %v", entry.Name, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(archiveContextReader{ctx, rc}, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, uploadErr(CodeInvalid, "ZIP entry %s: %v", entry.Name, err)
	}
	if int64(len(body)) > limit || uint64(len(body)) != entry.UncompressedSize64 {
		return nil, uploadErr(CodeTooLarge, "ZIP entry %s exceeds its size limit", entry.Name)
	}
	return body, nil
}

type archiveContextReader struct {
	ctx context.Context
	src io.Reader
}

func (r archiveContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.src.Read(p)
}
