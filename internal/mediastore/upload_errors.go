package mediastore

import (
	"errors"
	"fmt"

	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
)

// AsImageError returns the refusal in err's chain, or nil.
func AsImageError(err error) *media.ImageError {
	var ie *media.ImageError
	if errors.As(err, &ie) {
		return ie
	}
	return nil
}

// editErr is an edit refused against known dims: its ImageError, else invalid_request.
func editErr(err error, format string, a ...any) error {
	if ie := AsImageError(err); ie != nil {
		return ie
	}
	return uploadErr(media.CodeInvalid, format, append(a, err)...)
}

func uploadErr(code, format string, a ...any) *media.UploadError {
	return &media.UploadError{Code: code, Message: fmt.Sprintf(format, a...)}
}

// tooManyFiles refuses a commit past max files, which details carry.
func tooManyFiles(max int, format string, a ...any) *media.UploadError {
	e := uploadErr(media.CodeTooManyFiles, format, a...)
	e.Details = &media.ErrorDetails{Max: max}
	return e
}

// AsUploadError classifies err: an *UploadError, or the kind and registry
// sentinels. ok is false for internal failures.
func AsUploadError(err error) (*media.UploadError, bool) {
	var ue *media.UploadError
	switch {
	case errors.As(err, &ue):
		return ue, true
	case AsImageError(err) != nil:
		ie := AsImageError(err)
		return &media.UploadError{Code: ie.Code, Message: ie.Message, Details: &ie.Details}, true
	case errors.Is(err, ErrUnknownKind):
		return &media.UploadError{Code: media.CodeNotFound, Message: "no such media kind"}, true
	case errors.Is(err, contentref.ErrInvalidID):
		return &media.UploadError{Code: media.CodeInvalid, Message: "the id must be a canonical lowercase UUIDv7"}, true
	case errors.Is(err, ErrInvalidAccountID):
		return &media.UploadError{Code: media.CodeInvalid, Message: "the account id must be a canonical lowercase UUID"}, true
	case errors.As(err, new(*MissingObjectError)):
		return &media.UploadError{Code: media.CodeNotUploaded, Message: "a referenced file is gone; upload or copy again"}, true
	case errors.As(err, new(*SettlingError)):
		return &media.UploadError{Code: media.CodeConflict, Message: "the file is being deleted; upload it again later"}, true
	case errors.Is(err, ErrManifestConflict):
		return &media.UploadError{Code: media.CodeConflict, Message: "the item kept changing; try again"}, true
	}
	return nil, false
}
