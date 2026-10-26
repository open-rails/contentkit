package media

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
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
const archiveCommitBatch = 8
const archiveExpandedLimit = int64(2 << 30)

var errArchiveChanged = errors.New("media: archive source changed during ingestion")

// IngestArchives expands the configured ZIP uploads of an item. Placement
// precedes this call. All entries are validated before any attachment, then
// bounded batches commit with stable identities inside this same ingest job.
// Each committed batch records observable progress. The original ZIP remains.
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
		if !source.IsUpload() || source.Fail() != nil || item.Kind().ArchiveDestination(source.Path) == "" {
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

// FailIngest settles uploads that placement or archive ingestion left unfinished.
func (m *Manifests) FailIngest(ctx context.Context, ref contentref.ContentRef, cause error) error {
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	_, err = m.EditExisting(ctx, ref, func(cur *Manifest) error {
		for _, f := range cur.Files {
			if f.IsUpload() && f.Fail() == nil && (f.Staged != "" || statusArchivePending(item.Kind(), f)) {
				cur.SetFailed(f.Path, cause)
			}
		}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

type archivePageSource struct {
	file                       string
	entry                      *zip.File
	name, typ                  string
	sum                        []byte
	size                       int64
	blob                       string
	uploadIndex                int
	retainedPath, retainedBlob string
}

func (m *Manifests) ingestArchive(ctx context.Context, item Item, source File, tempDir string) (resultErr error) {
	if source.Admission == nil {
		return uploadErr(CodeConflict, "archive lacks a verified upload admission; recommit the ZIP through the upload API")
	}
	admission := *source.Admission
	if admission.Enforce && (admission.Owner == "" || !admission.Accounting) {
		return uploadErr(CodeForbidden, "archive quota owner is missing; recommit the ZIP")
	}
	k := item.Kind()
	destination := k.ArchiveDestination(source.Path)
	target := k.uploadIndex(destination)
	if target < 0 {
		return uploadErr(CodeInvalid, "archive destination is not a configured upload")
	}
	if admission.Target != destination {
		return uploadErr(CodeForbidden, "archive destination changed after upload admission; recommit the ZIP")
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
	if err := validateArchiveDirectory(zipFile.Name()); err != nil {
		return err
	}
	zr, err := zip.OpenReader(zipFile.Name())
	if err != nil {
		var disk *os.PathError
		if errors.As(err, &disk) {
			return err
		}
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
	defer func() {
		for _, page := range pages {
			if page.file != "" {
				_ = os.Remove(page.file)
			}
		}
	}()
	var expanded int64
	// Complete validation precedes every blob write and manifest edit. Reading
	// one image at a time bounds memory and detects a late CRC/type failure.
	for i := range pages {
		decoded, err := spoolArchiveEntry(ctx, pages[i].entry, tempDir, min(rule.MaxBytes, archiveExpandedLimit-expanded))
		pages[i].file = decoded.file
		if err != nil {
			return err
		}
		if err := allows(rule, decoded.typ, decoded.size); err != nil {
			return err
		}
		pages[i].sum, pages[i].size, pages[i].typ = decoded.sum, decoded.size, decoded.typ
		pages[i].name = cleaned[i]
		expanded += decoded.size
	}

	digest := hex.EncodeToString(expected)
	// A second alias of the same archive reuses already attached originals.
	// Their identities are checked again in the atomic edit before completion.
	current, _, err := m.Get(ctx, item.Ref())
	if err != nil {
		return err
	}
	for _, file := range current.Files {
		if !file.IsUpload() {
			continue
		}
		g, _, _, _, ok := k.upload(file.Path)
		if !ok || g != target {
			continue
		}
		pm, _ := pageMetadata(file.Meta)
		if pm["archive_sha256"] != digest {
			continue
		}
		index := 0
		switch n := pm["source_index"].(type) {
		case int:
			index = n
		case float64:
			index = int(n)
		}
		if index < 1 || index > len(pages) {
			continue
		}
		page := &pages[index-1]
		if sourceDigest(file) != hex.EncodeToString(page.sum) || pm["source_entry"] != page.entry.Name {
			return uploadErr(CodeConflict, "previous imported archive page changed")
		}
		page.retainedPath, page.retainedBlob = file.Path, file.Blob
	}
	var newBytes int64
	newPages := 0
	for _, page := range pages {
		if page.retainedPath == "" {
			newBytes += max(page.size, uploadFloor)
			newPages++
		}
	}
	if current.uploads()+newPages > MaxUploads {
		return tooManyFiles(MaxUploads, "archive exceeds item upload limit")
	}

	fingerprintInput, _ := json.Marshal(struct {
		Source, Path, Target string
		Admission            SourceAdmission
	}{source.Blob, source.Path, destination, admission})
	operation := uuid.NewSHA1(uuid.NameSpaceOID, append([]byte("contentkit:archive-ingest:v1:"+item.Ref().String()+":"), fingerprintInput...))
	// Reserve the validated expanded bytes before any page PUT. The accepted
	// source policy is replayed without invoking a host callback in the worker.
	reservationKey := item.TempPrefix() + "archive-quota-" + operation.String()
	quotaLedger := *m.journal.forTenant(item.Ref().TenantID).limiter
	quotaLedger.limits.Quota = func(context.Context, string, string) (int64, error) { return admission.Quota, nil }
	if admission.Enforce && newBytes > 0 {
		if err := quotaLedger.Reserve(ctx, Reservation{Tenant: item.Ref().TenantID, Owner: admission.Owner, Uploader: "contentkit:archive-ingest", Key: reservationKey, Size: newBytes}); err != nil {
			return err
		}
		defer func() {
			clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			_ = quotaLedger.Settle(clean, Settlement{Tenant: item.Ref().TenantID, Owner: admission.Owner, Keys: []string{reservationKey}})
		}()
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
	for batchStart := 0; batchStart < len(pages); batchStart += archiveCommitBatch {
		batchEnd := min(batchStart+archiveCommitBatch, len(pages))
		batchKey := "batch:" + strconv.Itoa(batchStart)
		batchInput := append(slices.Clone(fingerprintInput), []byte(batchKey)...)
		mutation := &manifestMutation{commit: manifestCommit{ID: uuid.NewSHA1(operation, []byte(batchKey)), Ref: item.Ref(), Folder: item.Prefix(), Actor: "contentkit:archive-ingest", Fingerprint: sha256.Sum256(batchInput)}, quota: admission.Quota}
		if _, replayed, err := m.replayCommit(ctx, item, mutation.commit); err != nil {
			return err
		} else if replayed {
			if err := CompleteProcessing(ctx, m, item.Ref(), nil); err != nil {
				return err
			}
			continue
		}
		// Each batch owns its fresh allocations before PUT; its exact mutation
		// adopts them together. Failed/unused objects belong to content purge.
		for i := batchStart; i < batchEnd; i++ {
			if pages[i].retainedPath != "" {
				continue
			}
			blob, err := m.NewBlobs(ctx, item.Ref(), k.Area(destination, ""), [][]byte{pages[i].sum})
			if err != nil {
				return err
			}
			pages[i].blob = blob[0]
			written = append(written, blob[0])
			input, err := os.Open(pages[i].file)
			if err != nil {
				return err
			}
			physical, err := item.Blob(blob[0])
			if err != nil {
				input.Close()
				return err
			}
			_, writeErr := m.store.Put(ctx, physical, input, pages[i].size, PutOptions{ContentType: pages[i].typ, ChecksumSHA256: pages[i].sum, IfNoneMatch: "*"})
			closeErr := input.Close()
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
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
			archiveUnattached := cur.Files[i].Unattached
			before := cur.uploadBytes()
			objects := map[string]Object{}
			for _, page := range pages[batchStart:batchEnd] {
				if page.retainedPath != "" {
					if old, ok := cur.Get(page.retainedPath); !ok || old.Blob != page.retainedBlob {
						return errArchiveChanged
					}
					continue
				}
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
			for n, page := range pages[batchStart:batchEnd] {
				n += batchStart
				if old, ok := existing[n+1]; ok {
					if sourceDigest(old) != hex.EncodeToString(page.sum) {
						return uploadErr(CodeConflict, "previous imported archive page changed")
					}
					if old.Unattached && !archiveUnattached {
						if err := runner.apply(-1, Op{Op: OpAttach, Path: old.Path}); err != nil {
							return err
						}
					}
					continue
				}
				if page.blob == "" {
					return errArchiveChanged
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
				if err := runner.apply(-1, Op{Op: OpPut, Path: targetPath, Blob: page.blob, SourceName: path.Base(page.entry.Name), CreateID: createID, Meta: meta, Index: &appendAt, Unattached: archiveUnattached}); err != nil {
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
			meta["archive_pages_total"] = len(pages)
			if batchEnd == len(pages) {
				meta["archive_completed_sha256"] = digest
			}
			cur.Files[i].Meta = meta
			if cur.uploads() > MaxUploads {
				return tooManyFiles(MaxUploads, "archive exceeds item upload limit")
			}
			chargedOwner := admission.Owner
			if !admission.Accounting {
				chargedOwner = ""
			}
			var reservationKeys []string
			if batchEnd == len(pages) {
				reservationKeys = []string{reservationKey}
			}
			mutation.effects.Settlement = Settlement{Tenant: item.Ref().TenantID, Owner: chargedOwner, Keys: reservationKeys, Delta: cur.uploadBytes() - before, Enforce: admission.Enforce}
			return nil
		})
		if err != nil {
			return err
		}
		if err := CompleteProcessing(ctx, m, item.Ref(), nil); err != nil {
			return err
		}
	}

	return nil
}

func sourceDigest(f File) string {
	if sum, ok := layout.BlobDigest(f.Blob); ok {
		return hex.EncodeToString(sum)
	}
	return ""
}
func archiveImageName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".avif", ".heic", ".heif", ".tif", ".tiff":
		return true
	}
	return false
}

type decodedArchiveEntry struct {
	file, typ string
	size      int64
	sum       []byte
}

func spoolArchiveEntry(ctx context.Context, entry *zip.File, tempDir string, limit int64) (decodedArchiveEntry, error) {
	if limit <= 0 {
		return decodedArchiveEntry{}, uploadErr(CodeTooLarge, "ZIP exceeds its expanded byte limit")
	}
	rc, err := entry.Open()
	if err != nil {
		var disk *os.PathError
		if errors.As(err, &disk) {
			return decodedArchiveEntry{}, err
		}
		return decodedArchiveEntry{}, uploadErr(CodeInvalid, "ZIP entry %s: %v", entry.Name, err)
	}
	defer rc.Close()
	output, err := os.CreateTemp(tempDir, "contentkit-archive-page-*")
	if err != nil {
		return decodedArchiveEntry{}, err
	}
	result := decodedArchiveEntry{file: output.Name()}
	n, sum, copyErr := copyArchiveEntry(ctx, rc, output, limit)
	closeErr := output.Close()
	if copyErr != nil {
		return result, copyErr
	}
	if closeErr != nil {
		return result, closeErr
	}
	if n > limit || uint64(n) != entry.UncompressedSize64 {
		return result, uploadErr(CodeTooLarge, "ZIP entry %s exceeds its size limit", entry.Name)
	}
	input, err := os.Open(result.file)
	if err != nil {
		return result, err
	}
	defer input.Close()
	prefix := make([]byte, 64<<10)
	read, err := input.Read(prefix)
	if err != nil && !errors.Is(err, io.EOF) {
		return result, err
	}
	result.typ = archiveContentType(prefix[:read])
	result.size = n
	result.sum = sum
	return result, nil
}

// copyArchiveEntry keeps sink failures separate from malformed compressed
// input. Disk/cancellation errors are retryable infrastructure failures.
func copyArchiveEntry(ctx context.Context, source io.Reader, destination io.Writer, limit int64) (int64, []byte, error) {
	sink := &archiveSink{writer: destination}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(sink, hash), io.LimitReader(archiveContextReader{ctx, source}, limit+1))
	if sink.failure != nil {
		return n, nil, sink.failure
	}
	if err != nil {
		if ctx.Err() != nil {
			return n, nil, ctx.Err()
		}
		var disk *os.PathError
		if errors.As(err, &disk) {
			return n, nil, err
		}
		return n, nil, uploadErr(CodeInvalid, "ZIP entry stream: %v", err)
	}
	return n, hash.Sum(nil), nil
}

type archiveSink struct {
	writer  io.Writer
	failure error
}

func (s *archiveSink) Write(p []byte) (int, error) {
	n, err := s.writer.Write(p)
	if n != len(p) && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.failure = err
	}
	return n, err
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

// archiveContentType recognizes the supported raster containers without
// guessing from an extension. Native image processing still validates/decodes
// the actual codec and enforces frame/pixel limits before producing renditions.
func archiveContentType(body []byte) string {
	if len(body) >= 16 && string(body[4:8]) == "ftyp" {
		size := int(binary.BigEndian.Uint32(body[:4]))
		if size >= 16 && size <= len(body) {
			brands := []string{string(body[8:12])}
			for i := 16; i+4 <= size; i += 4 {
				brands = append(brands, string(body[i:i+4]))
			}
			if slices.Contains(brands, "avif") || slices.Contains(brands, "avis") {
				return "image/avif"
			}
			for _, brand := range brands {
				switch brand {
				case "heic", "heix", "hevc", "hevx":
					return "image/heic"
				}
			}
			if slices.Contains(brands, "mif1") || slices.Contains(brands, "msf1") {
				return "image/heif"
			}
		}
	}
	if len(body) >= 4 && (bytes.Equal(body[:4], []byte{'I', 'I', 42, 0}) || bytes.Equal(body[:4], []byte{'M', 'M', 0, 42})) {
		return "image/tiff"
	}
	return http.DetectContentType(body)
}
