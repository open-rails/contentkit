// Package subject names the person an activity row, reading state or recent
// search belongs to: the account id when signed in, otherwise an id derived
// from the browser's anonymous id. It also reads the request's local UTC
// offset.
package subject

import (
	"crypto/sha256"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/access"
)

// Header carries a signed-out browser's anonymous id: a random UUID it keeps
// in first-party storage and replaces at sign-out.
const Header = "ContentKit-Anonymous-Id"

// OffsetHeader carries the browser's local time minus UTC, in minutes.
const OffsetHeader = "ContentKit-UTC-Offset"

// cfTimezone is Cloudflare's visitor time zone header, the offset's fallback.
const cfTimezone = "cf-timezone"

// Account is an account's subject; false when the id is not a UUID or is a
// UUIDv8, which is reserved for anonymous subjects.
func Account(id string) (uuid.UUID, bool) {
	u, err := uuid.Parse(id)
	return u, err == nil && len(id) == 36 && u != uuid.Nil && !IsAnonymous(u)
}

// Anonymous is the subject of a browser's anonymous id in app. The id is a
// bearer identity, so it is never stored: the subject is a UUIDv8 of its hash.
func Anonymous(app uuid.UUID, browser string) (uuid.UUID, bool) {
	b, err := uuid.Parse(strings.TrimSpace(browser))
	if err != nil || len(strings.TrimSpace(browser)) != 36 || b == uuid.Nil {
		return uuid.Nil, false
	}
	h := sha256.New()
	h.Write([]byte("contentkit anonymous\x00"))
	h.Write(app[:])
	h.Write(b[:])
	var u uuid.UUID
	copy(u[:], h.Sum(nil))
	u[6] = u[6]&0x0f | 0x80
	u[8] = u[8]&0x3f | 0x80
	return u, true
}

// IsAnonymous reports whether subject is a signed-out browser's (Anonymous).
func IsAnonymous(subject uuid.UUID) bool { return subject.Version() == 8 }

// Of is the actor's subject: its account when it has one in app, else the
// anonymous id it sent. False for an application or a request without either.
func Of(app uuid.UUID, actor access.Actor, browser string) (uuid.UUID, bool) {
	if actor.Member() {
		return Account(actor.AccountID())
	}
	if !actor.Anonymous {
		return uuid.Nil, false
	}
	return Anonymous(app, browser)
}

// FromRequest is Of with the anonymous id from r's header.
func FromRequest(app uuid.UUID, actor access.Actor, r *http.Request) (uuid.UUID, bool) {
	return Of(app, actor, r.Header.Get(Header))
}

// maxOffset bounds an offset: the world's zones run from UTC-12 to UTC+14.
const maxOffset = 14 * 60

// Offset is r's local time minus UTC at now, in minutes: the browser's
// header, else Cloudflare's time zone, else 0.
func Offset(r *http.Request, now time.Time) int16 {
	if v := strings.TrimSpace(r.Header.Get(OffsetHeader)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= -maxOffset && n <= maxOffset {
			return int16(n)
		}
	}
	if loc := location(strings.TrimSpace(r.Header.Get(cfTimezone))); loc != nil {
		_, seconds := now.In(loc).Zone()
		return int16(seconds / 60)
	}
	return 0
}

var zones sync.Map // IANA name -> *time.Location; only names that load

func location(name string) *time.Location {
	if name == "" || len(name) > 64 {
		return nil
	}
	if v, ok := zones.Load(name); ok {
		return v.(*time.Location)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	zones.Store(name, loc)
	return loc
}
