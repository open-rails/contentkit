package media

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
)

// Edit is a non-destructive image edit: Crop, in the source's pixels (EXIF
// orientation applied), then a clockwise Rotate. Variants derive from the
// source through it; the source is never modified.
type Edit struct {
	Crop   *Crop `json:"crop,omitempty"`
	Rotate int   `json:"rotate,omitempty"` // clockwise degrees: 0, 90, 180 or 270
}

// Crop is a rectangle in source pixels.
type Crop struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Dims is a source's size with EXIF orientation applied.
type Dims struct {
	W int `json:"w"`
	H int `json:"h"`
}

// Normalize returns nil for an identity edit, else a copy.
func (e *Edit) Normalize() *Edit {
	if e == nil || (e.Crop == nil && e.Rotate == 0) {
		return nil
	}
	out := *e
	if e.Crop != nil {
		c := *e.Crop
		out.Crop = &c
	}
	return &out
}

// Check validates the edit's shape and, with a known size (w, h > 0), that
// the crop lies inside it.
func (e *Edit) Check(w, h int) error {
	if e == nil {
		return nil
	}
	switch e.Rotate {
	case 0, 90, 180, 270:
	default:
		return fmt.Errorf("rotate must be 0, 90, 180 or 270, not %d", e.Rotate)
	}
	if c := e.Crop; c != nil {
		if c.X < 0 || c.Y < 0 || c.W <= 0 || c.H <= 0 {
			return fmt.Errorf("crop %dx%d at %d,%d is empty or negative", c.W, c.H, c.X, c.Y)
		}
		if w > 0 && h > 0 && (c.X+c.W > w || c.Y+c.H > h) {
			return fmt.Errorf("crop %dx%d at %d,%d is outside the %dx%d source", c.W, c.H, c.X, c.Y, w, h)
		}
	}
	return nil
}

// Size is the edited size of a w×h source.
func (e *Edit) Size(w, h int) (int, int) {
	if e == nil {
		return w, h
	}
	if e.Crop != nil {
		w, h = e.Crop.W, e.Crop.H
	}
	if e.Rotate == 90 || e.Rotate == 270 {
		return h, w
	}
	return w, h
}

// Hash is the edit's stable identity; "" for no edit.
func (e *Edit) Hash() string {
	if e = e.Normalize(); e == nil {
		return ""
	}
	s := "r" + strconv.Itoa(e.Rotate)
	if c := e.Crop; c != nil {
		s += fmt.Sprintf("|c%d,%d,%d,%d", c.X, c.Y, c.W, c.H)
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// For is the identity of s derived through e, recorded as Variant.Spec: a
// variant is stale when its spec or its file's edit changes. Unedited specs
// ignore the edit.
func (s Spec) For(e *Edit) string {
	if h := e.Hash(); h != "" && !s.Unedited {
		return s.Hash() + "." + h
	}
	return s.Hash()
}

// fit derives the crop height from its width so the edited image has the
// slot's aspect (width/height); a native slot keeps the crop as given.
func (s Slot) fit(e *Edit) *Edit {
	if e = e.Normalize(); e == nil || e.Crop == nil || s.Native() {
		return e
	}
	e.Crop.H = max(1, int(math.Round(float64(e.Crop.W)/s.cropRatio(e.Rotate))))
	return e
}

// cropRatio is the crop's width/height that yields Aspect after rotating.
func (s Slot) cropRatio(rotate int) float64 {
	if rotate == 90 || rotate == 270 {
		return 1 / s.Aspect
	}
	return s.Aspect
}

// Resolve is the edit the slot applies to a w×h source (EXIF-oriented): e
// with its height fitted, or without a crop the largest centred one at
// Aspect (a native slot: the whole source). It must lie inside the source and
// be at least Min wide once edited.
func (s Slot) Resolve(e *Edit, w, h int) (*Edit, error) {
	e = s.fit(e)
	if (e == nil || e.Crop == nil) && s.Native() {
		if e == nil {
			e = &Edit{}
		}
	} else if e == nil || e.Crop == nil {
		rotate := 0
		if e != nil {
			rotate = e.Rotate
		}
		r := s.cropRatio(rotate)
		cw := w
		if ch := int(math.Round(float64(cw) / r)); ch > h {
			cw = max(1, min(w, int(math.Round(float64(h)*r))))
		}
		ch := min(h, max(1, int(math.Round(float64(cw)/r))))
		e = &Edit{Crop: &Crop{X: (w - cw) / 2, Y: (h - ch) / 2, W: cw, H: ch}, Rotate: rotate}
	}
	if err := e.Check(w, h); err != nil {
		return nil, err
	}
	if ew, _ := e.Size(w, h); ew < s.Min() {
		return nil, &ImageError{Code: CodeImageTooSmall, Message: fmt.Sprintf("the edited image must be at least %dpx wide; this one is %dpx", s.Min(), ew),
			Details: ErrorDetails{Width: ew, MinWidth: s.Min()}}
	}
	return e, nil
}
