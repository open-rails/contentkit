package media

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media/layout"
)

// ProcessingCount counts source files, not individual encoded widths or chunks.
// Completed and Failed are disjoint; the remaining files are still pending.
type ProcessingCount struct {
	Completed int `json:"completed"`
	Total     int `json:"total"`
	Failed    int `json:"failed"`
}

// ArchiveStatus counts source ZIPs and, once validation has determined the
// expected entries, the pages actually committed by the existing import worker.
// Page counts are persisted progress, not a claim that a worker is running.
type ArchiveStatus struct {
	ProcessingCount
	Pages *ProcessingCount `json:"pages,omitempty"`
}

// PresetStatus is progress for one configured rendition preset.
type PresetStatus struct {
	Name string `json:"name"`
	ProcessingCount
}

// ProcessingFailure is an editor-visible failure without internal paths,
// source URLs or the worker's unfiltered error chain.
type ProcessingFailure struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// MediaStatus is a read-only snapshot of attached uploads in the whole manifest.
// Public presets count only while visible, as in Kind.Readiness. An archive
// may have finished placement while still waiting for extraction. Encoding is
// the existing worker checkpoint, when available, never a new job or operation.
type MediaStatus struct {
	State        string              `json:"state"`
	Uploads      ProcessingCount     `json:"uploads"`
	Archives     ArchiveStatus       `json:"archives"`
	Presets      []PresetStatus      `json:"presets"`
	Encoding     *EncodeProgress     `json:"encoding,omitempty"`
	FailureCount int                 `json:"failure_count"`
	Failures     []ProcessingFailure `json:"failures"`
}

// Status requires edit access and issues no media capability, cookie or work.
func (r *Reader) Status(ctx context.Context, ref contentref.ContentRef, actor access.Actor) (*MediaStatus, error) {
	item, err := r.reg.Item(ref)
	if err != nil {
		return nil, ErrNotVisible
	}
	res, err := resolve.One(resolve.WithEditorRead(ctx), r.reg.cfg.Hooks.Resolver, ref, actor)
	if err != nil {
		return nil, errors.Join(ErrResolve, err)
	}
	if !res.Visible || !res.Editor {
		return nil, ErrNotVisible
	}
	m, _, err := r.o.Manifests.Get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		m = &Manifest{V: ManifestVersion}
	} else if err != nil {
		return nil, err
	}
	return statusOf(item.Kind(), m, r.statusProgress(ctx, ref, m)), nil
}

func (r *Reader) statusProgress(ctx context.Context, ref contentref.ContentRef, m *Manifest) EncodeStatus {
	if r.o.Progress != nil && slices.ContainsFunc(m.Files, func(f File) bool {
		return f.IsUpload() && len(f.Pending) > 0 && (isVideoType(f.Type) || isAudioType(f.Type))
	}) {
		if progress, err := r.o.Progress.EncodeProgress(ctx, ref); err == nil {
			return progress
		}
	}
	return EncodeStatus{}
}

func statusOf(k *Kind, m *Manifest, progress EncodeStatus) *MediaStatus {
	out := &MediaStatus{State: k.Readiness(m).State, Presets: []PresetStatus{}, Failures: []ProcessingFailure{}}
	if slices.ContainsFunc(m.Files, func(f File) bool {
		return f.IsUpload() && !f.Unattached && len(f.Pending) > 0 && (isVideoType(f.Type) || isAudioType(f.Type))
	}) {
		out.Encoding = progress.Current()
	}
	readyOutputs := map[[2]string]bool{}
	imported := map[string]map[string]struct{}{}
	for _, f := range m.Files {
		if f.Preset != "" && f.Blob != "" {
			readyOutputs[[2]string{f.From, f.Preset}] = true
		}
		if f.IsUpload() && k.IsPageUpload(f) {
			if page, err := pageMetadata(f.Meta); err == nil {
				if sum, ok := page["archive_sha256"].(string); ok {
					if imported[sum] == nil {
						imported[sum] = map[string]struct{}{}
					}
					identity := f.Path
					if index := statusCount(page["source_index"]); index > 0 {
						identity = "index:" + strconv.Itoa(index)
					}
					imported[sum][identity] = struct{}{}
				}
			}
		}
	}
	presets := map[string]*ProcessingCount{}
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached {
			continue
		}
		failed := f.Fail()
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
					out.Archives.Pages = &ProcessingCount{}
				}
				digest, _ := layout.BlobDigest(f.Blob)
				sum := hex.EncodeToString(digest)
				if len(digest) == 0 {
					sum, _ = f.Meta["sha256"].(string)
				}
				done := min(total, len(imported[sum]))
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
			case importPending(k, f):
				if out.State == StateReady {
					out.State = StateProcessing
				}
			default:
				out.Archives.Completed++
			}
		}
		for _, name := range k.Presets(f.Path, m.Hidden) {
			count := presets[name]
			if count == nil {
				count = &ProcessingCount{}
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
		out.Presets = append(out.Presets, PresetStatus{Name: name, ProcessingCount: *count})
	}
	slices.SortFunc(out.Presets, func(a, b PresetStatus) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func isArchiveUpload(k *Kind, f File) bool {
	if k.Publish == nil || k.Publish.Import == "" {
		return false
	}
	pattern, ok := parsePattern(k.Publish.Import)
	if !ok {
		return false
	}
	stem, _ := splitExt(f.Path)
	_, matches := pattern.match(stem)
	return matches
}

func statusFailure(path, code string) ProcessingFailure {
	message := "Media processing failed."
	switch code {
	case CodeNotUploaded:
		message = "The uploaded file is missing or changed; upload it again."
	case CodeImageUnreadable:
		message = "The image could not be decoded."
	case CodeImageTooLarge, CodeTooLarge:
		message = "The file exceeds the configured size limit."
	case CodeChecksum:
		message = "The uploaded file did not match its checksum."
	case CodeType:
		message = "This file type is not supported."
	case CodeInvalid:
		message = "The uploaded file is invalid."
	case CodeUnavailable:
		message = "Media storage is temporarily unavailable."
	}
	return ProcessingFailure{Path: path, Code: code, Message: message}
}

func statusCount(v any) int {
	var n int64
	switch v := v.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case float64:
		if v <= 0 || v > MaxUploads || v != float64(int64(v)) {
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
	if n < 0 || n > MaxUploads {
		return 0
	}
	return int(n)
}
