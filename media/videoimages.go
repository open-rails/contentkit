package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// Video kinds carry two public images besides the HLS sprite: the poster
// slot and a hover preview, a short silent loop. Both are item-level.
const (
	PosterSlot   = "poster"
	HoverPreview = "hover_preview"
)

// VideoPoster is the poster slot every video kind gets. Its original is an
// uploaded image or a frame the video worker grabbed; the image job encodes
// either through the slot's Edit. It is native: the poster keeps the frame's
// (or upload's) own aspect unless an edit crops it.
var VideoPoster = Slot{Widths: []int{480, 960, 1920}}

// Hover preview bounds (seconds) and output widths. The smallest width is
// always rendered, so listings can link it without a read; wider ones only
// when the video is at least that wide.
const (
	HoverPreviewDefault = 3.0
	HoverPreviewMin     = 1.0
	HoverPreviewMax     = 6.0
)

var HoverPreviewWidths = []int{320, 640}

// PosterFrame is a poster grabbed from a video frame (SlotRecord.Frame).
type PosterFrame struct {
	Version string  `json:"version,omitempty"` // manifest holding File, for versioned kinds
	File    string  `json:"file"`
	Time    float64 `json:"time"`
	Auto    bool    `json:"auto,omitempty"`   // chosen by the worker (first non-flat frame)
	Source  string  `json:"source,omitempty"` // original grabbed from; "" until grabbed
}

// Same reports the same selection; an automatic one at any time.
func (f PosterFrame) Same(o PosterFrame) bool {
	return f.Version == o.Version && f.File == o.File && f.Auto == o.Auto && (f.Auto || f.Time == o.Time)
}

// PosterFrameSize is the size of a grabbed poster frame from a w×h rendition:
// as is, or upscaled until it reaches VideoPoster.Min() wide, so every poster
// has its smallest width. Poster edits are in these pixels.
func PosterFrameSize(w, h int) Dims {
	if min := float64(VideoPoster.Min()); w > 0 && float64(w) < min {
		f := min / float64(w)
		return Dims{W: int(math.Ceil(float64(w) * f)), H: int(math.Ceil(float64(h) * f))}
	}
	return Dims{W: w, H: h}
}

// FrameRendition is the rendition poster frames are grabbed from: the widest.
func FrameRendition(f File) (Rendition, bool) {
	if f.HLS == nil || len(f.HLS.Video) == 0 {
		return Rendition{}, false
	}
	return slices.MaxFunc(f.HLS.Video, func(a, b Rendition) int { return a.Width - b.Width }), true
}

// HoverPreviewRecord is originals/hover_preview.json: the selected section and
// the last render.
type HoverPreviewRecord struct {
	Version  string              `json:"version,omitempty"`
	File     string              `json:"file"`
	Start    float64             `json:"start"`
	Duration float64             `json:"duration"`
	Auto     bool                `json:"auto,omitempty"`
	Result   *HoverPreviewResult `json:"result,omitempty"`
}

// Key identifies the selection a result was rendered for.
func (r HoverPreviewRecord) Key() string {
	return r.Version + "|" + r.File + "|" + strconv.FormatFloat(r.Start, 'f', 3, 64) + "|" + strconv.FormatFloat(r.Duration, 'f', 3, 64)
}

// HoverPreviewResult is what the served outputs were rendered from.
type HoverPreviewResult struct {
	Of      string `json:"of"`      // HoverPreviewRecord.Key
	Source  string `json:"source"`  // the file's original
	Recipe  string `json:"recipe"`  // the renderer's identity
	Version string `json:"version"` // URL version (?v=) the outputs carry
	Outputs []Dims `json:"outputs"`
}

// HoverPreviewVersion is the URL version of a render.
func HoverPreviewVersion(key, source, recipe string) string {
	sum := sha256.Sum256([]byte(key + "|" + source + "|" + recipe))
	return hex.EncodeToString(sum[:8])
}

// AutoHoverPreview is the default section: HoverPreviewDefault seconds from a
// quarter in, inside the video.
func AutoHoverPreview(duration float64) (start, length float64) {
	length = math.Min(HoverPreviewDefault, duration)
	start = math.Max(0, math.Min(duration*0.25, duration-length))
	return round3(start), round3(length)
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// HoverPreviewAspect is the hover preview's centred crop.
const HoverPreviewAspect = 16.0 / 9

// HoverPreviewSizes are the output sizes for a w×h video: the centred
// HoverPreviewAspect crop, never upscaled beyond the smallest width.
func HoverPreviewSizes(w, h int) []Dims {
	cropW := min(float64(w), float64(h)*HoverPreviewAspect)
	var out []Dims
	for i, pw := range HoverPreviewWidths {
		if i == 0 || float64(pw) <= cropW {
			out = append(out, Dims{W: pw, H: max(1, int(math.Round(float64(pw)/HoverPreviewAspect)))})
		}
	}
	return out
}

// HoverPreviewRecord is originals/hover_preview.json.
func (i Item) HoverPreviewRecord() string { return i.OriginalsPrefix() + HoverPreview + slotRecordExt }

// HoverPreviewOutput is where the worker renders a hover preview:
// editor/hover_preview_{width}.webp or .mp4. Publish copies it to
// HoverPreviewPublic as the item's Exposure allows.
func (i Item) HoverPreviewOutput(width int, mp4 bool) string {
	return i.EditorPrefix() + hoverPreviewName(width, mp4)
}

// HoverPreviewPublic is public/hover_preview_{width}.webp or .mp4.
func (i Item) HoverPreviewPublic(width int, mp4 bool) string {
	return i.PublicPrefix() + hoverPreviewName(width, mp4)
}

func hoverPreviewName(width int, mp4 bool) string {
	ext := layout.PublicExt
	if mp4 {
		ext = layout.PublicMP4Ext
	}
	return HoverPreview + "_" + strconv.Itoa(width) + ext
}

// HoverPreview returns an item's hover-preview record, or ErrNotFound.
func (m *Manifests) HoverPreview(ctx context.Context, ref contentref.ContentRef) (*HoverPreviewRecord, error) {
	item, err := m.kinds.Item(ref.Content())
	if err != nil {
		return nil, err
	}
	body, _, err := m.raw(ctx, item.HoverPreviewRecord())
	if err != nil {
		return nil, err
	}
	var rec HoverPreviewRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("media: decode hover preview record: %w", err)
	}
	return &rec, nil
}

// UpdateHoverPreview applies fn to the record (nil before the first write)
// like UpdateSlot. fn returns the record to write, or nil to leave it alone.
func (m *Manifests) UpdateHoverPreview(ctx context.Context, ref contentref.ContentRef, fn func(*HoverPreviewRecord) (*HoverPreviewRecord, error)) error {
	item, err := m.kinds.Item(ref.Content())
	if err != nil {
		return err
	}
	_, err = m.edit(ctx, item.HoverPreviewRecord(), func(body []byte) ([]byte, error) {
		var cur *HoverPreviewRecord
		if body != nil {
			cur = &HoverPreviewRecord{}
			if err := json.Unmarshal(body, cur); err != nil {
				return nil, fmt.Errorf("media: decode hover preview record: %w", err)
			}
		}
		next, err := fn(cur)
		if err != nil || next == nil {
			return nil, err
		}
		out, err := json.Marshal(next)
		if err != nil || bytes.Equal(out, body) {
			return nil, err
		}
		return out, nil
	})
	return err
}

// ErrSuperseded reports a record that changed while a job worked from it; the
// job for the newer record does the work instead.
var ErrSuperseded = errors.New("media: record changed")

// FrameGrabber renders a small JPEG of an encoded video file's frame for the
// poster picker (media/video.Frames).
type FrameGrabber interface {
	Frame(ctx context.Context, item Item, f File, t float64, width int) ([]byte, error)
}

// Frame endpoint bounds.
const (
	FrameDefaultWidth = 320
	FrameMaxWidth     = 1280
	frameMinWidth     = 64
)

// Poster sources.
const (
	PosterSourceFrame  = "frame"
	PosterSourceUpload = "upload"
	PosterSourceAuto   = "auto"
)

// PosterRequest selects a video's poster: Source "frame" grabs File at Time
// (seconds); "upload" commits the image uploaded to the poster slot (SHA256
// as presigned); "auto" returns to the worker's choice. Edit is the slot edit,
// in the grabbed frame's pixels (VideoInfo.W×H) or the upload's; nil centres.
type PosterRequest struct {
	Source string
	File   string // default: the first video file
	Time   float64
	SHA256 []byte
	Edit   *Edit
}

// SetVideoPoster records a poster selection and enqueues its work: a frame
// grab by the video worker (which then hands the frame to the image job), or
// the upload's encode. Frame selections need the ref's manifest (a version for
// versioned kinds) and an encoded file.
func (u *Uploads) SetVideoPoster(ctx context.Context, actor access.Actor, ref contentref.ContentRef, r PosterRequest) error {
	item, err := u.videoItem(ref)
	if err != nil {
		return err
	}
	if r.Source == PosterSourceUpload {
		return u.CommitSlot(ctx, actor, ref.Content(), PosterSlot, r.SHA256, r.Edit)
	}
	if r.Source != PosterSourceFrame && r.Source != PosterSourceAuto {
		return uploadErr(CodeInvalid, "poster source must be frame, upload or auto")
	}
	if _, err := u.authorize(ctx, actor, ref.Content()); err != nil {
		return err
	}
	f, err := u.encodedVideo(ctx, item, r.File)
	if err != nil {
		return err
	}
	frame := &PosterFrame{Version: ref.Version(), File: f.Name, Auto: true}
	var edit *Edit
	if r.Source == PosterSourceFrame {
		d := metaFloat(f.Meta, "duration")
		if math.IsNaN(r.Time) || r.Time < 0 || r.Time >= d {
			return uploadErr(CodeInvalid, "time must be within 0-%.3f seconds", d)
		}
		size := posterFrame(f)
		if _, err := VideoPoster.Resolve(r.Edit, size.W, size.H); err != nil {
			return uploadErr(CodeInvalid, "edit: %v", err)
		}
		edit = VideoPoster.fit(r.Edit)
		frame.Auto, frame.Time = false, round3(r.Time)
	} else if r.Edit.Normalize() != nil {
		return uploadErr(CodeInvalid, "an automatic poster takes no edit")
	}
	if err := u.o.Manifests.UpdateSlot(ctx, ref.Content(), PosterSlot, func(rec *SlotRecord) error {
		if rec.Frame != nil && rec.Frame.Same(*frame) && rec.Edit.Hash() == edit.Hash() {
			return nil
		}
		// No original is committed until the worker grabs the frame: the image job skips the slot.
		rec.Original, rec.Edit, rec.Frame = "", edit, frame
		return nil
	}); err != nil {
		return err
	}
	return u.enqueueVideo(ctx, ref)
}

// posterFrame is the size a frame of f is grabbed at.
func posterFrame(f File) Dims {
	r, _ := FrameRendition(f)
	return PosterFrameSize(r.Width, r.Height)
}

// PreviewRequest selects the hover-preview section: Start nil is the
// automatic section; Duration 0 is HoverPreviewDefault.
type PreviewRequest struct {
	File     string
	Start    *float64
	Duration float64
}

// SetHoverPreview records a hover-preview selection and enqueues its render.
func (u *Uploads) SetHoverPreview(ctx context.Context, actor access.Actor, ref contentref.ContentRef, r PreviewRequest) error {
	item, err := u.videoItem(ref)
	if err != nil {
		return err
	}
	if _, err := u.authorize(ctx, actor, ref.Content()); err != nil {
		return err
	}
	f, err := u.encodedVideo(ctx, item, r.File)
	if err != nil {
		return err
	}
	d := metaFloat(f.Meta, "duration")
	rec := HoverPreviewRecord{Version: ref.Version(), File: f.Name}
	if r.Start == nil {
		if r.Duration != 0 {
			return uploadErr(CodeInvalid, "duration needs a start")
		}
		rec.Auto = true
		rec.Start, rec.Duration = AutoHoverPreview(d)
	} else {
		length := r.Duration
		if length == 0 {
			length = HoverPreviewDefault
		}
		length = math.Min(length, d)
		start := *r.Start
		switch {
		case math.IsNaN(length) || length < math.Min(HoverPreviewMin, d) || length > HoverPreviewMax:
			return uploadErr(CodeInvalid, "duration must be %g-%g seconds", HoverPreviewMin, HoverPreviewMax)
		case math.IsNaN(start) || start < 0 || start+length > d+0.001:
			return uploadErr(CodeInvalid, "the section must lie within 0-%.3f seconds", d)
		}
		rec.Start, rec.Duration = round3(start), round3(length)
	}
	if err := u.o.Manifests.UpdateHoverPreview(ctx, ref, func(cur *HoverPreviewRecord) (*HoverPreviewRecord, error) {
		if cur != nil {
			rec.Result = cur.Result
		}
		return &rec, nil
	}); err != nil {
		return err
	}
	return u.enqueueVideo(ctx, ref)
}

// Frame renders a small JPEG of the encoded file's frame at t for the poster
// picker. t is clamped into the video, width into 64-FrameMaxWidth (0 is
// FrameDefaultWidth). At most UploadOptions.FrameConcurrency run at once;
// others wait briefly, then answer CodeRate.
func (u *Uploads) Frame(ctx context.Context, actor access.Actor, ref contentref.ContentRef, file string, t float64, width int) ([]byte, error) {
	if u.o.Frames == nil {
		return nil, uploadErr(CodeNotFound, "frame grabs are not configured")
	}
	item, err := u.videoItem(ref)
	if err != nil {
		return nil, err
	}
	if math.IsNaN(t) || math.IsInf(t, 0) {
		return nil, uploadErr(CodeInvalid, "t must be a number of seconds")
	}
	if _, err := u.authorize(ctx, actor, ref.Content()); err != nil {
		return nil, err
	}
	f, err := u.encodedVideo(ctx, item, file)
	if err != nil {
		return nil, err
	}
	d := metaFloat(f.Meta, "duration")
	t = math.Min(math.Max(0, t), math.Max(0, d-0.25)) // the last frame starts before the end
	if width <= 0 {
		width = FrameDefaultWidth
	}
	width = min(max(width, frameMinWidth), FrameMaxWidth) &^ 1
	wait := time.NewTimer(2 * time.Second)
	defer wait.Stop()
	select {
	case u.frames <- struct{}{}:
		defer func() { <-u.frames }()
	case <-wait.C:
		return nil, &UploadError{Code: CodeRate, Message: "frame grabs are busy; retry", RetryAfter: time.Second}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return u.o.Frames.Frame(ctx, item, f, t, width)
}

func (u *Uploads) videoItem(ref contentref.ContentRef) (Item, error) {
	item, err := u.item(ref)
	if err != nil {
		return Item{}, err
	}
	if item.Kind().Video == nil {
		return Item{}, uploadErr(CodeInvalid, "kind %q is not a video kind", item.Kind().Name)
	}
	return item, nil
}

// encodedVideo is the named file, or the manifest's first video file, once encoded.
func (u *Uploads) encodedVideo(ctx context.Context, item Item, name string) (File, error) {
	if _, err := item.ManifestKey(); err != nil {
		return File{}, uploadErr(CodeInvalid, "%v", err)
	}
	man, _, err := u.o.Manifests.Get(ctx, item.Ref())
	if errors.Is(err, ErrNotFound) {
		return File{}, uploadErr(CodeNotFound, "%s has no manifest", item.Ref())
	} else if err != nil {
		return File{}, err
	}
	f, ok := VideoFile(man, name)
	if !ok {
		return File{}, uploadErr(CodeNotFound, "no video file %q", name)
	}
	if !Encoded(f) {
		return File{}, uploadErr(CodeConflict, "video %q is not encoded yet", f.Name)
	}
	return f, nil
}

// Encoded reports a video file whose current source has an HLS ladder.
func Encoded(f File) bool {
	_, ok := FrameRendition(f)
	return ok && f.HLS.Source == f.Source() && metaFloat(f.Meta, "duration") > 0
}

// VideoFile is the named file when it is a video, or with name "" the first video file.
func VideoFile(m *Manifest, name string) (File, bool) {
	for _, f := range m.Files {
		if strings.HasPrefix(f.Type, "video/") && (name == "" || f.Name == name) {
			return f, true
		}
	}
	return File{}, false
}

func (u *Uploads) enqueueVideo(ctx context.Context, ref contentref.ContentRef) error {
	if u.o.Queue == nil {
		return nil
	}
	return u.o.Queue.Enqueue(ctx, ProcessJob{Ref: ref})
}

// VideoImages is a video item's poster and hover preview. Selections and
// Video are only in the uploader's reply (POST /video-images).
type VideoImages struct {
	Poster       PosterManifest       `json:"poster"`
	HoverPreview HoverPreviewManifest `json:"hover_preview"`
	Video        *VideoInfo           `json:"video,omitempty"`
	// Progress of the encode the outputs wait on (GET only, with
	// ReaderOptions.Progress): a file's run, then PhaseImages.
	Progress *EncodeProgress `json:"progress,omitempty"`
}

// PosterManifest is the poster slot's manifest plus its selection.
type PosterManifest struct {
	SlotManifest
	Selection *PosterSelection `json:"selection,omitempty"`
}

// PosterSelection is the current poster choice; Source is auto, frame or upload.
type PosterSelection struct {
	Source  string   `json:"source"`
	Version string   `json:"version,omitempty"`
	File    string   `json:"file,omitempty"`
	Time    *float64 `json:"time,omitempty"`
}

// HoverPreviewManifest lists the rendered loops by ascending width, at URLs
// versioned like slot outputs: MP4 (H.264, 2-3× smaller) and animated WebP.
type HoverPreviewManifest struct {
	Selection *HoverPreviewSelection `json:"selection,omitempty"`
	Version   string                 `json:"version,omitempty"`
	MP4       []PreviewImage         `json:"mp4"`
	WebP      []PreviewImage         `json:"webp"`
	Pending   bool                   `json:"pending"`
}

type HoverPreviewSelection struct {
	Version  string  `json:"version,omitempty"`
	File     string  `json:"file"`
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
	Auto     bool    `json:"auto,omitempty"`
}

type PreviewImage struct {
	W   int    `json:"w"`
	H   int    `json:"h"`
	URL string `json:"url"`
}

// VideoInfo is the picker's video: the selected (or first) file of the ref's
// manifest. W×H is the grabbed poster frame's size, the space of frame
// poster edits.
type VideoInfo struct {
	Version  string  `json:"version,omitempty"`
	File     string  `json:"file"`
	Duration float64 `json:"duration"`
	W        int     `json:"w"`
	H        int     `json:"h"`
	Encoded  bool    `json:"encoded"`
}

// HoverPreviewURLs are the public URLs of the smallest hover-preview loop,
// which every rendered preview has; it reads nothing, so listings link
// previews without reads (posters: SlotOutputs with PosterSlot). They answer
// only once the item's Exposure publishes the preview: link them only for
// items anonymous viewers fully see (DefaultExposure), never for drafts or
// paid items. version is HoverPreviewManifest.Version, or "" for URLs
// revalidated on every view.
func (r *Reader) HoverPreviewURLs(ref contentref.ContentRef, version string) (mp4, webp string, err error) {
	item, err := r.kinds.Item(ref.Content())
	if err != nil || item.Kind().Video == nil {
		return "", "", fmt.Errorf("%w: not a video item", ErrNotVisible)
	}
	w := HoverPreviewWidths[0]
	base := r.base.String()
	return previewURL(base, item, w, true, version), previewURL(base, item, w, false, version), nil
}

func previewURL(base string, item Item, w int, mp4 bool, version string) string {
	return versioned(strings.TrimRight(base, "/")+"/"+item.HoverPreviewPublic(w, mp4), version)
}

// VideoImages resolves ref for actor and reads a video item's poster and
// hover preview: ErrNotVisible for an item actor may not see; editors get
// both from editor/, others what the item has published (see Exposure).
func (r *Reader) VideoImages(ctx context.Context, ref contentref.ContentRef, actor access.Actor) (VideoImages, error) {
	urls, err := r.outputURLs(ctx, ref, actor)
	if err != nil {
		return VideoImages{}, err
	}
	out, err := r.manifests.VideoImages(ctx, urls, ref, false, "")
	if err == nil && r.progress != nil {
		if st, perr := r.progress.EncodeProgress(ctx, ref); perr == nil {
			out.Progress = st.Current()
		}
	}
	return out, err
}

// VideoImages builds output URLs with urls. With uploader set it adds the
// selections and, when ref addresses a manifest, the video file (file "" is
// the first).
func (m *Manifests) VideoImages(ctx context.Context, urls OutputURLs, ref contentref.ContentRef, uploader bool, file string) (VideoImages, error) {
	item, err := m.kinds.Item(ref)
	if err != nil || item.Kind().Video == nil {
		return VideoImages{}, fmt.Errorf("%w: not a video item", ErrNotVisible)
	}
	var out VideoImages
	if out.Poster.SlotManifest, err = m.SlotManifest(ctx, urls, ref.Content(), PosterSlot); err != nil {
		return VideoImages{}, err
	}
	out.HoverPreview = HoverPreviewManifest{MP4: []PreviewImage{}, WebP: []PreviewImage{}, Pending: true}
	prev, err := m.HoverPreview(ctx, ref)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return VideoImages{}, err
	}
	editor := urls.EditorToken != ""
	switch {
	case !editor && !urls.Exposure.HoverPreview:
		out.HoverPreview.Pending = false // not this caller's
	case prev != nil && prev.Result != nil:
		res := prev.Result
		out.HoverPreview.Pending = res.Of != prev.Key()
		out.HoverPreview.Version = res.Version
		for _, o := range res.Outputs {
			mp4, webp := previewURL(urls.BaseURL, item, o.W, true, res.Version), previewURL(urls.BaseURL, item, o.W, false, res.Version)
			if editor {
				mp4, webp = urls.editorURL(item.HoverPreviewOutput(o.W, true), res.Version), urls.editorURL(item.HoverPreviewOutput(o.W, false), res.Version)
			}
			out.HoverPreview.MP4 = append(out.HoverPreview.MP4, PreviewImage{o.W, o.H, mp4})
			out.HoverPreview.WebP = append(out.HoverPreview.WebP, PreviewImage{o.W, o.H, webp})
		}
	}
	if !uploader {
		return out, nil
	}
	if prev != nil {
		out.HoverPreview.Selection = &HoverPreviewSelection{Version: prev.Version, File: prev.File, Start: prev.Start, Duration: prev.Duration, Auto: prev.Auto}
	}
	rec, err := m.Slot(ctx, ref.Content(), PosterSlot)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return VideoImages{}, err
	}
	switch {
	case rec == nil:
	case rec.Frame != nil:
		s := &PosterSelection{Source: PosterSourceFrame, Version: rec.Frame.Version, File: rec.Frame.File}
		if rec.Frame.Auto {
			s.Source = PosterSourceAuto
		}
		if !rec.Frame.Auto || rec.Frame.Source != "" {
			t := rec.Frame.Time
			s.Time = &t
		}
		out.Poster.Selection = s
	case rec.Original != "":
		out.Poster.Selection = &PosterSelection{Source: PosterSourceUpload}
	}
	if _, err := item.ManifestKey(); err == nil {
		man, _, err := m.Get(ctx, ref)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return VideoImages{}, err
		}
		if man != nil {
			if f, ok := VideoFile(man, file); ok {
				size := posterFrame(f)
				out.Video = &VideoInfo{Version: ref.Version(), File: f.Name, Duration: metaFloat(f.Meta, "duration"),
					W: size.W, H: size.H, Encoded: Encoded(f)}
			}
		}
	}
	return out, nil
}
