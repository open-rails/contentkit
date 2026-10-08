// Package layout defines media object keys, dependency-free so the media
// gateway can classify paths without importing the media runtime:
//
//	{namespace}/{kind}/{id}/manifest.json         gzip JSON; never served
//	                       /private/sha256-{hex}  every blob: uploads, derived files, editor views
//	                       /public/{name}         app-declared names, e.g. cover-460.webp
//	                       /temp/{name}           in-flight writes and staged uploads (u-{uuid}); never served
//	{namespace}/{kind}/_default/public/{name}     a public preset's default image
//
// The URL of an object is its key under /v1/ on the site's media host.
package layout

import (
	"encoding/hex"
	"strings"
)

// Folder areas.
const (
	AreaManifest = "manifest"
	AreaPrivate  = "private"
	AreaPublic   = "public"
	AreaTemp     = "temp"
)

const (
	// ManifestName is the manifest's key within the folder.
	ManifestName = "manifest.json"
	// DefaultID is the item id under which a kind's default public images live.
	DefaultID = "_default"
	// SHA256Prefix starts every blob name.
	SHA256Prefix = "sha256-"
	// StagedPrefix starts a staged upload's name in temp/.
	StagedPrefix = "u-"
	// URLPrefix is the path every media URL starts with.
	URLPrefix = "/v1/"
)

// Key is a parsed object key.
type Key struct {
	Namespace, Kind, ID string
	Area                string
	Name                string // the name within the area; "" for the manifest
}

// Parse classifies an object key; ok is false for anything outside the layout.
func Parse(key string) (Key, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 4 || len(parts) > 5 || !ValidSegment(parts[0]) || !ValidSegment(parts[1]) || !ValidSegment(parts[2]) {
		return Key{}, false
	}
	k := Key{Namespace: parts[0], Kind: parts[1], ID: parts[2]}
	rest := parts[3:]
	switch {
	case len(rest) == 1 && rest[0] == ManifestName:
		k.Area = AreaManifest
	case len(rest) == 2 && rest[0] == AreaPrivate && ValidHashName(rest[1]),
		len(rest) == 2 && (rest[0] == AreaPublic || rest[0] == AreaTemp) && ValidSegment(rest[1]):
		k.Area, k.Name = rest[0], rest[1]
	default:
		return Key{}, false
	}
	return k, true
}

// Prefix is an item's folder: "{namespace}/{kind}/{id}/".
func Prefix(namespace, kind, id string) string { return namespace + "/" + kind + "/" + id + "/" }

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

// ValidHashName accepts "sha256-{64 lowercase hex}".
func ValidHashName(name string) bool {
	_, ok := ParseSHA256Name(name)
	return ok
}

// ParseSHA256Name returns the digest of a "sha256-{hex}" name.
func ParseSHA256Name(name string) ([]byte, bool) {
	h, ok := strings.CutPrefix(name, SHA256Prefix)
	if !ok || len(h) != 64 || strings.ToLower(h) != h {
		return nil, false
	}
	sum, err := hex.DecodeString(h)
	return sum, err == nil
}

// ValidStagedName accepts "u-{uuid}", a canonical lowercase UUID.
func ValidStagedName(name string) bool {
	id, ok := strings.CutPrefix(name, StagedPrefix)
	if !ok || len(id) != 36 {
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

// SHA256Name names a blob by its digest: "sha256-{hex}".
func SHA256Name(sum []byte) string { return SHA256Prefix + hex.EncodeToString(sum) }
