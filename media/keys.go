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
	AreaBlobs     = layout.AreaBlobs
	AreaPublic    = layout.AreaPublic
)

// Item is a validated content item and the keys of its folder:
//
//	{tenant}/{kind}/{content_id}/manifest.json | manifests/{version}.json
//	                            /originals/{sha256-hex | u-uuid | slot}
//	                            /blobs/{sha256-hex | u-uuid}
//	                            /public/{output}.webp
type Item struct {
	ref    contentref.ContentRef
	kind   Kind
	prefix string
}

// Item validates ref against the registry. A version is required to address
// a versioned kind's manifest, and refused for an unversioned kind.
func (r *Registry) Item(ref contentref.ContentRef) (Item, error) {
	k, err := r.Kind(ref.ContentKind)
	if err != nil {
		return Item{}, err
	}
	if !layout.ValidSegment(ref.TenantID) || !layout.ValidSegment(ref.ContentID) {
		return Item{}, fmt.Errorf("media: invalid ref %s", ref)
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

// ManifestKey is manifest.json, or manifests/{version}.json for versioned kinds.
func (i Item) ManifestKey() (string, error) {
	if !i.kind.Versioned {
		return i.prefix + "manifest.json", nil
	}
	if v := i.ref.Version(); v != "" {
		return i.prefix + "manifests/" + v + ".json", nil
	}
	return "", fmt.Errorf("media: kind %q is versioned; ref %s has no version", i.kind.Name, i.ref)
}

// ManifestsPrefix lists every manifest of a versioned kind.
func (i Item) ManifestsPrefix() string { return i.prefix + "manifests/" }
func (i Item) OriginalsPrefix() string { return i.prefix + AreaOriginals + "/" }
func (i Item) BlobsPrefix() string     { return i.prefix + AreaBlobs + "/" }
func (i Item) PublicPrefix() string    { return i.prefix + AreaPublic + "/" }

// Original is the key of an uploaded file or master (never served).
func (i Item) Original(name string) (string, error) {
	if !layout.ValidBlobName(name) {
		return "", fmt.Errorf("media: invalid original name %q", name)
	}
	return i.OriginalsPrefix() + name, nil
}

// Blob is the key of a served, immutable derivative.
func (i Item) Blob(name string) (string, error) {
	if !layout.ValidBlobName(name) {
		return "", fmt.Errorf("media: invalid blob name %q", name)
	}
	return i.BlobsPrefix() + name, nil
}

// SlotOriginal is the fixed, overwritten original of a registered public slot.
func (i Item) SlotOriginal(slot string) (string, error) {
	if _, ok := i.kind.Slots[slot]; !ok {
		return "", fmt.Errorf("media: kind %q has no slot %q", i.kind.Name, slot)
	}
	return i.OriginalsPrefix() + slot, nil
}

// Public is public/{name}.webp: a slot output or a host-chosen inline image id.
func (i Item) Public(name string) (string, error) {
	if !layout.ValidSegment(name) || layout.ValidBlobName(name) {
		return "", fmt.Errorf("media: invalid public name %q", name)
	}
	return i.PublicPrefix() + name + layout.PublicExt, nil
}

// SHA256Name names content-addressed files: "sha256-{hex}".
func SHA256Name(sum []byte) string { return layout.SHA256Prefix + hex.EncodeToString(sum) }

// NewUploadName names a multipart upload whose hash is unknown: "u-{uuid}".
func NewUploadName() string { return layout.UploadPrefix + uuid.NewString() }
