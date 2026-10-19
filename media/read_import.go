package media

import (
	"encoding/hex"

	"github.com/open-rails/contentkit/media/layout"
)

// An attached source archive is still work for the version importer after
// placement has finished. Expose that work to editors without changing the
// worker readiness that triggers the import in the first place.
func importPending(k *Kind, f File) bool {
	if k.Publish == nil || k.Publish.Import == "" || !f.IsUpload() || f.Unattached || f.Gone || f.Fail() != nil {
		return false
	}
	p, ok := parsePattern(k.Publish.Import)
	stem, _ := splitExt(f.Path)
	if _, match := p.match(stem); !ok || !match {
		return false
	}
	digest, ok := layout.BlobDigest(f.Blob)
	var source string
	if ok {
		source = hex.EncodeToString(digest)
	} else {
		source, _ = f.Meta["sha256"].(string)
	}
	completed, _ := f.Meta["archive_completed_sha256"].(string)
	return source == "" || completed != source
}
