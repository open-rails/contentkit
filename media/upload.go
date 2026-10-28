package media

import (
	"github.com/open-rails/contentkit/contentref"
)

const (
	MaxSinglePut = 64 << 20
)

const (
	MinPartSize = 8 << 20
)

const (
	MaxPartSize = 16 << 20
)

// PresignRequest declares one upload; SHA256 is the whole file's: it finds
// an identical blob already in the folder and binds a single PUT's body.
type PresignRequest struct {
	Ref    contentref.ContentRef
	Path   string
	Type   string
	Size   int64
	SHA256 []byte
}

// Presigned is the upload plan: Exists (commit it), a single Put, or a
// Multipart upload. Path is the path to commit; Blob is the name to commit:
// the folder's blob when Exists, else the staged upload (u-{uuid}) that the
// PUT or the parts write.
type Presigned struct {
	Path            string
	SourceName      string
	Blob            string
	Exists          bool
	Put             *PresignedRequest
	Multipart       *Multipart
	ProcessOnUpload bool
}

// Multipart carries the opaque ticket for PresignParts, ListParts, Complete
// and Abort, and the part-size bounds the client adapts within.
type Multipart struct {
	Ticket      string
	MinPartSize int64
	MaxPartSize int64
	MaxParts    int
}

// PartRequest is one part to presign: its exact length and SHA-256.
type PartRequest struct {
	Number int32
	Size   int64
	SHA256 []byte
}

// PresignedPart is a part URL.
type PresignedPart struct {
	Number int32
	PresignedRequest
}
