// Package token signs and verifies media access tokens, shared by the host
// signer and the access worker so the format cannot drift:
//
//	{kid}.{exp}.base64url(HMAC-SHA256(secret, "{scope}|{exp}"))
//
// A scope is either a folder prefix ending in "/", which covers the objects
// directly under it, or one object key. A download scope binds a
// Content-Disposition name: "{key}#dl={name}". Tokens are bearer tokens,
// revoked only by expiry.
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
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

// CookieName carries a folder token in cookie delivery mode.
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

// Expiry is ceil((now + ttl) / window) * window.
func Expiry(now time.Time, ttl, window time.Duration) time.Time {
	if window <= 0 {
		window = DefaultWindow
	}
	w := int64(window / time.Second)
	t := now.Add(ttl).Unix()
	return time.Unix((t+w-1)/w*w, 0).UTC()
}

// FileScope scopes a token to one object key.
func FileScope(key string) string { return key }

// DownloadScope scopes a token to one key served under a download name.
func DownloadScope(key, name string) string { return key + "#dl=" + name }

// Sign signs scope until exp with the current key.
func (r Ring) Sign(scope string, exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return r.current.ID + "." + e + "." + mac(r.current.Secret, scope, e)
}

// Verify checks tok for object key at now. A non-empty dl requires a download
// scope for exactly that name; otherwise the token must cover the key itself
// or the folder directly containing it.
func (r Ring) Verify(tok, key, dl string, now time.Time) error {
	kid, rest, ok := strings.Cut(tok, ".")
	if !ok {
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
	if key == "" || strings.HasSuffix(key, "/") {
		return ErrInvalid
	}
	var scopes []string
	if dl != "" {
		scopes = []string{DownloadScope(key, dl)}
	} else {
		scopes = []string{key}
		if i := strings.LastIndexByte(key, '/'); i >= 0 {
			scopes = append(scopes, key[:i+1])
		}
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return ErrMalformed
	}
	for _, s := range scopes {
		want, _ := base64.RawURLEncoding.DecodeString(mac(secret, s, e))
		if hmac.Equal(got, want) {
			return nil
		}
	}
	return ErrInvalid
}

// Attachment is the Content-Disposition for a signed download name: an ASCII
// fallback plus the RFC 5987 UTF-8 name.
func Attachment(name string) string {
	var ascii, ext strings.Builder
	for _, c := range name {
		if c < 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			ascii.WriteByte('_')
		} else {
			ascii.WriteRune(c)
		}
	}
	for _, b := range []byte(name) {
		if b < 0x80 && (b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("!#$&+-.^_`|~", b) >= 0) {
			ext.WriteByte(b)
		} else {
			fmt.Fprintf(&ext, "%%%02X", b)
		}
	}
	return `attachment; filename="` + ascii.String() + `"; filename*=UTF-8''` + ext.String()
}

func mac(secret []byte, scope, exp string) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(scope + "|" + exp))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
