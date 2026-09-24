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

// probe returns the displayed size of src (EXIF orientation applied) without
// decoding its pixels, refusing bytes that are not contentType and sources
// over maxPixels.
func probe(src []byte, contentType string, maxPixels int) (w, h int, err error) {
	unreadable := permanentError{&media.ImageError{Code: media.CodeImageUnreadable,
		Message: fmt.Sprintf("the file is not a readable %s image", contentType), Details: media.ErrorDetails{Type: contentType}}}
	if want, ok := formats[contentType]; !ok || vips.DetermineImageType(src) != want {
		return 0, 0, unreadable
	}
	img, err := vips.NewImageFromBuffer(src)
	if err != nil {
		return 0, 0, unreadable
	}
	defer img.Close()
	w, h = img.Width(), img.Height()
	if w <= 0 || h <= 0 {
		return 0, 0, unreadable
	}
	if w*h > maxPixels {
		return 0, 0, permanentError{&media.ImageError{Code: media.CodeImageTooLarge,
			Message: fmt.Sprintf("images may have at most %d pixels; this one is %dx%d", maxPixels, w, h),
			Details: media.ErrorDetails{Width: w, Height: h, MaxPixels: maxPixels}}}
	}
	if o := img.Orientation(); o >= 5 && o <= 8 {
		w, h = h, w
	}
	return w, h, nil
}

// encode derives one WebP from src through edit (unless the spec is
// Unedited) per spec. Inside never enlarges; cover fills the box and crops the
// centre; a zero box keeps full resolution.
func encode(src []byte, s media.Spec, edit *media.Edit) ([]byte, error) {
	if s.Unedited {
		edit = nil
	}
	img, err := load(src, s, edit)
	if img != nil {
		defer img.Close()
	}
	if err == nil && s.Blur > 0 {
		err = img.GaussianBlur(s.Blur)
	}
	if err != nil {
		return nil, permanentError{err}
	}
	p := vips.NewWebpExportParams()
	p.Quality = s.Quality
	if p.Quality <= 0 {
		p.Quality = 80
	}
	p.StripMetadata = true
	out, _, err := img.ExportWebp(p)
	if err != nil {
		return nil, permanentError{err}
	}
	return out, nil
}

// load decodes src, applies edit and sizes it to s. Without an edit, a sized
// spec shrinks on load.
func load(src []byte, s media.Spec, edit *media.Edit) (*vips.ImageRef, error) {
	full := s.Width == 0 && s.Height == 0
	w, h := orUnbounded(s.Width), orUnbounded(s.Height)
	crop, size := vips.InterestingNone, vips.SizeDown
	if s.Fit == media.FitCover && s.Width > 0 && s.Height > 0 {
		crop, size = vips.InterestingCentre, vips.SizeBoth
	}
	if edit == nil && !full {
		return vips.NewThumbnailWithSizeFromBuffer(src, w, h, crop, size)
	}
	img, err := vips.NewImageFromBuffer(src)
	if err != nil {
		return nil, err
	}
	if err = img.AutoRotate(); err == nil && edit != nil {
		err = apply(img, edit)
	}
	if err == nil && !full {
		err = img.ThumbnailWithSize(w, h, crop, size)
	}
	return img, err
}

// slotOutput is one encoded slot width.
type slotOutput struct {
	webp []byte
	dims media.Dims
}

// encodeSlot checks src is contentType, decodes it, applies its EXIF orientation and the slot's
// resolved edit, and encodes every width that fits the edited image, never
// upscaling. dims is the oriented source's size once known.
func encodeSlot(src []byte, contentType string, s media.Slot, edit *media.Edit, maxPixels int) (map[int]slotOutput, media.Dims, error) {
	var dims media.Dims
	w, h, err := probe(src, contentType, maxPixels)
	if err != nil {
		return nil, dims, err
	}
	dims = media.Dims{W: w, H: h}
	if edit, err = s.Resolve(edit, w, h); err != nil {
		return nil, dims, permanentError{fmt.Errorf("edit: %w", err)}
	}
	img, err := vips.NewImageFromBuffer(src)
	if err != nil {
		return nil, dims, permanentError{err}
	}
	defer img.Close()
	if err = img.AutoRotate(); err == nil {
		err = apply(img, edit)
	}
	if err != nil {
		return nil, dims, permanentError{err}
	}
	q := s.Quality
	if q <= 0 {
		q = 80
	}
	outs := map[int]slotOutput{}
	for _, width := range s.OutputWidths(img.Width()) {
		d := s.Size(width, media.Dims{W: img.Width(), H: img.Height()})
		out, err := img.Copy()
		if err == nil {
			err = out.ThumbnailWithSize(d.W, d.H, vips.InterestingNone, vips.SizeForce)
		}
		var b []byte
		if err == nil {
			p := vips.NewWebpExportParams()
			p.Quality, p.StripMetadata = q, true
			b, _, err = out.ExportWebp(p)
		}
		if out != nil {
			out.Close()
		}
		if err != nil {
			return nil, dims, permanentError{err}
		}
		outs[s.Rung(width)] = slotOutput{b, d}
	}
	return outs, dims, nil
}

func apply(img *vips.ImageRef, e *media.Edit) error {
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
