// Package token signs and verifies media access tokens, shared by the host
// signer and the media gateway so the format cannot drift:
//
//	{kid}.{exp}.base64url(HMAC-SHA256(secret, "{scope}|{exp}"))
//
// An item token's scope is an item's, "{ns}/{kind}/{id}" or a version's
// "{ns}/{kind}/{id}/{version}": every file in the item's private/ or none. A
// private-file token covers one immutable key, never its siblings. A download
// token covers one key in download/, which neither private token opens.
// Tokens are bearer tokens, revoked only by expiry.
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/open-rails/contentkit/media/layout"
	"strconv"
	"strings"
	"time"
)

var (
	ErrMalformed  = errors.New("token: malformed")
	ErrExpired    = errors.New("token: expired")
	ErrUnknownKey = errors.New("token: unknown key")
	ErrInvalid    = errors.New("token: signature does not cover this path")
)

// CookieName carries an item token in cookie delivery mode.
const CookieName = "mt"

// DefaultWindow aligns expiries so tokens and URLs repeat within a window.
const DefaultWindow = 4 * time.Hour

// Key is one signing key.
type Key struct {
	ID     string
	Secret []byte
}

// Ring signs with the current key and verifies with the current or previous one.
type Ring struct {
	current  Key
	previous *Key
}

// NewRing validates keys: ids are non-empty without '.', secrets at least 32 bytes.
func NewRing(current Key, previous *Key) (Ring, error) {
	for _, k := range []*Key{&current, previous} {
		if k == nil {
			continue
		}
		if k.ID == "" || strings.ContainsAny(k.ID, ".|") || len(k.Secret) < 32 {
			return Ring{}, fmt.Errorf("token: key %q needs a dot-free id and a secret of at least 32 bytes", k.ID)
		}
	}
	if previous != nil && previous.ID == current.ID {
		return Ring{}, fmt.Errorf("token: previous key reuses id %q", current.ID)
	}
	return Ring{current: current, previous: previous}, nil
}

// ParseKey parses "{kid}:{base64 secret}" (standard or URL alphabet, padding
// optional), the form hosts and the access worker read from their secret store.
func ParseKey(s string) (Key, error) {
	id, enc, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return Key{}, fmt.Errorf("token: key must be \"{kid}:{base64 secret}\"")
	}
	enc = strings.TrimRight(enc, "=")
	secret, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil {
		if secret, err = base64.RawURLEncoding.DecodeString(enc); err != nil {
			return Key{}, fmt.Errorf("token: key %q: secret is not base64", id)
		}
	}
	return Key{ID: id, Secret: secret}, nil
}

// ParseRing builds a ring from ParseKey strings; previous may be empty.
func ParseRing(current, previous string) (Ring, error) {
	cur, err := ParseKey(current)
	if err != nil {
		return Ring{}, err
	}
	var prev *Key
	if strings.TrimSpace(previous) != "" {
		p, err := ParseKey(previous)
		if err != nil {
			return Ring{}, err
		}
		prev = &p
	}
	return NewRing(cur, prev)
}

// Expiry rounds now+ttl up to a window. Token timestamps store whole Unix
// seconds, so fractional-second windows round up to whole seconds.
func Expiry(now time.Time, ttl, window time.Duration) time.Time {
	if window <= 0 {
		window = DefaultWindow
	}
	w := int64(window / time.Second)
	if window%time.Second != 0 {
		w++
	}
	target := now.Add(ttl)
	seconds := target.Unix()
	if target.Nanosecond() != 0 {
		seconds++
	}
	intervals := seconds / w
	if seconds%w > 0 {
		intervals++
	}
	return time.Unix(intervals*w, 0).UTC()
}

// Sign signs scope until exp with the current key.
func (r Ring) Sign(scope string, exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return r.current.ID + "." + e + "." + mac(r.current.Secret, scope, e)
}

// PrivateFileScope covers one private object key, including tenant, app, kind,
// item and version. Its domain separator keeps it distinct from download and
// whole-item scopes.
func PrivateFileScope(key string) string { return "private-file|" + key }

// VerifyPrivate checks an exact-file or whole-item token for a private key.
func (r Ring) VerifyPrivate(tok, key string, now time.Time) error {
	k, area, ok := keyFolder(key)
	if !ok || area != layout.AreaPrivate {
		return ErrInvalid
	}
	if err := r.VerifyScope(tok, PrivateFileScope(key), now); !errors.Is(err, ErrInvalid) {
		return err
	}
	return r.VerifyScope(tok, strings.TrimSuffix(k.Folder(), "/"), now)
}

// VerifyScope checks tok for exactly scope at now.
func (r Ring) VerifyScope(tok, scope string, now time.Time) error {
	kid, rest, ok := strings.Cut(tok, ".")
	if !ok || kid == "" { // a zero Ring's empty key id would verify an empty secret
		return ErrMalformed
	}
	e, sig, ok := strings.Cut(rest, ".")
	if !ok || e == "" || sig == "" {
		return ErrMalformed
	}
	exp, err := strconv.ParseInt(e, 10, 64)
	if err != nil || strconv.FormatInt(exp, 10) != e {
		return ErrMalformed
	}
	var secret []byte
	switch {
	case kid == r.current.ID:
		secret = r.current.Secret
	case r.previous != nil && kid == r.previous.ID:
		secret = r.previous.Secret
	default:
		return ErrUnknownKey
	}
	if now.Unix() >= exp {
		return ErrExpired
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return ErrMalformed
	}
	if want, _ := base64.RawURLEncoding.DecodeString(mac(secret, scope, e)); !hmac.Equal(got, want) {
		return ErrInvalid
	}
	return nil
}

func mac(secret []byte, scope, exp string) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(scope + "|" + exp))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func keyFolder(key string) (layout.Key, string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 3 || parts[len(parts)-1] == "" {
		return layout.Key{}, "", false
	}
	area := parts[len(parts)-2]
	k, ok := layout.ParseFolder(strings.Join(parts[:len(parts)-2], "/") + "/")
	return k, area, ok
}
