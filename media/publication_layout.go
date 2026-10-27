package media

import (
	"errors"
)

var ErrPublicationLayoutRequired = errors.New("media: publication is being restored")

// PublicationLayoutReady is a local manifest check for the reader and
// publisher; it performs no migration or storage effects.
func (m *Manifest) PublicationLayoutReady() bool {
	return m != nil && m.PublicationLayout == PublicationLayoutVersion && !m.Deleted
}
