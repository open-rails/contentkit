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

// probe returns the displayed size of src (EXIF orientation applied) without
// decoding its pixels, refusing sources over maxPixels.
func probe(src []byte, maxPixels int) (w, h int, err error) {
	img, err := vips.NewImageFromBuffer(src)
	if err != nil {
		return 0, 0, permanentError{err}
	}
	defer img.Close()
	w, h = img.Width(), img.Height()
	if w <= 0 || h <= 0 || w*h > maxPixels {
		return 0, 0, permanentError{fmt.Errorf("%dx%d exceeds %d pixels", w, h, maxPixels)}
	}
	if o := img.Orientation(); o >= 5 && o <= 8 {
		w, h = h, w
	}
	return w, h, nil
}

// encode derives one WebP from src per spec. Inside never enlarges; cover
// fills the box and crops the centre; a zero box keeps full resolution.
func encode(src []byte, s media.Spec) ([]byte, error) {
	var (
		img *vips.ImageRef
		err error
	)
	if s.Width == 0 && s.Height == 0 {
		if img, err = vips.NewImageFromBuffer(src); err == nil {
			err = img.AutoRotate()
		}
	} else {
		w, h := orUnbounded(s.Width), orUnbounded(s.Height)
		crop, size := vips.InterestingNone, vips.SizeDown
		if s.Fit == media.FitCover && s.Width > 0 && s.Height > 0 {
			crop, size = vips.InterestingCentre, vips.SizeBoth
		}
		img, err = vips.NewThumbnailWithSizeFromBuffer(src, w, h, crop, size)
	}
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

func orUnbounded(n int) int {
	if n <= 0 {
		return unbounded
	}
	return n
}
