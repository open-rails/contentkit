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

	"github.com/open-rails/contentkit/media/layout"
)

// Kind is a host's per-kind rule set, registered once at startup.
type Kind struct {
	Name      string
	Versioned bool // manifests live at manifests/{version_id}.json
	Types     []string
	MaxBytes  int64
	Specs     map[string]Spec // variant name → spec
	Slots     map[string]Slot // public slot name → outputs
	Video     bool
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
}

// Hash is the spec's stable identity; a variant whose recorded spec differs is stale.
func (s Spec) Hash() string {
	sum := sha256.Sum256([]byte(strconv.Itoa(s.Width) + "x" + strconv.Itoa(s.Height) + "|" + string(s.Fit) + "|q" +
		strconv.Itoa(s.Quality) + "|b" + strconv.FormatFloat(s.Blur, 'g', -1, 64)))
	return hex.EncodeToString(sum[:4])
}

// Slot is a fixed public image: its original is kept at originals/{slot} and
// each output is written to public/{output}.webp.
type Slot struct {
	Outputs map[string]Spec
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
	if size < 0 || (k.MaxBytes > 0 && size > k.MaxBytes) {
		return fmt.Errorf("%w: %d bytes for kind %q (max %d)", ErrTooLarge, size, k.Name, k.MaxBytes)
	}
	return nil
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
		for name, slot := range k.Slots {
			if !layout.ValidSegment(name) || layout.ValidBlobName(name) || len(slot.Outputs) == 0 {
				return nil, fmt.Errorf("media: kind %q: invalid slot %q", k.Name, name)
			}
			for out := range slot.Outputs {
				if !layout.ValidSegment(out) {
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
