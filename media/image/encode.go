package image

import (
	"errors"
	"fmt"
	"sync"

	"github.com/davidbyttow/govips/v2/vips"

	"github.com/open-rails/contentkit/media"
)

// unbounded stands in for a zero spec dimension (VIPS_MAX_COORD).
const unbounded = 10_000_000

var (
	startOnce sync.Once
	startErr  error
)

// start initialises libvips once per process: one thread per operation
// (Config.Workers parallelises across files) and no operation cache.
func start() error {
	startOnce.Do(func() {
		vips.LoggingSettings(nil, vips.LogLevelError)
		startErr = vips.Startup(&vips.Config{ConcurrencyLevel: 1})
	})
	return startErr
}

// permanentError is a source that cannot become a variant (undecodable, too
// large or missing); retrying cannot help.
type permanentError struct{ err error }

func (e permanentError) Error() string { return "media/image: " + e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func isPermanent(err error) bool {
	var d permanentError
	return errors.As(err, &d)
}

// formats are the declared types decoded and the format their bytes must
// sniff as, so a kind's Types also bound the libvips loaders an upload reaches.
var formats = map[string]vips.ImageType{
	"image/jpeg": vips.ImageTypeJPEG, "image/png": vips.ImageTypePNG, "image/webp": vips.ImageTypeWEBP,
	"image/gif": vips.ImageTypeGIF, "image/avif": vips.ImageTypeAVIF, "image/heic": vips.ImageTypeHEIF,
	"image/heif": vips.ImageTypeHEIF, "image/tiff": vips.ImageTypeTIFF, "image/jxl": vips.ImageTypeJXL,
	"image/bmp": vips.ImageTypeBMP, "image/svg+xml": vips.ImageTypeSVG,
}

// animated are the formats loaded with every frame (libvips n=-1): the frames
// stack into one tall image with a page height, delays and a loop count.
var animated = map[string]bool{"image/gif": true, "image/webp": true}

// rules bound a source: pixels over all frames, frames, running time and the
// kind's or slot's animation policy.
type rules struct {
	maxPixels  int
	maxFrames  int
	maxSeconds float64
	animation  media.Animation
}

// source is a probed upload: one frame's displayed size (EXIF orientation
// applied to stills) and its frame count.
type source struct {
	w, h, frames int
}

func (s source) dims() media.Dims { return media.Dims{W: s.w, H: s.h} }

// probe checks src is contentType and within r without decoding pixels.
func probe(src []byte, contentType string, r rules) (source, error) {
	unreadable := permanentError{&media.ImageError{Code: media.CodeImageUnreadable,
		Message: fmt.Sprintf("the file is not a readable %s image", contentType), Details: media.ErrorDetails{Type: contentType}}}
	if want, ok := formats[contentType]; !ok || vips.DetermineImageType(src) != want {
		return source{}, unreadable
	}
	if isImageSequence(src) {
		// libheif decodes only the first frame of an AVIF/HEIF sequence: refuse rather than flatten.
		if r.animation == media.AnimationReject {
			return source{}, notAllowed()
		}
		return source{}, permanentError{&media.ImageError{Code: media.CodeAnimationUnsupported,
			Message: fmt.Sprintf("animated %s images are not supported yet; upload a GIF or animated WebP", contentType),
			Details: media.ErrorDetails{Type: contentType}}}
	}
	img, err := decode(src, contentType)
	if err != nil {
		return source{}, unreadable
	}
	defer img.Close()
	s := source{w: img.Width(), h: img.PageHeight(), frames: 1}
	if s.h > 0 {
		s.frames = max(1, img.Height()/s.h)
	}
	if s.w <= 0 || s.h <= 0 {
		return source{}, unreadable
	}
	if s.frames > 1 {
		if r.animation == media.AnimationReject {
			return source{}, notAllowed()
		}
		if r.maxFrames > 0 && s.frames > r.maxFrames {
			return source{}, permanentError{&media.ImageError{Code: media.CodeAnimationTooLong,
				Message: fmt.Sprintf("animations may have at most %d frames; this one has %d", r.maxFrames, s.frames),
				Details: media.ErrorDetails{Frames: s.frames, MaxFrames: r.maxFrames}}}
		}
		delays, _ := img.PageDelay()
		ms := 0
		for _, d := range delays {
			ms += d
		}
		if secs := float64(ms) / 1000; r.maxSeconds > 0 && secs > r.maxSeconds {
			return source{}, permanentError{&media.ImageError{Code: media.CodeAnimationTooLong,
				Message: fmt.Sprintf("animations may run at most %g seconds; this one runs %.1f", r.maxSeconds, secs),
				Details: media.ErrorDetails{Seconds: secs, MaxSeconds: r.maxSeconds}}}
		}
	}
	if s.w*s.h*s.frames > r.maxPixels {
		msg := fmt.Sprintf("images may have at most %d pixels; this one is %dx%d", r.maxPixels, s.w, s.h)
		if s.frames > 1 {
			msg = fmt.Sprintf("animations may have at most %d pixels over all frames; this one is %d frames of %dx%d", r.maxPixels, s.frames, s.w, s.h)
		}
		return source{}, permanentError{&media.ImageError{Code: media.CodeImageTooLarge, Message: msg,
			Details: media.ErrorDetails{Width: s.w, Height: s.h, Frames: s.frames, MaxPixels: r.maxPixels}}}
	}
	if o := img.Orientation(); s.frames == 1 && o >= 5 && o <= 8 {
		s.w, s.h = s.h, s.w
	}
	return s, nil
}

func notAllowed() error {
	return permanentError{&media.ImageError{Code: media.CodeAnimationNotAllowed,
		Message: "animated images are not allowed here; upload a still image"}}
}

// isImageSequence reports an AVIF/HEIF image sequence (ftyp brand avis or msf1/hevs).
func isImageSequence(src []byte) bool {
	if len(src) < 16 || string(src[4:8]) != "ftyp" {
		return false
	}
	n := int(src[0])<<24 | int(src[1])<<16 | int(src[2])<<8 | int(src[3])
	if n < 16 || n > len(src) {
		n = min(len(src), 64)
	}
	for i := 8; i+4 <= n; i += 4 {
		switch string(src[i : i+4]) {
		case "avis", "msf1", "hevs":
			return true
		}
	}
	return false
}

// decode loads src lazily: every frame of an animated format, the first page otherwise.
func decode(src []byte, contentType string) (*vips.ImageRef, error) {
	p := vips.NewImportParams()
	if animated[contentType] {
		p.NumPages.Set(-1)
	}
	return vips.LoadImageFromBuffer(src, p)
}

// open decodes src for editing: a still turned upright by its EXIF
// orientation, an animation as its frames (animations carry no orientation).
func open(src []byte, contentType string) (*vips.ImageRef, error) {
	img, err := decode(src, contentType)
	if err != nil {
		return nil, err
	}
	if frames(img) == 1 {
		if err := img.AutoRotate(); err != nil {
			img.Close()
			return nil, err
		}
	}
	return img, nil
}

func frames(img *vips.ImageRef) int {
	if ph := img.PageHeight(); ph > 0 && img.Height() > ph {
		return img.Height() / ph
	}
	return 1
}

// eachFrame applies fn to every frame and restacks them with the source's
// delays and loop; a still is fn(img). The result replaces img, which it closes.
func eachFrame(img *vips.ImageRef, fn func(*vips.ImageRef) error) (*vips.ImageRef, error) {
	n := frames(img)
	if n == 1 {
		return img, fn(img)
	}
	defer img.Close()
	ph, w := img.PageHeight(), img.Width()
	delay, _ := img.PageDelay()
	loop := img.Loop()
	flat, err := img.Copy()
	if err != nil {
		return nil, err
	}
	defer flat.Close()
	// One page as tall as the strip: ExtractArea then cuts plain rectangles.
	if err := flat.SetPageHeight(flat.Height()); err != nil {
		return nil, err
	}
	pages := make([]*vips.ImageRef, 0, n)
	defer func() {
		for _, p := range pages[1:] {
			p.Close()
		}
	}()
	for i := range n {
		p, err := flat.Copy()
		if err == nil {
			err = p.ExtractArea(0, i*ph, w, ph)
		}
		if err == nil {
			err = p.SetPages(1) // a still to the frame-aware ops (Rotate)
		}
		if err == nil {
			err = p.SetPageHeight(ph)
		}
		if err == nil {
			err = fn(p)
		}
		if p != nil {
			pages = append(pages, p)
		}
		if err != nil {
			if len(pages) > 0 {
				pages[0].Close()
			}
			return nil, err
		}
	}
	out := pages[0]
	fh := out.Height()
	if err := out.ArrayJoin(pages[1:], 1); err != nil {
		out.Close()
		return nil, err
	}
	err = out.SetPageHeight(fh)
	if err == nil {
		err = out.SetPages(n)
	}
	if err == nil && len(delay) == n {
		err = out.SetPageDelay(delay)
	}
	if err == nil {
		err = out.SetLoop(loop)
	}
	if err != nil {
		out.Close()
		return nil, err
	}
	return out, nil
}

// webp encodes img, every frame of an animation included.
func webp(img *vips.ImageRef, quality int) ([]byte, error) {
	p := vips.NewWebpExportParams()
	p.Quality = quality
	if p.Quality <= 0 {
		p.Quality = 80
	}
	p.StripMetadata = true
	out, _, err := img.ExportWebp(p)
	return out, err
}

// encode derives one WebP from src through edit (unless the spec is
// Unedited) per spec, frame by frame. Inside never enlarges; cover fills the
// box and crops the centre; a zero box keeps full resolution.
func encode(src []byte, contentType string, s media.Spec, edit *media.Edit) ([]byte, media.Dims, error) {
	if s.Unedited {
		edit = nil
	}
	full := s.Width == 0 && s.Height == 0
	w, h := orUnbounded(s.Width), orUnbounded(s.Height)
	crop, size := vips.InterestingNone, vips.SizeDown
	if s.Fit == media.FitCover && s.Width > 0 && s.Height > 0 {
		crop, size = vips.InterestingCentre, vips.SizeBoth
	}
	var img *vips.ImageRef
	var err error
	if edit == nil && !full && !animated[contentType] {
		img, err = vips.NewThumbnailWithSizeFromBuffer(src, w, h, crop, size) // shrink on load
		if err == nil && s.Blur > 0 {
			err = img.GaussianBlur(s.Blur)
		}
	} else if img, err = open(src, contentType); err == nil {
		img, err = eachFrame(img, func(f *vips.ImageRef) error {
			err := apply(f, edit)
			if err == nil && !full {
				err = f.ThumbnailWithSize(w, h, crop, size)
			}
			if err == nil && s.Blur > 0 {
				err = f.GaussianBlur(s.Blur)
			}
			return err
		})
	}
	if img != nil {
		defer img.Close()
	}
	if err != nil {
		return nil, media.Dims{}, permanentError{err}
	}
	out, err := webp(img, s.Quality)
	if err != nil {
		return nil, media.Dims{}, permanentError{err}
	}
	return out, media.Dims{W: img.Width(), H: img.PageHeight()}, nil
}

// slotOutput is one encoded slot width.
type slotOutput struct {
	webp []byte
	dims media.Dims
}

// encodeSlot checks src is contentType and within r, decodes it, applies its
// EXIF orientation and the slot's resolved edit, and encodes every width that
// fits the edited image, never upscaling; animations stay animated. dims is
// the oriented source's (one frame's) size once known.
func encodeSlot(src []byte, contentType string, s media.Slot, edit *media.Edit, r rules) (map[int]slotOutput, media.Dims, error) {
	var dims media.Dims
	info, err := probe(src, contentType, r)
	if err != nil {
		return nil, dims, err
	}
	dims = info.dims()
	if edit, err = s.Resolve(edit, dims.W, dims.H); err != nil {
		return nil, dims, permanentError{fmt.Errorf("edit: %w", err)}
	}
	img, err := open(src, contentType)
	if err == nil {
		img, err = eachFrame(img, func(f *vips.ImageRef) error { return apply(f, edit) })
	}
	if err != nil {
		return nil, dims, permanentError{err}
	}
	defer img.Close()
	edited := media.Dims{W: img.Width(), H: img.PageHeight()}
	outs := map[int]slotOutput{}
	var last slotOutput
	for _, rung := range s.Widths {
		d := s.Size(s.OutputWidth(rung, edited.W), edited)
		if d == last.dims {
			outs[rung] = last // rungs past the edited width share its bytes
			continue
		}
		out, err := img.Copy()
		if err == nil {
			out, err = eachFrame(out, func(f *vips.ImageRef) error {
				return f.ThumbnailWithSize(d.W, d.H, vips.InterestingNone, vips.SizeForce)
			})
		}
		var b []byte
		if err == nil {
			b, err = webp(out, s.Quality)
		}
		if out != nil {
			out.Close()
		}
		if err != nil {
			return nil, dims, permanentError{err}
		}
		last = slotOutput{b, d}
		outs[rung] = last
	}
	return outs, dims, nil
}

func apply(img *vips.ImageRef, e *media.Edit) error {
	if e == nil {
		return nil
	}
	if err := e.Check(img.Width(), img.Height()); err != nil {
		return err
	}
	if c := e.Crop; c != nil {
		if err := img.ExtractArea(c.X, c.Y, c.W, c.H); err != nil {
			return err
		}
	}
	switch e.Rotate {
	case 90:
		return img.Rotate(vips.Angle90)
	case 180:
		return img.Rotate(vips.Angle180)
	case 270:
		return img.Rotate(vips.Angle270)
	}
	return nil
}

func orUnbounded(n int) int {
	if n <= 0 {
		return unbounded
	}
	return n
}
