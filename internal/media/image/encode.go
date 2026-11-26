package image

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

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
// (Config.Workers parallelises across files), no operation cache, and every
// loader blocked but the formats' trusted ones.
func start() error {
	startOnce.Do(func() {
		vips.LoggingSettings(nil, vips.LogLevelError)
		if startErr = vips.Startup(&vips.Config{ConcurrencyLevel: 1}); startErr == nil {
			startErr = blockLoaders()
		}
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

// formats are the format the bytes of each of media.ImageTypes must sniff
// as, so a kind's Types also bound the libvips loaders an upload reaches.
var formats = map[string]vips.ImageType{
	"image/jpeg": vips.ImageTypeJPEG, "image/png": vips.ImageTypePNG, "image/webp": vips.ImageTypeWEBP,
	"image/gif": vips.ImageTypeGIF, "image/avif": vips.ImageTypeAVIF, "image/heic": vips.ImageTypeHEIF,
	"image/heif": vips.ImageTypeHEIF, "image/tiff": vips.ImageTypeTIFF,
}

// animated are the formats loaded with every frame (libvips n=-1): the frames
// stack into one tall image with a page height, delays and a loop count.
var animated = map[string]bool{"image/gif": true, "image/webp": true}

// rules bound a source: pixels over all frames, frames, running time and the
// preset's animation policy.
type rules struct {
	maxPixels  int
	maxFrames  int
	maxSeconds float64
	animation  media.Animation
}

// source is a probed upload: one frame's displayed size (EXIF orientation
// applied to stills), its frame count and a still's EXIF orientation.
type source struct {
	w, h, frames, orientation int
}

func (s source) dims() media.Dims { return media.Dims{W: s.w, H: s.h} }

// probe checks src is contentType and within r without decoding pixels.
func probe(src []byte, contentType string, r rules) (source, error) {
	unreadable := permanentError{&media.ImageError{Code: media.CodeImageUnreadable,
		Message: fmt.Sprintf("the file is not a readable %s image", contentType), Details: media.ErrorDetails{Type: contentType}}}
	if want, ok := formats[contentType]; !ok || !slices.Contains(media.ImageTypes, contentType) || vips.DetermineImageType(src) != want {
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
	if o := img.Orientation(); s.frames == 1 && o >= 2 && o <= 8 {
		s.orientation = o
		if o >= 5 {
			s.w, s.h = s.h, s.w
		}
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

// webp encodes img, every frame of an animation included, and closes it.
func webp(img *vips.ImageRef, quality int) ([]byte, media.Dims, error) {
	defer img.Close()
	p := vips.NewWebpExportParams()
	p.Quality = quality
	if p.Quality <= 0 {
		p.Quality = 80
	}
	p.StripMetadata = true
	out, _, err := img.ExportWebp(p)
	if err != nil {
		return nil, media.Dims{}, permanentError{err}
	}
	return out, media.Dims{W: img.Width(), H: img.PageHeight()}, nil
}

// fitted is the size s renders a w×h frame at; nothing is enlarged. Inside
// fits the box (a zero side unbounded). Cover fills it, cropping the centre,
// or, from a frame smaller than the box, is the frame's largest centred crop
// at the box's aspect.
func fitted(s media.Image, w, h int) (int, int) {
	if s.Fit == media.FitCover && s.Width > 0 && s.Height > 0 {
		switch {
		case w >= s.Width && h >= s.Height:
			return s.Width, s.Height
		case w*s.Height > h*s.Width:
			return max(1, min(w, (h*s.Width+s.Height/2)/s.Height)), h
		default:
			return w, max(1, min(h, (w*s.Height+s.Width/2)/s.Width))
		}
	}
	bw, bh := orUnbounded(s.Width), orUnbounded(s.Height)
	switch {
	case w <= bw && h <= bh:
		return w, h
	case w*bh > h*bw:
		return bw, max(1, (h*bw+w/2)/w)
	default:
		return max(1, (w*bh+h/2)/h), bh
	}
}

// resize puts one frame into s's box (fitted).
func resize(f *vips.ImageRef, s media.Image) error {
	if s.Fit != media.FitCover || s.Width <= 0 || s.Height <= 0 {
		return f.ThumbnailWithSize(orUnbounded(s.Width), orUnbounded(s.Height), vips.InterestingNone, vips.SizeDown)
	}
	w, h := f.Width(), f.Height()
	if tw, th := fitted(s, w, h); tw < s.Width || th < s.Height {
		return f.ExtractArea((w-tw)/2, (h-th)/2, tw, th)
	}
	return f.ThumbnailWithSize(s.Width, s.Height, vips.InterestingCentre, vips.SizeDown)
}

// transform is every rendering's one pipeline: each frame through edit,
// into s's box (a zero box keeps its size). The result replaces img, which
// it closes on error.
func transform(img *vips.ImageRef, edit *media.Edit, s media.Image) (*vips.ImageRef, error) {
	out, err := eachFrame(img, func(f *vips.ImageRef) error {
		err := apply(f, edit)
		if err == nil && (s.Width > 0 || s.Height > 0) {
			err = resize(f, s)
		}
		return err
	})
	if err != nil {
		if out != nil {
			out.Close()
		}
		return nil, permanentError{err}
	}
	return out, nil
}

// output refuses a rendering of info's frames at w×h when, every frame
// together, it would pass r's pixels: checked before anything decodes.
func (r rules) output(info source, w, h int) error {
	if info.frames*w*h <= r.maxPixels {
		return nil
	}
	return permanentError{&media.ImageError{Code: media.CodeImageTooLarge,
		Message: fmt.Sprintf("renderings may have at most %d pixels over all frames; this one would be %d frames of %dx%d", r.maxPixels, info.frames, w, h),
		Details: media.ErrorDetails{Width: w, Height: h, Frames: info.frames, MaxPixels: r.maxPixels}}}
}

// rendered is the size s renders info through edit at.
func rendered(info source, edit *media.Edit, s media.Image) (int, int) {
	w, h := edit.Size(info.w, info.h)
	return fitted(s, w, h)
}

// load decodes src for renderings needing at most need of the edited image:
// a JPEG or WebP still decodes at a fraction (shrink-on-load) still over
// need, its edit scaled to match; anything else opens whole.
func load(src []byte, contentType string, info source, edit *media.Edit, need media.Dims) (*vips.ImageRef, *media.Edit, error) {
	ew, eh := edit.Size(info.w, info.h)
	if info.frames != 1 || need.W <= 0 || need.H <= 0 || ew <= 0 || eh <= 0 {
		img, err := open(src, contentType)
		return img, edit, err
	}
	// Four edited pixels of headroom survive the load's and crop's rounding.
	scale := min(1, max(float64(need.W)/float64(ew), float64(need.H)/float64(eh))+4/float64(min(ew, eh)))
	p := vips.NewImportParams()
	switch {
	case scale > 0.5:
		img, err := open(src, contentType)
		return img, edit, err
	case contentType == "image/jpeg":
		f := 2 // libjpeg shrinks by 2, 4 or 8
		for f < 8 && scale*float64(2*f) <= 1 {
			f *= 2
		}
		p.JpegShrinkFactor.Set(f)
	case contentType == "image/webp":
		p.WebpScaleFactor.Set(scale)
	default:
		img, err := open(src, contentType)
		return img, edit, err
	}
	img, err := vips.LoadImageFromBuffer(src, p)
	if err != nil {
		return nil, nil, err
	}
	if err := img.AutoRotate(); err != nil {
		img.Close()
		return nil, nil, err
	}
	return img, scaleEdit(edit, info, img.Width(), img.Height()), nil
}

// scaleEdit is e for its source shrunk from info's size to w×h.
func scaleEdit(e *media.Edit, info source, w, h int) *media.Edit {
	if e = e.Normalize(); e == nil || e.Crop == nil {
		return e
	}
	c := *e.Crop
	x0, y0 := c.X*w/info.w, c.Y*h/info.h
	x1, y1 := min(w, (c.X+c.W)*w/info.w), min(h, (c.Y+c.H)*h/info.h)
	return &media.Edit{Rotate: e.Rotate, Crop: &media.Crop{X: x0, Y: y0, W: max(1, x1-x0), H: max(1, y1-y0)}}
}

// encode renders src (probed as info) through edit per spec (nil edit: the
// whole source), shrinking a still on load. A source-format no-op copies its
// bytes without their metadata (stripMetadata), unless s keeps it.
func encode(src []byte, contentType string, info source, s media.Image, edit *media.Edit, r rules) ([]byte, media.Dims, error) {
	if s.Format == media.ImageFormatSource && edit.Normalize() == nil && s.Fit == media.FitInside && s.Aspect.Native() &&
		(s.Width == 0 || info.w <= s.Width) && (s.Height == 0 || info.h <= s.Height) {
		if s.KeepMetadata {
			return src, info.dims(), nil
		}
		out, err := stripMetadata(src, contentType, info.orientation)
		if err != nil {
			return nil, media.Dims{}, permanentError{err}
		}
		return out, info.dims(), nil
	}
	w, h := rendered(info, edit, s)
	if err := r.output(info, w, h); err != nil {
		return nil, media.Dims{}, err
	}
	var img *vips.ImageRef
	var err error
	if edit.Normalize() == nil && info.frames == 1 && (s.Width > 0 || s.Height > 0) {
		// One sequential pass straight to the rendered size.
		crop := vips.InterestingNone
		if s.Fit == media.FitCover && s.Width > 0 && s.Height > 0 {
			crop = vips.InterestingCentre
		} else {
			w, h = orUnbounded(s.Width), orUnbounded(s.Height)
		}
		img, err = vips.NewThumbnailWithSizeFromBuffer(src, w, h, crop, vips.SizeDown)
		s.Width, s.Height = 0, 0 // sized
	} else {
		img, edit, err = load(src, contentType, info, edit, media.Dims{W: w, H: h})
	}
	if err != nil {
		return nil, media.Dims{}, permanentError{err}
	}
	if img, err = transform(img, edit, s); err != nil {
		return nil, media.Dims{}, err
	}
	if s.Format == media.ImageFormatSource {
		return originalFormat(img, contentType, s.Quality)
	}
	return webp(img, s.Quality)
}

func outputType(s media.Image, source string) string {
	if s.Format == media.ImageFormatSource {
		return source
	}
	return "image/webp"
}

// originalFormat writes only an oversized or explicitly edited source. Normal
// sources bypass encoding above. The archival upload is never replaced.
func originalFormat(img *vips.ImageRef, contentType string, quality int) ([]byte, media.Dims, error) {
	if contentType == "image/webp" {
		return webp(img, quality)
	}
	defer img.Close()
	if quality <= 0 {
		quality = 80
	}
	var out []byte
	var err error
	switch contentType {
	case "image/jpeg":
		p := vips.NewJpegExportParams()
		p.Quality, p.StripMetadata = quality, true
		out, _, err = img.ExportJpeg(p)
	case "image/png":
		p := vips.NewPngExportParams()
		p.StripMetadata = true
		out, _, err = img.ExportPng(p)
	case "image/gif":
		out, _, err = img.ExportGIF(vips.NewGifExportParams())
	default:
		return nil, media.Dims{}, permanentError{fmt.Errorf("resizing source format %q is not supported", contentType)}
	}
	if err != nil {
		return nil, media.Dims{}, permanentError{err}
	}
	return out, media.Dims{W: img.Width(), H: img.PageHeight()}, nil
}

// rendition is one encoded public name.
type rendition struct {
	webp []byte
	dims media.Dims
}

// encodePublic checks src is contentType and within r, decodes it, applies
// its EXIF orientation and the preset's resolved edit, and encodes each of
// names (one per width, or one at the image's box without widths) through
// the same transform as every rendering. dims is the oriented source's (one
// frame's) size once known. Animations stay animated.
func encodePublic(src []byte, contentType string, p *media.Public, names []string, edit *media.Edit, r rules) (map[string]rendition, media.Dims, error) {
	var dims media.Dims
	info, err := probe(src, contentType, r)
	if err != nil {
		return nil, dims, err
	}
	dims = info.dims()
	if edit, err = p.Image.Resolve(edit, dims.W, dims.H); err != nil {
		return nil, dims, permanentError{fmt.Errorf("edit: %w", err)}
	}
	if len(p.Widths) == 0 {
		out, d, err := encode(src, contentType, info, p.Image, edit, r)
		if err != nil {
			return nil, dims, err
		}
		return map[string]rendition{names[0]: {out, d}}, dims, nil
	}
	ew, eh := edit.Size(dims.W, dims.H)
	edited := media.Dims{W: ew, H: eh}
	// Every rung's box and size is known before anything decodes.
	specs, sizes := make([]media.Image, len(p.Widths)), make([]media.Dims, len(p.Widths))
	var need media.Dims
	for i, rung := range p.Widths {
		specs[i] = atWidth(p.Image, rung, edited)
		w, h := fitted(specs[i], ew, eh)
		if err := r.output(info, w, h); err != nil {
			return nil, dims, err
		}
		sizes[i], need = media.Dims{W: w, H: h}, media.Dims{W: max(need.W, w), H: max(need.H, h)}
	}
	img, edit, err := load(src, contentType, info, edit, need)
	if err != nil {
		return nil, dims, permanentError{err}
	}
	if img, err = transform(img, edit, media.Image{}); err != nil {
		return nil, dims, err
	}
	defer img.Close()
	outs := map[string]rendition{}
	for i := range p.Widths {
		s := specs[i]
		if i > 0 && sizes[i] == sizes[i-1] && s.Quality == specs[i-1].Quality {
			outs[names[i]] = outs[names[i-1]] // widths past the edited image share its bytes
			continue
		}
		out, err := img.Copy()
		if err != nil {
			return nil, dims, permanentError{err}
		}
		if out, err = transform(out, nil, s); err != nil {
			return nil, dims, err
		}
		b, d, err := webp(out, s.Quality)
		if err != nil {
			return nil, dims, err
		}
		outs[names[i]] = rendition{b, d}
	}
	return outs, dims, nil
}

// atWidth is a Widths preset's image at one width: its box is the width,
// never past the edited image's (nothing is upscaled), at the shape of
// im's Width×Height (fitted per Fit) or else at the edit's own aspect,
// which it fills exactly.
func atWidth(im media.Image, width int, edited media.Dims) media.Image {
	s := im
	s.Width = min(width, edited.W)
	if im.Width > 0 && im.Height > 0 {
		s.Height = max(1, (s.Width*im.Height+im.Width/2)/im.Width)
		return s
	}
	aspect := im.Aspect
	if aspect.Native() {
		aspect = mediastore.AspectOf(edited.W, edited.H)
	}
	s.Height, s.Fit = aspect.Height(s.Width), media.FitCover
	return s
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

// Lossless re-encodes an 8-bit image as lossless WebP without its metadata:
// the form a hook's changed master is stored in.
func Lossless(src []byte) ([]byte, error) {
	if err := start(); err != nil {
		return nil, err
	}
	img, err := vips.NewImageFromBuffer(src)
	if err != nil {
		return nil, permanentError{err}
	}
	defer img.Close()
	p := vips.NewWebpExportParams()
	p.Lossless, p.StripMetadata = true, true
	out, _, err := img.ExportWebp(p)
	if err != nil {
		return nil, permanentError{err}
	}
	return out, nil
}
