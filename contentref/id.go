package contentref

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
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
	if id[14] != '7' || (id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b') {
		return &IDError{ID: id}
	}
	return nil
}

// NewID returns a new content id (a UUIDv7).
func NewID() string { return IDAt(time.Now()) }

// IDAt returns a UUIDv7 whose timestamp is t (millisecond precision) and whose
// remaining 74 bits are random, for ids generated once and stored. Imports
// that must derive the same id on every run use LegacyID.
func IDAt(t time.Time) string {
	var u [16]byte
	if _, err := rand.Read(u[6:]); err != nil {
		panic(err)
	}
	return v7(t, u)
}

// legacyEpoch dates legacy rows without a created_at, in legacy id order.
var legacyEpoch = time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)

// LegacyID is the deterministic content id of a legacy row: a UUIDv7 whose
// timestamp is createdAt (zero: 2010-01-01 UTC plus legacyID milliseconds) and
// whose other 74 bits hash "{namespace}/{kind}/{legacyID}", so re-running an
// import (media included) yields the same ids and folders. namespace is the
// host's import namespace ("doujins", "hentai0"): lowercase letters, digits,
// '_' or '-'; LegacyID panics on another (a host constant).
func LegacyID(namespace, kind string, legacyID int64, createdAt time.Time) string {
	if !validNamespace(namespace) {
		panic(fmt.Sprintf("contentref: invalid LegacyID namespace %q", namespace))
	}
	if createdAt.IsZero() {
		createdAt = legacyEpoch.Add(time.Duration(legacyID) * time.Millisecond)
	}
	var u [16]byte
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d", namespace, kind, legacyID)))
	copy(u[6:], h[:10])
	return v7(createdAt, u)
}

func validNamespace(ns string) bool {
	if ns == "" || len(ns) > 64 {
		return false
	}
	for _, c := range ns {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func v7(t time.Time, u [16]byte) string {
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(t.UnixMilli())<<16)
	copy(u[:6], ts[:6])
	u[6] = 0x70 | u[6]&0x0f
	u[8] = 0x80 | u[8]&0x3f
	h := hex.EncodeToString(u[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
