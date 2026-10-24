package versions

import (
	"encoding/hex"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

const digestMeta = "sha256"

func pageDigest(f media.File) string {
	if sum, ok := layout.BlobDigest(f.Blob); ok {
		return hex.EncodeToString(sum)
	}
	s, _ := f.Meta[digestMeta].(string)
	return s
}
