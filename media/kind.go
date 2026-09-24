// Package media stores host content files in per-item folders of one private
// bucket: library-built keys, the generic manifest with conditional-write
// edits, and the Store port. See media/s3 for the S3 implementation and
// media/token for access tokens.
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/media/layout"
)

// Kind is a host's per-kind rule set, registered once at startup.
type Kind struct {
	Name      string
	Versioned bool // manifests live at manifests/{version_id}.json
	// Types are the accepted content types (empty: any); image processing
	// also requires the bytes to be the declared format.
	Types    []string
	MaxBytes int64
	// MaxFiles caps a manifest's files; 0 is unlimited.
	MaxFiles int
	// TypeLimits are caps per top-level type ("image", "video"): a set
	// MaxBytes replaces the kind's for that type, and MaxFiles caps that
	// type's files. A kind may mix images (Specs) and videos (Video).
	TypeLimits map[string]Limit
	Specs      map[string]Spec // variant name → spec
	Slots      map[string]Slot // public slot name → outputs
	// Inline enables inline images: write-once public images with random ids
	// ("i-{uuid}", from NewInlineName), each re-encoded with this spec from
	// originals/{id} to public/{id}.webp. Post bodies and poll options use them.
	Inline *Spec
	Video  *Video // nil: no video encoding
	// Zip names the variant packed, in file order, into downloads["zip"];
	// "" offers no zip.
	Zip string
}

// Video configures a kind's video encoding (media/video).
type Video struct {
	// Ladder is the rendition short sides (height of landscape, width of
	// vertical video), largest first; rungs above the source's short side
	// are dropped. Empty is DefaultLadder.
	Ladder []int `json:"ladder,omitempty"`
	// MinAspect and MaxAspect bound a source's display width/height; a
	// source outside fails permanently. Zero is DefaultMinAspect/DefaultMaxAspect.
	MinAspect float64 `json:"min_aspect,omitempty"`
	MaxAspect float64 `json:"max_aspect,omitempty"`
}

// DefaultLadder is the default H.264 ladder by short side.
var DefaultLadder = []int{2160, 1440, 1080, 720, 480}

// Default aspect bounds: 1:2.4 vertical to 2.4:1 wide, which admits "21:9"
// content (2560×1080 at 2.37, 2.39:1 cinema) and its vertical equivalents.
const (
	DefaultMinAspect = 1 / 2.4
	DefaultMaxAspect = 2.4
)

// Rungs is the ladder in effect.
func (v *Video) Rungs() []int {
	if v == nil || len(v.Ladder) == 0 {
		return DefaultLadder
	}
	return v.Ladder
}

// Aspects are the aspect bounds in effect.
func (v *Video) Aspects() (lo, hi float64) {
	lo, hi = DefaultMinAspect, DefaultMaxAspect
	if v != nil && v.MinAspect > 0 {
		lo = v.MinAspect
	}
	if v != nil && v.MaxAspect > 0 {
		hi = v.MaxAspect
	}
	return lo, hi
}

// Validate requires even rungs of 2–4320, largest first, without repeats,
// and aspect bounds with MinAspect ≤ 1 ≤ MaxAspect.
func (v Video) Validate() error {
	for i, n := range v.Ladder {
		if n < 2 || n > 4320 || n%2 != 0 || i > 0 && n >= v.Ladder[i-1] {
			return fmt.Errorf("media: invalid video ladder %v", v.Ladder)
		}
	}
	if lo, hi := v.Aspects(); v.MinAspect < 0 || v.MaxAspect < 0 || lo > 1 || hi < 1 {
		return fmt.Errorf("media: invalid video aspect bounds %g–%g", lo, hi)
	}
	return nil
}

// Limit is a per-type cap; zero values are unlimited.
type Limit struct {
	MaxBytes int64
	MaxFiles int
}

// Fit is how an image spec fits its box.
type Fit string

const (
	FitInside Fit = "inside"
	FitCover  Fit = "cover"
)

// Spec describes one derived image. Zero Width and Height keep full resolution.
type Spec struct {
	Width   int
	Height  int
	Fit     Fit
	Quality int
	Blur    float64
	// Unedited ignores the file's Edit: an editor's view of the whole source.
	// It must be EditorOnly.
	Unedited bool
	// EditorOnly variants are signed by the read API only for actors whose
	// Resolution is Editor; slots, inline images and zips cannot use them.
	EditorOnly bool
}

// Hash is the spec's stable identity; a variant whose recorded spec differs is
// stale (see For, which adds the file's edit).
func (s Spec) Hash() string {
	id := strconv.Itoa(s.Width) + "x" + strconv.Itoa(s.Height) + "|" + string(s.Fit) + "|q" +
		strconv.Itoa(s.Quality) + "|b" + strconv.FormatFloat(s.Blur, 'g', -1, 64)
	if s.Unedited {
		id += "|u"
	}
	if s.EditorOnly {
		id += "|e"
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:4])
}

// Slot is a fixed public image at Aspect (the edited image's width/height),
// rendered at each of Widths to public/{slot}_{width}.webp (SlotOutput). Its
// original is kept at originals/{slot} and its Edit in the slot record. Widths
// wider than the edited image are skipped, never upscaled; an edit narrower
// than Min fails, so every width up to Min exists once the slot is processed.
type Slot struct {
	Aspect   float64
	Widths   []int
	MinWidth int
	Quality  int // WebP quality; default 80
}

// Min is the narrowest edited width accepted: MinWidth, at least the smallest width.
func (s Slot) Min() int { return max(s.MinWidth, slices.Min(s.Widths)) }

// Height is the output height of a width at Aspect.
func (s Slot) Height(width int) int { return max(1, int(math.Round(float64(width)/s.Aspect))) }

// Hash is the slot spec's stable identity; outputs under another are stale.
func (s Slot) Hash() string {
	b := []byte(strconv.FormatFloat(s.Aspect, 'g', -1, 64) + "|" + strconv.Itoa(s.Min()) + "|q" + strconv.Itoa(s.Quality))
	for _, w := range s.Widths {
		b = strconv.AppendInt(append(b, '|'), int64(w), 10)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

// SlotOutput names a slot's output of one width: "{slot}_{width}".
func SlotOutput(slot string, width int) string { return slot + "_" + strconv.Itoa(width) }

const maxSlotWidth = 8192

var (
	ErrUnknownKind = errors.New("media: unknown kind")
	ErrType        = errors.New("media: content type not allowed")
	ErrTooLarge    = errors.New("media: file too large")
)

// Allows checks a file against the kind's types and size cap.
func (k Kind) Allows(contentType string, size int64) error {
	if len(k.Types) > 0 && !slices.Contains(k.Types, contentType) {
		return fmt.Errorf("%w: %q for kind %q", ErrType, contentType, k.Name)
	}
	limit := k.MaxBytes
	if l := k.TypeLimits[topType(contentType)]; l.MaxBytes > 0 {
		limit = l.MaxBytes
	}
	if size < 0 || (limit > 0 && size > limit) {
		return fmt.Errorf("%w: %d bytes of %s for kind %q (max %d)", ErrTooLarge, size, contentType, k.Name, limit)
	}
	return nil
}

// checkFiles refuses an edit that leaves more files than the kind's caps
// allow and adds to them; a manifest already over a lowered cap can still
// shrink or be reordered.
func (k Kind) checkFiles(before, after *Manifest) error {
	if k.MaxFiles > 0 && len(after.Files) > k.MaxFiles && len(after.Files) > len(before.Files) {
		return uploadErr(CodeTooManyFiles, "kind %q allows at most %d files", k.Name, k.MaxFiles)
	}
	if len(k.TypeLimits) == 0 {
		return nil
	}
	count := func(m *Manifest) map[string]int {
		n := map[string]int{}
		for _, f := range m.Files {
			n[topType(f.Type)]++
		}
		return n
	}
	was, now := count(before), count(after)
	for t, l := range k.TypeLimits {
		if l.MaxFiles > 0 && now[t] > l.MaxFiles && now[t] > was[t] {
			return uploadErr(CodeTooManyFiles, "kind %q allows at most %d %s files", k.Name, l.MaxFiles, t)
		}
	}
	return nil
}

func topType(contentType string) string {
	t, _, _ := strings.Cut(contentType, "/")
	return t
}

// Registry is the host's set of kinds.
type Registry struct {
	kinds map[string]Kind
}

// NewRegistry validates and registers kinds.
func NewRegistry(kinds ...Kind) (*Registry, error) {
	r := &Registry{kinds: make(map[string]Kind, len(kinds))}
	for _, k := range kinds {
		if !layout.ValidSegment(k.Name) {
			return nil, fmt.Errorf("media: invalid kind name %q", k.Name)
		}
		if _, dup := r.kinds[k.Name]; dup {
			return nil, fmt.Errorf("media: duplicate kind %q", k.Name)
		}
		for name, s := range k.Specs {
			if !layout.ValidSegment(name) || s.Unedited && !s.EditorOnly {
				return nil, fmt.Errorf("media: kind %q: invalid spec %q (Unedited must be EditorOnly)", k.Name, name)
			}
		}
		if s, ok := k.Specs[k.Zip]; k.Zip != "" && (!ok || s.EditorOnly) {
			return nil, fmt.Errorf("media: kind %q: zip variant %q has no spec or is EditorOnly", k.Name, k.Zip)
		}
		if k.Inline != nil && (k.Inline.Unedited || k.Inline.EditorOnly) {
			return nil, fmt.Errorf("media: kind %q: inline images are public; their spec cannot be Unedited or EditorOnly", k.Name)
		}
		if k.Video != nil {
			if err := k.Video.Validate(); err != nil {
				return nil, fmt.Errorf("media: kind %q: %w", k.Name, err)
			}
		}
		for t, l := range k.TypeLimits {
			if t == "" || strings.Contains(t, "/") || l.MaxBytes < 0 || l.MaxFiles < 0 {
				return nil, fmt.Errorf("media: kind %q: invalid type limit %q", k.Name, t)
			}
		}
		slots := make(map[string]Slot, len(k.Slots))
		for name, slot := range k.Slots {
			if !layout.ValidSegment(name) || layout.ValidBlobName(name) || layout.ValidInlineName(name) || strings.HasSuffix(name, slotRecordExt) {
				return nil, fmt.Errorf("media: kind %q: invalid slot name %q", k.Name, name)
			}
			if !(slot.Aspect > 0 && slot.Aspect < 100) || len(slot.Widths) == 0 || slot.MinWidth < 0 || slot.Quality < 0 || slot.Quality > 100 {
				return nil, fmt.Errorf("media: kind %q slot %q: needs an Aspect and Widths", k.Name, name)
			}
			slot.Widths = slices.Sorted(slices.Values(slot.Widths))
			for i, w := range slot.Widths {
				if w <= 0 || w > maxSlotWidth || (i > 0 && w == slot.Widths[i-1]) {
					return nil, fmt.Errorf("media: kind %q slot %q: invalid width %d", k.Name, name, w)
				}
			}
			slots[name] = slot
		}
		k.Slots = slots
		r.kinds[k.Name] = k
	}
	return r, nil
}

// Kind returns a registered kind.
func (r *Registry) Kind(name string) (Kind, error) {
	k, ok := r.kinds[name]
	if !ok {
		return Kind{}, fmt.Errorf("%w %q", ErrUnknownKind, name)
	}
	return k, nil
}
