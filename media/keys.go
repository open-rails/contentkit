package media

import (
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// Folder areas.
const (
	AreaManifest  = layout.AreaManifest
	AreaOriginals = layout.AreaOriginals
	AreaStaging   = layout.AreaStaging
	AreaPrivate   = layout.AreaPrivate
	AreaPublic    = layout.AreaPublic
)

// Item is a validated content item and the keys of its folder (see
// media/layout):
//
//	{tenant}/{kind}/{content_id}/manifest.json
//	                            /originals/sha256-{hex}
//	                            /staging/u-{uuid}
//	                            /private/sha256-{hex}
//	                            /public/sha256-{hex}
type Item struct {
	ref    contentref.ContentRef
	kind   Kind
	prefix string
}

// Item validates ref against the registry. A version is required to address
// a versioned kind's files, and refused for an unversioned kind.
func (r *Registry) Item(ref contentref.ContentRef) (Item, error) {
	k, err := r.Kind(ref.ContentKind)
	if err != nil {
		return Item{}, err
	}
	if !layout.ValidSegment(ref.TenantID) {
		return Item{}, fmt.Errorf("media: invalid ref %s", ref)
	}
	if err := contentref.ValidateID(ref.ContentID); err != nil {
		return Item{}, fmt.Errorf("media: invalid ref %s: %w", ref, err)
	}
	if v := ref.Version(); v != "" && (!k.Versioned || !layout.ValidSegment(v)) {
		return Item{}, fmt.Errorf("media: invalid version in ref %s", ref)
	}
	if ref.ContentVersionID != nil && ref.Version() == "" {
		return Item{}, fmt.Errorf("media: empty version in ref %s", ref)
	}
	return Item{ref: ref, kind: k, prefix: ref.TenantID + "/" + ref.ContentKind + "/" + ref.ContentID + "/"}, nil
}

func (i Item) Ref() contentref.ContentRef { return i.ref }
func (i Item) Kind() Kind                 { return i.kind }

// Prefix is the folder, "{tenant}/{kind}/{id}/"; versions share it.
func (i Item) Prefix() string { return i.prefix }

// ManifestKey is the folder's manifest.json; versions are sections of it.
func (i Item) ManifestKey() string { return i.prefix + layout.ManifestName }

// Section is the manifest section the ref's files live in: its version for
// a versioned kind (required), "" otherwise.
func (i Item) Section() (string, error) {
	if !i.kind.Versioned {
		return "", nil
	}
	if v := i.ref.Version(); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("media: kind %q is versioned; ref %s has no version", i.kind.Name, i.ref)
}

func (i Item) OriginalsPrefix() string { return i.prefix + AreaOriginals + "/" }
func (i Item) StagingPrefix() string   { return i.prefix + AreaStaging + "/" }
func (i Item) PrivatePrefix() string   { return i.prefix + AreaPrivate + "/" }
func (i Item) PublicPrefix() string    { return i.prefix + AreaPublic + "/" }

// Original is the key of an uploaded file (never served):
// originals/sha256-{hex}, or staging/u-{uuid} for a multipart upload the
// worker has not placed yet.
func (i Item) Original(name string) (string, error) {
	if !layout.ValidSourceName(name) {
		return "", fmt.Errorf("media: invalid original name %q", name)
	}
	return i.prefix + layout.SourceArea(name) + "/" + name, nil
}

// Private is the key of a rendition: private/sha256-{hex}, token-gated.
func (i Item) Private(name string) (string, error) {
	if !layout.ValidHashName(name) {
		return "", fmt.Errorf("media: invalid rendition name %q", name)
	}
	return i.PrivatePrefix() + name, nil
}

// Public is the key of an exposed rendition's copy: public/sha256-{hex}.
func (i Item) Public(name string) (string, error) {
	if !layout.ValidHashName(name) {
		return "", fmt.Errorf("media: invalid rendition name %q", name)
	}
	return i.PublicPrefix() + name, nil
}

// Inline reports whether name is an inline image id the kind accepts.
func (i Item) Inline(name string) bool {
	return i.kind.Inline != nil && layout.ValidInlineName(name)
}

// SHA256Name names content-addressed files: "sha256-{hex}".
func SHA256Name(sum []byte) string { return layout.SHA256Prefix + hex.EncodeToString(sum) }

// NewInlineName names a new inline image: "i-{uuid}".
func NewInlineName() string { return layout.InlinePrefix + uuid.NewString() }

// NewUploadName names a multipart upload whose hash is unknown: "u-{uuid}".
func NewUploadName() string { return layout.UploadPrefix + uuid.NewString() }
