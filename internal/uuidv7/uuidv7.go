// Package uuidv7 mints the ids ContentKit stores as time-ordered UUIDv7s:
// random ones dated now, and deterministic ones for idempotency keys, dated at
// an instant their inputs fix. Secrets, nonces and pseudonyms stay random v4:
// a UUIDv7 shows when it was made and has only 74 random bits.
package uuidv7

import (
	"crypto/sha256"
	"encoding/binary"
	"time"

	"github.com/google/uuid"
)

// New is a random UUIDv7 dated now.
func New() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// NewString is New in canonical form.
func NewString() string { return New().String() }

// Name is the deterministic UUIDv7 of name in namespace: dated at, to the
// millisecond, with SHA-256(namespace, name) in its other 74 bits. at must be
// an instant the inputs fix (At), never the clock, so a retry derives the same id.
func Name(at time.Time, namespace uuid.UUID, name string) uuid.UUID {
	h := sha256.New()
	h.Write(namespace[:])
	h.Write([]byte(name))
	var u uuid.UUID
	copy(u[6:], h.Sum(nil))
	return Stamp(at, u)
}

// Child is the deterministic UUIDv7 of name under parent, dated at parent's
// own instant.
func Child(parent uuid.UUID, name string) uuid.UUID {
	return Name(At(parent.String()), parent, name)
}

// At is the instant of the first UUIDv7 among ids. Other versions carry none:
// with no UUIDv7 among ids it is the Unix epoch, so an id derived from an older
// parent stays deterministic and sorts before every dated one.
func At(ids ...string) time.Time {
	for _, s := range ids {
		if u, err := uuid.Parse(s); err == nil && u.Version() == 7 && u.Variant() == uuid.RFC4122 {
			return time.UnixMilli(int64(binary.BigEndian.Uint64(u[:8]) >> 16)).UTC()
		}
	}
	return time.UnixMilli(0).UTC()
}

// Stamp makes u a UUIDv7 dated t: t's Unix milliseconds in the first 48 bits,
// then the version and variant; u's other 74 bits are kept.
func Stamp(t time.Time, u uuid.UUID) uuid.UUID {
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(t.UnixMilli())<<16)
	copy(u[:6], ts[:6])
	u[6] = 0x70 | u[6]&0x0f
	u[8] = 0x80 | u[8]&0x3f
	return u
}
