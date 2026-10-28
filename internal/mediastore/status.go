package mediastore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/resolve"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Status requires edit access and issues no media capability, cookie or work.
func (r *Reader) Status(ctx context.Context, ref contentref.ContentRef, actor access.Actor) (*media.MediaStatus, error) {
	item, err := r.reg.Item(ref)
	if err != nil {
		return nil, ErrNotVisible
	}
	res, err := resolve.One(resolve.WithEditorRead(ctx), r.reg.hooks.Resolver, ref, actor)
	if err != nil {
		return nil, errors.Join(ErrResolve, err)
	}
	if !res.Visible || !res.Editor {
		return nil, ErrNotVisible
	}
	m, admitted, err := r.o.Manifests.admittedManifest(ctx, item)
	if err != nil {
		return nil, err
	}
	if !admitted {
		m, _, err = r.o.Manifests.Get(ctx, ref)
		if errors.Is(err, ErrNotFound) {
			m = &Manifest{V: media.ManifestVersion}
		} else if err != nil {
			return nil, err
		}
	}
	status := statusOf(item.Kind(), m, r.statusProgress(ctx, ref, m))
	if admitted {
		status.State = media.StateProcessing
	}
	return status, nil
}

// admittedManifest observes accepted work before its S3 PUT. Only editor
// status uses this image: ordinary readers never resolve uncommitted objects.
func (m *Manifests) admittedManifest(ctx context.Context, item Item) (*Manifest, bool, error) {
	jr := m.journal.forTenant(item.Ref().TenantID)
	var body []byte
	err := jr.pool.QueryRow(ctx, `SELECT effects FROM `+jr.table+` WHERE tenant_id=$1 AND folder_prefix=$2
AND content_kind=$3 AND content_id=$4 AND content_version_id IS NOT DISTINCT FROM $5::text
AND state IN ('prepared','frozen') AND effects->>'ingest'='true'`, item.Ref().TenantID, item.Prefix(), item.Ref().ContentKind, item.Ref().ContentID, item.Ref().ContentVersionID).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var effects journalEffects
	if err := json.Unmarshal(body, &effects); err != nil {
		return nil, false, err
	}
	manifest, err := decodeManifest(effects.Manifest)
	return manifest, true, err
}

func (r *Reader) statusProgress(ctx context.Context, ref contentref.ContentRef, m *Manifest) media.EncodeStatus {
	if r.o.Progress != nil && slices.ContainsFunc(m.Files, func(f media.File) bool {
		return f.IsUpload() && len(f.Pending) > 0 && isVideoType(f.Type)
	}) {
		if progress, err := r.o.Progress.EncodeProgress(ctx, ref); err == nil {
			return progress
		}
	}
	return media.EncodeStatus{}
}

func statusOf(k *RegisteredKind, m *Manifest, progress media.EncodeStatus) *media.MediaStatus {
	out := &media.MediaStatus{State: k.Readiness(m).State, Presets: []media.PresetStatus{}, Failures: []media.ProcessingFailure{}}
	if slices.ContainsFunc(m.Files, func(f media.File) bool {
		return f.IsUpload() && statusHasPending(k, m, f) && isVideoType(f.Type)
	}) {
		out.Encoding = progress.Current()
	}
	readyOutputs := map[[2]string]bool{}
	imported := map[[2]string]map[string]struct{}{}
	for _, f := range m.Files {
		if f.Preset != "" && f.Blob != "" {
			readyOutputs[[2]string{f.From, f.Preset}] = true
		}
		if f.IsUpload() {
			i, _, _, _, known := k.upload(f.Path)
			page, err := pageMetadata(f.Meta)
			if known && err == nil {
				if sum, ok := page["archive_sha256"].(string); ok {
					key := [2]string{sum, k.Uploads[i].Path}
					if imported[key] == nil {
						imported[key] = map[string]struct{}{}
					}
					identity := f.Path
					if index := statusCount(page["source_index"]); index > 0 {
						identity = "index:" + strconv.Itoa(index)
					}
					imported[key][identity] = struct{}{}
				}
			}
		}
	}
	presets := map[string]*media.ProcessingCount{}
	processing := out.State == media.StateProcessing || out.State == media.StateFull
	failures := out.State == media.StateFailed
	for _, f := range m.Files {
		if !f.IsUpload() {
			continue
		}
		failed := f.Fail()
		if k.audioOriginal(f) {
			failed = nil // An obsolete conversion failure does not block original playback.
		}
		if failed != nil {
			failures = true
		} else if f.Blob == "" || statusHasPending(k, m, f) {
			processing = true
		}
		out.Uploads.Total++
		if failed != nil {
			out.Uploads.Failed++
			out.FailureCount++
			if len(out.Failures) < 32 {
				out.Failures = append(out.Failures, statusFailure(f.Path, failed.Code))
			}
		} else if f.Blob != "" {
			out.Uploads.Completed++
		}
		if isArchiveUpload(k, f) {
			out.Archives.Total++
			if total := statusCount(f.Meta["archive_pages_total"]); total > 0 {
				if out.Archives.Pages == nil {
					out.Archives.Pages = &media.ProcessingCount{}
				}
				digest, _ := layout.BlobDigest(f.Blob)
				sum := hex.EncodeToString(digest)
				if len(digest) == 0 {
					sum, _ = f.Meta["sha256"].(string)
				}
				done := min(total, len(imported[[2]string{sum, k.ArchiveDestination(f.Path)}]))
				// A completed extraction cannot regress when QA excludes or
				// removes pages later; completion is bound to this exact ZIP.
				if completed, _ := f.Meta["archive_completed_sha256"].(string); sum != "" && completed == sum {
					done = total
				}
				out.Archives.Pages.Total += total
				out.Archives.Pages.Completed += done
				if failed != nil {
					out.Archives.Pages.Failed += total - done
				}
			}
			switch {
			case failed != nil:
				out.Archives.Failed++
			case statusArchivePending(k, f):
				processing = true
			default:
				out.Archives.Completed++
			}
		}
		for _, name := range k.Presets(f.Path, f.Unattached) {
			count := presets[name]
			if count == nil {
				count = &media.ProcessingCount{}
				presets[name] = count
			}
			count.Total++
			if failed != nil {
				count.Failed++
				continue
			}
			if f.Blob == "" || slices.Contains(f.Pending, name) {
				continue
			}
			if public := k.public(name); public != nil {
				publication, ok := k.Current(m, f, public)
				if !ok || !publication.Ready() {
					continue
				}
			} else if !readyOutputs[[2]string{f.Path, name}] {
				continue
			}
			count.Completed++
		}
	}
	for name, count := range presets {
		out.Presets = append(out.Presets, media.PresetStatus{Name: name, ProcessingCount: *count})
	}
	slices.SortFunc(out.Presets, func(a, b media.PresetStatus) int { return strings.Compare(a.Name, b.Name) })
	switch {
	case processing && m.Full:
		out.State = media.StateFull
	case processing:
		out.State = media.StateProcessing
	case failures:
		out.State = media.StateFailed
	default:
		out.State = media.StateReady
	}
	return out
}

func statusHasPending(k *RegisteredKind, m *Manifest, f media.File) bool {
	return slices.ContainsFunc(f.Pending, func(name string) bool {
		return k.private(name) != nil || !f.Unattached && k.public(name) != nil
	})
}

func isArchiveUpload(k *RegisteredKind, f media.File) bool {
	return f.IsUpload() && k.ArchiveDestination(f.Path) != ""
}

func statusArchivePending(k *RegisteredKind, f media.File) bool {
	if !isArchiveUpload(k, f) || f.Gone || f.Fail() != nil {
		return false
	}
	digest, valid := layout.BlobDigest(f.Blob)
	var sum string
	if valid {
		sum = hex.EncodeToString(digest)
	} else {
		sum, _ = f.Meta["sha256"].(string)
	}
	completed, _ := f.Meta["archive_completed_sha256"].(string)
	return sum == "" || completed != sum
}

func statusFailure(path, code string) media.ProcessingFailure {
	message := "Media processing failed."
	switch code {
	case media.CodeNotUploaded:
		message = "The uploaded file is missing or changed; upload it again."
	case media.CodeImageUnreadable:
		message = "The image could not be decoded."
	case media.CodeImageTooLarge, media.CodeTooLarge:
		message = "The file exceeds the configured size limit."
	case media.CodeChecksum:
		message = "The uploaded file did not match its checksum."
	case media.CodeType:
		message = "This file type is not supported."
	case media.CodeInvalid:
		message = "The uploaded file is invalid."
	case media.CodeUnavailable:
		message = "Media storage is temporarily unavailable."
	}
	return media.ProcessingFailure{Path: path, Code: code, Message: message}
}

func statusCount(v any) int {
	var n int64
	switch v := v.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case float64:
		if v <= 0 || v > media.MaxUploads || v != float64(int64(v)) {
			return 0
		}
		n = int64(v)
	case json.Number:
		var err error
		n, err = v.Int64()
		if err != nil {
			return 0
		}
	default:
		return 0
	}
	if n < 0 || n > media.MaxUploads {
		return 0
	}
	return int(n)
}
