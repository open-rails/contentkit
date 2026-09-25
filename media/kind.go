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
	Specs      map[string]Spec // variant name → spec ("editor" is reserved)
	Slots      map[string]Slot // public slot name → outputs
	// Editor is the editor view: the whole source (EXIF-oriented, ignoring
	// crop and rotate) the cropper draws on, for image files and slot
	// originals. It is an input-keyed cache in temp/ (Item.EditorView), never
	// in the manifest: the image job renders it, the sweep deletes it after
	// JobsConfig.EditorTTL, and an editor's read renders it again. Only
	// editors get its URL, under an editor token. nil: no editor views.
	Editor *Spec
	// Inline enables inline images: write-once public images with random ids
	// ("i-{uuid}", from NewInlineName), each re-encoded with this spec from
	// originals/{id} to public/{id}.webp. Post bodies and poll options use them.
	Inline *Spec
	// Animation is the policy for animated images (GIF, WebP) in files and
	// inline images; slots set their own.
	Animation Animation
	Video     *Video // nil: no video encoding
	// Zip names the variant packed, in file order, into downloads["zip"];
	// "" offers no zip.
	Zip string
}

// Video configures a kind's video encoding (media/video).
type Video struct {
	// Ladder is the rendition short sides (height of landscape, width of
	// vertical video), largest first; rungs above the source's short side
	// are dropped, and a source below 1080 that is not a rung also gets one
	// at its own short side. Empty is DefaultLadder.
	Ladder []int `json:"ladder,omitempty"`
	// MinAspect and MaxAspect bound a source's display width/height; a
	// source outside fails permanently. Zero is DefaultMinAspect/DefaultMaxAspect.
	MinAspect float64 `json:"min_aspect,omitempty"`
	MaxAspect float64 `json:"max_aspect,omitempty"`
	// PosterWidths are the cover (poster slot) output widths, the host's
	// display sizes × densities; widths wider than the frame or upload are
	// skipped. Empty is DefaultPosterWidths.
	PosterWidths []int `json:"poster_widths,omitempty"`
	// Profile tunes the encode to the content: VideoLive (default) or
	// VideoAnimation (x264 tune animation, lower CRF, lower caps).
	Profile string `json:"profile,omitempty"`
}

// DefaultPosterWidths cover a full-width column at 2–3× density.
var DefaultPosterWidths = []int{640, 960, 1280, 1920, 2560}

// Poster is the kind's poster slot: native aspect at PosterWidths.
func (v *Video) Poster() Slot {
	if v == nil || len(v.PosterWidths) == 0 {
		return Slot{Widths: DefaultPosterWidths}
	}
	return Slot{Widths: slices.Sorted(slices.Values(v.PosterWidths))}
}

// Video.Profile values.
const (
	VideoLive      = ""
	VideoAnimation = "animation"
)

// DefaultLadder is the default ladder by short side.
var DefaultLadder = []int{2160, 1080, 480}

// Codec is a video codec of a ladder: each rung is encoded in each codec
// the media worker is configured with.
type Codec string

const (
	CodecH264 Codec = "h264"
	CodecHEVC Codec = "hevc"
	CodecAV1  Codec = "av1"
)

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
// aspect bounds with MinAspect ≤ 1 ≤ MaxAspect, and a known Profile.
func (v Video) Validate() error {
	for i, n := range v.Ladder {
		if n < 2 || n > 4320 || n%2 != 0 || i > 0 && n >= v.Ladder[i-1] {
			return fmt.Errorf("media: invalid video ladder %v", v.Ladder)
		}
	}
	if lo, hi := v.Aspects(); v.MinAspect < 0 || v.MaxAspect < 0 || lo > 1 || hi < 1 {
		return fmt.Errorf("media: invalid video aspect bounds %g–%g", lo, hi)
	}
	if v.Profile != VideoLive && v.Profile != VideoAnimation {
		return fmt.Errorf("media: unknown video profile %q", v.Profile)
	}
	return nil
}

// Limit is a per-type cap; zero values are unlimited.
type Limit struct {
	MaxBytes int64
	MaxFiles int
}

// EditorVariant is the read API's name for the editor view (Kind.Editor).
const EditorVariant = "editor"

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
}

// Hash is the spec's stable identity; a variant whose recorded spec differs is
// stale (see For, which adds the file's edit).
func (s Spec) Hash() string {
	id := strconv.Itoa(s.Width) + "x" + strconv.Itoa(s.Height) + "|" + string(s.Fit) + "|q" +
		strconv.Itoa(s.Quality) + "|b" + strconv.FormatFloat(s.Blur, 'g', -1, 64)
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:4])
}

// Slot is a fixed public image at Aspect (the edited image's width:height),
// or at the edited image's own shape when Aspect is AspectNative (no crop by
// default, any crop shape), rendered at each of Widths (the host's rungs,
// e.g. a small and a large one) to hash-named WebP renditions in private/,
// copied to public/ unless the item is hidden. A change writes new names.
// Nothing is upscaled: a rung wider than the edited image is rendered at the
// edited width, so every rung exists once the slot is set. Its original and
// Edit are recorded in the manifest (Root.Slots). An edit narrower than Min
// fails.
type Slot struct {
	Aspect    Aspect
	Widths    []int
	MinWidth  int
	Quality   int // WebP quality; default 80
	Animation Animation
}

// Animation is a policy for animated images (GIF, WebP; AVIF/HEIF sequences
// are refused as animation_unsupported, never flattened).
type Animation string

const (
	// AnimationAllow keeps every frame, delay and the loop count in each
	// rendition; edits and resizes apply per frame. The default.
	AnimationAllow Animation = ""
	// AnimationReject refuses an animated upload with animation_not_allowed.
	AnimationReject Animation = "reject"
)

// Min is the narrowest edited width accepted: MinWidth, else the smallest width.
func (s Slot) Min() int {
	if s.MinWidth > 0 {
		return s.MinWidth
	}
	return slices.Min(s.Widths)
}

// OutputWidth is the width rendered for rung from an image edited pixels
// wide: the rung, or edited when narrower (never upscaled).
func (s Slot) OutputWidth(rung, edited int) int { return min(rung, edited) }

// Native reports a slot at its edited image's own aspect.
func (s Slot) Native() bool { return s.Aspect.Native() }

// Height is the output height of a width at Aspect (0 for a native slot).
func (s Slot) Height(width int) int {
	return s.Aspect.Height(width)
}

// Size is the output of a width from an edited image of size edited.
func (s Slot) Size(width int, edited Dims) Dims {
	if s.Native() && edited.W > 0 {
		return Dims{W: width, H: Aspect{edited.W, edited.H}.Height(width)}
	}
	return Dims{W: width, H: s.Height(width)}
}

// Hash is the slot spec's stable identity; outputs under another are stale.
func (s Slot) Hash() string {
	b := []byte(s.Aspect.String() + "|" + strconv.Itoa(s.Min()) + "|q" + strconv.Itoa(s.Quality) + "|capped")
	for _, w := range s.Widths {
		b = strconv.AppendInt(append(b, '|'), int64(w), 10)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

const maxSlotWidth = 8192

var (
	ErrUnknownKind = errors.New("media: unknown kind")
	ErrType        = errors.New("media: content type not allowed")
	ErrTooLarge    = errors.New("media: file too large")
)

// Allows checks a file against the kind's types and size cap.
func (k Kind) Allows(contentType string, size int64) error {
	if len(k.Types) > 0 && !slices.Contains(k.Types, contentType) {
		return &UploadError{Code: CodeType, Message: fmt.Sprintf("%s files are not allowed here; allowed: %s", contentType, strings.Join(k.Types, ", ")),
			Details: &ErrorDetails{Type: contentType, Allowed: k.Types}}
	}
	limit := k.MaxBytes
	if l := k.TypeLimits[topType(contentType)]; l.MaxBytes > 0 {
		limit = l.MaxBytes
	}
	if size < 0 || (limit > 0 && size > limit) {
		return &UploadError{Code: CodeTooLarge, Message: fmt.Sprintf("%s files may be at most %d bytes; this one is %d", contentType, limit, size),
			Details: &ErrorDetails{Type: contentType, Size: size, MaxBytes: limit}}
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
		for name := range k.Specs {
			if !layout.ValidSegment(name) || name == EditorVariant {
				return nil, fmt.Errorf("media: kind %q: invalid spec name %q (%q is the editor view)", k.Name, name, EditorVariant)
			}
		}
		if _, ok := k.Specs[k.Zip]; k.Zip != "" && !ok {
			return nil, fmt.Errorf("media: kind %q: zip variant %q has no spec", k.Name, k.Zip)
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
			if !layout.ValidSegment(name) || layout.ValidSourceName(name) || layout.ValidInlineName(name) {
				return nil, fmt.Errorf("media: kind %q: invalid slot name %q", k.Name, name)
			}
			if !slot.Aspect.Valid() || len(slot.Widths) == 0 || slot.MinWidth < 0 || slot.Quality < 0 || slot.Quality > 100 {
				return nil, fmt.Errorf("media: kind %q slot %q: needs a valid Aspect (or AspectNative) and Widths", k.Name, name)
			}
			slot.Widths = slices.Sorted(slices.Values(slot.Widths))
			for i, w := range slot.Widths {
				if w <= 0 || w > maxSlotWidth || (i > 0 && w == slot.Widths[i-1]) {
					return nil, fmt.Errorf("media: kind %q slot %q: invalid width %d", k.Name, name, w)
				}
			}
			slots[name] = slot
		}
		if k.Video != nil {
			for _, reserved := range []string{PosterSlot} {
				if _, ok := slots[reserved]; ok {
					return nil, fmt.Errorf("media: kind %q: slot %q is reserved on video kinds", k.Name, reserved)
				}
			}
			poster := k.Video.Poster()
			for i, w := range poster.Widths {
				if w <= 0 || w > maxSlotWidth || (i > 0 && w == poster.Widths[i-1]) {
					return nil, fmt.Errorf("media: kind %q: invalid poster width %d", k.Name, w)
				}
			}
			slots[PosterSlot] = poster
		}
		k.Slots = slots
		r.kinds[k.Name] = k
	}
	return r, nil
}

func (r *Registry) hasVideo() bool {
	for _, k := range r.kinds {
		if k.Video != nil {
			return true
		}
	}
	return false
}

// Kind returns a registered kind.
func (r *Registry) Kind(name string) (Kind, error) {
	k, ok := r.kinds[name]
	if !ok {
		return Kind{}, fmt.Errorf("%w %q", ErrUnknownKind, name)
	}
	return k, nil
}
