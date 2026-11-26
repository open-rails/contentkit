package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/contentkit/contentref"
)

// An upload has three roles in its manifest: the original (as uploaded,
// never modified), its master (File.Master: what a version releases and
// downloads serve), and the renditions derived from the master (File.Input).
// A kind's master recipe (Kind.Master) is the chain of hooks that makes the
// master; the identity recipe, the default, makes the original itself the
// master: the same blob, no copy, no re-encode, no object of its own.

// IdentityRecipe is the fingerprint of the empty recipe.
const IdentityRecipe = "identity"

// Recipe is a kind's master processing chain: its steps run in order on
// each image and video upload, the first on the original. No steps is the
// identity.
type Recipe struct {
	Steps []RecipeStep `json:"steps,omitempty"`
}

// RecipeStep names a MasterHook, the version of it the step
// expects (a worker whose hook reports another refuses to start, so a hook's
// new behaviour is a deliberate version bump that re-derives its masters)
// and its parameters.
type RecipeStep struct {
	Hook    string         `json:"hook"`
	Version string         `json:"version"`
	Params  map[string]any `json:"params,omitempty"`
}

// Identity reports the identity recipe.
func (r *Recipe) Identity() bool { return r == nil || len(r.Steps) == 0 }

// FP is the recipe's fingerprint: IdentityRecipe, or a digest of its steps.
func (r *Recipe) FP() string {
	if r.Identity() {
		return IdentityRecipe
	}
	b, _ := json.Marshal(r.Steps)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Validate requires each step to name a hook, once, and its version.
func (r *Recipe) Validate() error {
	if r == nil {
		return nil
	}
	seen := map[string]bool{}
	for i, s := range r.Steps {
		switch {
		case strings.TrimSpace(s.Hook) == "" || strings.TrimSpace(s.Version) == "":
			return fmt.Errorf("master step %d needs a hook and a version", i)
		case seen[s.Hook]:
			return fmt.Errorf("master step %d repeats hook %q", i, s.Hook)
		}
		seen[s.Hook] = true
		if _, err := json.Marshal(s.Params); err != nil {
			return fmt.Errorf("master step %q: params: %w", s.Hook, err)
		}
	}
	return nil
}

// Master is an upload's master: the version a release serves and downloads
// carry, from which its renditions derive. Under the identity recipe (or
// hooks that changed nothing) Blob is the upload's own blob. A master a hook
// produced is its own lossless object, made from the original whose sha256
// is Of.
type Master struct {
	Blob   string       `json:"blob"`   // "{area}/{sha256}"
	Recipe string       `json:"recipe"` // the recipe's FP
	Steps  []RecipeStep `json:"steps,omitempty"`
	Of     string       `json:"of,omitempty"` // the original's sha256, for a master of its own
	Type   string       `json:"type,omitempty"`
	Size   int64        `json:"size,omitempty"`
	W      int          `json:"w,omitempty"`
	H      int          `json:"h,omitempty"`
}

// IsOriginal reports a master that is the original itself, with no object
// of its own.
func (m *Master) IsOriginal() bool { return m != nil && m.Of == "" }

// Lossless master formats. A hook that changes pixels writes an image as
// lossless WebP or PNG (stored as lossless WebP unless a side exceeds
// MaxWebPSide or it is 16-bit); a video as FFV1 in Matroska.
const (
	MasterWebP     = "image/webp"
	MasterPNG      = "image/png"
	MasterMatroska = "video/x-matroska"
	MaxWebPSide    = 16383
)

// MasterHook is one step of a master recipe: straightening a scan,
// adjusting contrast and brightness, upscaling with a model. A host passes
// its hooks to its own media worker binary (media/worker.Main), which
// refuses to start while a recipe names a hook it lacks or at another
// version. A hook may run in the worker or hand the file to an external job
// (a GPU upscaler) and wait for it. It is retried with its job; while it
// fails, the upload keeps its previous master and renditions, so a release
// never changes because of a failing hook.
//
// A video hook writes the whole file, its audio and subtitle tracks
// included: the renditions, sprite and grabbed frames all derive from it.
type MasterHook interface {
	// Name is the name recipes give the hook (RecipeStep.Hook).
	Name() string
	// Version is the hook's behaviour version, which recipes name.
	Version() string
	// Process reads in.Path and writes its result to in.Out, or returns
	// Unchanged when it changes no pixels (the step passes its input on and
	// stores nothing).
	Process(ctx context.Context, in MasterInput) (MasterOutput, error)
}

// MasterInput is one step's input: the original or the previous step's
// result, as a local file.
type MasterInput struct {
	Kind   string // the media kind
	File   string // the upload's app path: "pages/001.png"
	Path   string // local file holding the input
	Type   string // its content type
	Out    string // local file to write a changed result to
	Params map[string]any
}

// MasterOutput is a step's result: Unchanged, or the type of what it wrote
// to MasterInput.Out (MasterWebP lossless, MasterPNG, MasterMatroska with
// FFV1).
type MasterOutput struct {
	Unchanged bool
	Type      string
}

// ErrMasterFormat refuses a hook result in no lossless master format.
var ErrMasterFormat = errors.New("media: a master must be lossless WebP or PNG, or FFV1 in Matroska")

// Regeneration selects the items a regeneration visits: every item of Kind,
// or only those whose content or version id is in IDs, and of those only
// what Filter keeps. Each visited item's masters whose recipe changed are
// made again, then its renditions whose master or spec changed; current
// ones are left alone, so a regeneration can be repeated or resumed.
type Regeneration struct {
	Kind   string
	IDs    []string
	Filter func(contentref.ContentRef) bool
}
