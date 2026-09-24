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
	Specs      map[string]Spec // variant name → spec
	Slots      map[string]Slot // public slot name → outputs
	// Inline enables inline images: write-once public images with random ids
	// ("i-{uuid}", from NewInlineName), each re-encoded with this spec from
	// originals/{id} to public/{id}.webp. Post bodies and poll options use them.
	Inline *Spec
	Video  bool
	// Zip names the variant packed, in file order, into downloads["zip"];
	// "" offers no zip.
	Zip string
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
	Unedited bool
}

// Hash is the spec's stable identity; a variant whose recorded spec differs is
// stale (see For, which adds the file's edit).
func (s Spec) Hash() string {
	id := strconv.Itoa(s.Width) + "x" + strconv.Itoa(s.Height) + "|" + string(s.Fit) + "|q" +
		strconv.Itoa(s.Quality) + "|b" + strconv.FormatFloat(s.Blur, 'g', -1, 64)
	if s.Unedited {
		id += "|u"
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:4])
}

// Slot is a fixed public image: its original is kept at originals/{slot} and
// each output is written to public/{output}.webp.
type Slot struct {
	Outputs map[string]Spec
	// Aspect (width/height), when set, constrains crops set with
	// SetSlotFromFile: the crop's height is derived from its width.
	Aspect float64
}

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
		for name := range k.Specs {
			if !layout.ValidSegment(name) {
				return nil, fmt.Errorf("media: kind %q: invalid spec name %q", k.Name, name)
			}
		}
		if _, ok := k.Specs[k.Zip]; k.Zip != "" && !ok {
			return nil, fmt.Errorf("media: kind %q: zip variant %q has no spec", k.Name, k.Zip)
		}
		for t, l := range k.TypeLimits {
			if t == "" || strings.Contains(t, "/") || l.MaxBytes < 0 || l.MaxFiles < 0 {
				return nil, fmt.Errorf("media: kind %q: invalid type limit %q", k.Name, t)
			}
		}
		for name, slot := range k.Slots {
			if !layout.ValidSegment(name) || layout.ValidBlobName(name) || layout.ValidInlineName(name) || len(slot.Outputs) == 0 || slot.Aspect < 0 {
				return nil, fmt.Errorf("media: kind %q: invalid slot %q", k.Name, name)
			}
			for out := range slot.Outputs {
				if !layout.ValidSegment(out) || layout.ValidInlineName(out) {
					return nil, fmt.Errorf("media: kind %q slot %q: invalid output %q", k.Name, name, out)
				}
			}
		}
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
