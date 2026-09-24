package contentref

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidID is a content id that is not a canonical UUIDv7.
var ErrInvalidID = errors.New("contentref: content_id must be a canonical lowercase UUIDv7")

// IDError is a rejected content id; errors.Is(err, ErrInvalidID) holds.
type IDError struct{ ID string }

func (e *IDError) Error() string { return fmt.Sprintf("%v, got %q", ErrInvalidID, e.ID) }
func (e *IDError) Unwrap() error { return ErrInvalidID }

// ValidateID accepts only a canonical content id: 36 characters, lowercase
// hex, version 7, RFC 9562 variant. A content id names a media folder and a
// search/signal key, and is never reused: time-ordered UUIDs cannot repeat
// after a host's database or sequence is reset.
func ValidateID(id string) error {
	if len(id) != 36 {
		return &IDError{ID: id}
	}
	for i := 0; i < 36; i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return &IDError{ID: id}
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return &IDError{ID: id}
			}
		}
	}
	u := uuid.MustParse(id)
	if u.Version() != 7 || u.Variant() != uuid.RFC4122 {
		return &IDError{ID: id}
	}
	return nil
}

// NewID returns a new content id (a UUIDv7).
func NewID() string { return IDAt(time.Now()) }

// IDAt returns a UUIDv7 whose timestamp is t (millisecond precision) and whose
// remaining 74 bits are random. Backfills derive ids from each legacy row's
// created_at so the new ids keep the legacy order.
func IDAt(t time.Time) string {
	var u uuid.UUID
	if _, err := rand.Read(u[6:]); err != nil {
		panic(err)
	}
	ms := uint64(t.UnixMilli())
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms<<16)
	copy(u[:6], ts[:6])
	u[6] = 0x70 | u[6]&0x0f
	u[8] = 0x80 | u[8]&0x3f
	return u.String()
}
