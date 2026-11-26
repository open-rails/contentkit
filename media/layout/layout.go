// Package layout defines media object keys, dependency-free so the media
// gateway can classify paths without importing the media runtime. The bucket
// is the tenancy boundary: each app has its own, so no key names a tenant or
// an app. Every stored file is named by the SHA-256 of its bytes, in the area
// that decides who may read it: public/ everyone, private/ the item's signed
// readers, internal/ nobody (never served).
//
//	{kind}/{id}/manifest.json         gzip JSON, the item's only record of its files; never served
//	           /internal/{sha256}     unreleased uploads and outputs
//	           /private/{sha256}      released files, behind the item's bucket-bound token
//	           /public/{sha256}       always-visible files (covers), served to anyone
//	           /public/_contentkit_erased  fence: while it exists the gateway serves nothing of the item
//	           /tmp/u-{uuid}          staged uploads; never served
//	           /tmp/w-{run}-{name}    a worker run's scratch (video chunks); never served
//	{kind}/_default/                  a kind's default images, an item like any other
//	_accounts/{kind}/{id}/…           a tenant's account media (avatars), the same folders,
//	                                  in the avatar primary's bucket
//
// The store tags everything in tmp/ temporary when it writes it, so the
// bucket's lifecycle expires what nothing deletes.
//
// A versioned kind's item is its version: {kind}/{version}/. Top-level names
// starting with "_" are ContentKit's (_accounts/, the store's _probe/), and
// work/ is the bucket's video-work lifecycle prefix: neither is a kind. The
// URL of a served object is its key under /v1/ on the site's media host.
package layout

import (
	"encoding/hex"
	"strings"
)

// Folder areas.
const (
	AreaManifest = "manifest"
	AreaInternal = "internal"
	AreaPrivate  = "private"
	AreaPublic   = "public"
	AreaTmp      = "tmp"
)

const (
	// ManifestName is the manifest's key within the folder.
	ManifestName = "manifest.json"
	// FenceName is an item's reserved gateway fence in public/: while it
	// exists the gateway serves none of the item's files. Its FenceMeta is
	// FenceDeleted while the item is soft-deleted, and restore removes it;
	// FenceErased is permanent.
	FenceName = "_contentkit_erased"
	// FenceMeta is the fence's user-metadata key (x-amz-meta-contentkit-fence).
	FenceMeta = "contentkit-fence"
	// FenceDeleted and FenceErased are FenceMeta's values.
	FenceDeleted = "deleted"
	FenceErased  = "erased"
	// DefaultID is the item id holding a kind's default images.
	DefaultID = "_default"
	// StagedPrefix starts a staged upload's name in tmp/.
	StagedPrefix = "u-"
	// ScratchPrefix starts a worker's scratch name in tmp/: "w-{run}-{name}",
	// run a UUID.
	ScratchPrefix = "w-"
	// URLPrefix is the path every media URL starts with.
	URLPrefix = "/v1/"
	// AccountRoot holds a tenant's account media, in the avatar primary's
	// bucket.
	AccountRoot = "_accounts/"
)

// Key is a parsed object key.
type Key struct {
	Account  bool // under AccountRoot
	Kind, ID string
	Area     string
	Name     string // the name within the area; "" for the manifest
}

// Folder is the key's item folder.
func (k Key) Folder() string {
	if k.Account {
		return AccountPrefix(k.Kind, k.ID)
	}
	return Prefix(k.Kind, k.ID)
}

// Parse classifies an object key; ok is false for anything outside the layout.
func Parse(key string) (Key, bool) {
	var k Key
	key, k.Account = strings.CutPrefix(key, AccountRoot)
	parts := strings.Split(key, "/")
	if len(parts) < 3 || len(parts) > 4 || !ValidKind(parts[0]) || !ValidSegment(parts[1]) {
		return Key{}, false
	}
	k.Kind, k.ID = parts[0], parts[1]
	rest := parts[2:]
	switch {
	case len(rest) == 1 && rest[0] == ManifestName:
		k.Area = AreaManifest
	case len(rest) == 2 && (rest[0] == AreaInternal || rest[0] == AreaPrivate || rest[0] == AreaPublic) && ValidHash(rest[1]),
		len(rest) == 2 && rest[0] == AreaPublic && rest[1] == FenceName,
		len(rest) == 2 && rest[0] == AreaTmp && (ValidStagedName(rest[1]) || ValidScratchName(rest[1])):
		k.Area, k.Name = rest[0], rest[1]
	default:
		return Key{}, false
	}
	return k, true
}

// Prefix is an app item's folder: "{kind}/{id}/".
func Prefix(kind, id string) string { return kind + "/" + id + "/" }

// AccountPrefix is an account item's folder: "_accounts/{kind}/{id}/".
func AccountPrefix(kind, id string) string { return AccountRoot + Prefix(kind, id) }

// ParseFolder parses an item folder.
func ParseFolder(prefix string) (Key, bool) {
	k, ok := Parse(prefix + ManifestName)
	return k, ok && k.Folder() == prefix
}

// ValidKind accepts a kind name: a segment that is not one of the bucket's
// reserved top-level names ("_…" and "work").
func ValidKind(name string) bool {
	return ValidSegment(name) && !strings.HasPrefix(name, "_") && name != "work"
}

// ValidSegment keeps keys stable ASCII: [A-Za-z0-9._-]{1,128}, no leading dot.
func ValidSegment(s string) bool {
	if s == "" || len(s) > 128 || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// ValidHash accepts a stored file's name: 64 lowercase hex digits, the
// SHA-256 of its bytes.
func ValidHash(name string) bool {
	if len(name) != 64 {
		return false
	}
	for i := 0; i < 64; i++ {
		if c := name[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Hash is the stored name of bytes whose SHA-256 is sum.
func Hash(sum []byte) string { return hex.EncodeToString(sum) }

// A blob reference (a manifest's File.Blob) names a stored file within its
// item folder: "{area}/{sha256}". Identical bytes in the same area are one
// object.

// BlobRef is the reference of the bytes whose SHA-256 is sum in area.
func BlobRef(area string, sum []byte) string { return area + "/" + Hash(sum) }

// MoveRef is ref's bytes in another area.
func MoveRef(ref, area string) string {
	_, h, _ := strings.Cut(ref, "/")
	return area + "/" + h
}

// SplitBlobRef is a blob reference's area and hash.
func SplitBlobRef(ref string) (area, hash string, ok bool) {
	area, hash, ok = strings.Cut(ref, "/")
	switch area {
	case AreaInternal, AreaPrivate, AreaPublic:
	default:
		return "", "", false
	}
	return area, hash, ok && ValidHash(hash)
}

// ValidBlobRef accepts a blob reference.
func ValidBlobRef(ref string) bool {
	_, _, ok := SplitBlobRef(ref)
	return ok
}

// BlobDigest returns the SHA-256 a blob reference names.
func BlobDigest(ref string) ([]byte, bool) {
	_, h, ok := SplitBlobRef(ref)
	if !ok {
		return nil, false
	}
	sum, err := hex.DecodeString(h)
	return sum, err == nil
}

// BlobArea is a blob reference's area, "" when invalid.
func BlobArea(ref string) string {
	area, _, _ := SplitBlobRef(ref)
	return area
}

// Served reports an area the gateway serves.
func Served(area string) bool { return area == AreaPrivate || area == AreaPublic }

// ValidStagedName accepts "u-{uuid}", a canonical lowercase UUID.
func ValidStagedName(name string) bool {
	id, ok := strings.CutPrefix(name, StagedPrefix)
	return ok && validUUID(id)
}

// ValidScratchName accepts "w-{run}-{name}": a canonical lowercase UUID and
// a segment.
func ValidScratchName(name string) bool {
	rest, ok := strings.CutPrefix(name, ScratchPrefix)
	return ok && len(rest) > 37 && validUUID(rest[:36]) && rest[36] == '-' && ValidSegment(rest[37:])
}

// ScratchName is run's scratch name for name; ValidScratchName checks it.
func ScratchName(run, name string) string { return ScratchPrefix + run + "-" + name }

func validUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
