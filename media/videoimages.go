package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// PosterSlot is the video kinds' item-level cover. Players preview the video
// itself (the SDK plays its HLS inline), so there is no separate preview clip.
const PosterSlot = "poster"

// VideoPoster is the default poster slot (Video.Poster): every video kind
// gets one at its Video.PosterWidths. Its original is an uploaded image or a
// frame the video worker grabbed; the image job encodes either through the
// slot's Edit. It is native: the poster keeps the frame's (or upload's) own
// aspect unless an edit crops it.
var VideoPoster = (*Video)(nil).Poster()

// Poster is the item's poster slot (zero for non-video kinds).
func (i Item) Poster() Slot { return i.Kind().Slots[PosterSlot] }

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
// as is, or upscaled until it reaches the poster slot's Min() wide, so every
// poster has its smallest width. Poster edits are in these pixels.
func PosterFrameSize(poster Slot, w, h int) Dims {
	if min := float64(poster.Min()); w > 0 && float64(w) < min {
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

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

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
		return u.CommitSlot(ctx, actor, SlotCommit{Ref: ref.Content(), Slot: PosterSlot, SHA256: r.SHA256, Edit: r.Edit})
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
		size := posterFrame(item, f)
		if _, err := item.Poster().Resolve(r.Edit, size.W, size.H); err != nil {
			return editErr(err, "edit: %v")
		}
		edit = item.Poster().fit(r.Edit)
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
func posterFrame(item Item, f File) Dims {
	r, _ := FrameRendition(f)
	return PosterFrameSize(item.Poster(), r.Width, r.Height)
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
	if _, err := item.Section(); err != nil {
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

// VideoFile is the named file when it is a video, or with name "" the first
// attached video file.
func VideoFile(m *Manifest, name string) (File, bool) {
	for _, f := range m.Files {
		if strings.HasPrefix(f.Type, "video/") && (name == "" && !f.Unattached || name != "" && f.Name == name) {
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

// VideoImages is a video item's poster. Selections and Video are only in the
// uploader's reply (POST /video-images).
type VideoImages struct {
	Poster PosterManifest `json:"poster"`
	Video  *VideoInfo     `json:"video,omitempty"`
	// Progress of the encode the outputs wait on (GET only, with
	// ReaderOptions.Progress): a file's run, then PhaseImages.
	Progress *EncodeProgress `json:"progress,omitempty"`
}

// PosterManifest is the poster slot's manifest plus its selection. File and
// Time are the video and second a frame poster was cut from, for every caller
// that sees the poster: a gallery draws it on that video only and starts its
// inline preview there ("" for an uploaded poster, which belongs to the first
// video).
type PosterManifest struct {
	SlotManifest
	File      string           `json:"file,omitempty"`
	Time      *float64         `json:"time,omitempty"`
	Selection *PosterSelection `json:"selection,omitempty"`
}

// PosterSelection is the current poster choice; Source is auto, frame or upload.
type PosterSelection struct {
	Source  string   `json:"source"`
	Version string   `json:"version,omitempty"`
	File    string   `json:"file,omitempty"`
	Time    *float64 `json:"time,omitempty"`
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

// VideoImages resolves ref for actor and reads a video item's poster:
// ErrNotVisible for an item actor may not see; editors also see a hidden
// item's poster.
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
	rec, err := func() (rec *SlotRecord, err error) {
		out.Poster.SlotManifest, rec, err = m.slotManifest(ctx, urls, ref.Content(), PosterSlot)
		return rec, err
	}()
	if err != nil {
		return VideoImages{}, err
	}
	if rec != nil && rec.Frame != nil {
		out.Poster.File = rec.Frame.File
		if !rec.Frame.Auto || rec.Frame.Source != "" {
			t := rec.Frame.Time
			out.Poster.Time = &t
		}
	}
	if !uploader {
		return out, nil
	}
	switch {
	case rec == nil:
	case rec.Frame != nil:
		s := &PosterSelection{Source: PosterSourceFrame, Version: rec.Frame.Version, File: rec.Frame.File}
		if rec.Frame.Auto {
			s.Source = PosterSourceAuto
		}
		s.Time = out.Poster.Time
		out.Poster.Selection = s
	case rec.Original != "":
		out.Poster.Selection = &PosterSelection{Source: PosterSourceUpload}
	}
	if _, err := item.Section(); err == nil {
		man, _, err := m.Get(ctx, ref)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return VideoImages{}, err
		}
		if man != nil {
			if f, ok := VideoFile(man, file); ok {
				size := posterFrame(item, f)
				out.Video = &VideoInfo{Version: ref.Version(), File: f.Name, Duration: metaFloat(f.Meta, "duration"),
					W: size.W, H: size.H, Encoded: Encoded(f)}
			}
		}
	}
	return out, nil
}
