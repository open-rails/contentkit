package mediastore

import (
	"errors"

	media "github.com/open-rails/contentkit/media"
)

var ErrPublicationLayoutRequired = errors.New("media: publication is being restored")

// PublicationLayoutReady is a local manifest check for the reader and
// publisher; it performs no migration or storage effects.
func (m *Manifest) PublicationLayoutReady() bool {
	return m != nil && m.PublicationLayout == media.PublicationLayoutVersion && !m.Deleted
}
