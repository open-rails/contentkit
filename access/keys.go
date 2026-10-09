package access

import (
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/contentkit/contentref"
)

// KeyKind says what an entitlement key grants.
type KeyKind uint8

const (
	KeyOwn     KeyKind = iota + 1 // content:<tenant>:<kind>:<id>: owning one item; unlocks it at any level
	KeyMembers                    // members:<tenant>:<kind>:<id>: membership of a scope (channel, series)
	KeyTier                       // <tier>: a merchant-wide tier such as premium
)

const (
	ownNamespace     = "content"
	membersNamespace = "members"
	maxComponent     = 64
	// MaxKeyBytes bounds a key: members:<64>:<64>:<36>.
	MaxKeyBytes = len(membersNamespace) + 1 + maxComponent + 1 + maxComponent + 1 + 36
)

// ErrNotContentKey is a string or reference outside the key grammar.
var ErrNotContentKey = errors.New("access: not a content key")

// Key is an entitlement key ContentKit gives meaning to. OpenRails stores keys
// opaquely; construct them here, never by hand. Keys are work-level: a
// version shares its work's key. The zero Key is invalid.
type Key struct {
	kind KeyKind
	ref  contentref.ContentKey // KeyOwn, KeyMembers; ContentVersionID is ""
	tier string                // KeyTier
}

// MakeOwn returns the key that owns item. It refuses an invalid or versioned
// reference and a tenant or kind outside [a-z0-9_-]{1,64}.
func MakeOwn(item contentref.ContentRef) (Key, error) { return makeRef(KeyOwn, item) }

// MakeMembers returns the membership key of scope (a channel, series or
// collection), under the same rules as MakeOwn.
func MakeMembers(scope contentref.ContentRef) (Key, error) { return makeRef(KeyMembers, scope) }

// MakeTier returns a tier key: ^[a-z][a-z0-9_-]{0,62}$.
func MakeTier(name string) (Key, error) {
	if !validTier(name) {
		return Key{}, fmt.Errorf("%w: tier %q must match [a-z][a-z0-9_-]{0,62}", ErrNotContentKey, name)
	}
	return Key{kind: KeyTier, tier: name}, nil
}

// Own is MakeOwn for constants and tests: it panics on an invalid reference.
func Own(item contentref.ContentRef) Key { return must(MakeOwn(item)) }

// Members is MakeMembers, panicking on an invalid reference.
func Members(scope contentref.ContentRef) Key { return must(MakeMembers(scope)) }

// Tier is MakeTier, panicking on an invalid name.
func Tier(name string) Key { return must(MakeTier(name)) }

// ParseKey parses a key string. Anything outside the grammar is
// ErrNotContentKey, including a bare string that is not a valid tier name.
func ParseKey(s string) (Key, error) {
	if len(s) > MaxKeyBytes {
		return Key{}, fmt.Errorf("%w: longer than %d bytes", ErrNotContentKey, MaxKeyBytes)
	}
	ns, rest, ok := strings.Cut(s, ":")
	if !ok {
		return MakeTier(s)
	}
	var kind KeyKind
	switch ns {
	case ownNamespace:
		kind = KeyOwn
	case membersNamespace:
		kind = KeyMembers
	default:
		return Key{}, fmt.Errorf("%w: namespace %q", ErrNotContentKey, ns)
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 {
		return Key{}, fmt.Errorf("%w: %q is not %s:<tenant>:<kind>:<id>", ErrNotContentKey, s, ns)
	}
	return makeRef(kind, contentref.New(parts[0], parts[1], parts[2]))
}

// OwnPrefix is the keyspace of a kind's owned items: "content:<tenant>:<kind>:".
func OwnPrefix(tenant, kind string) string { return ownNamespace + ":" + tenant + ":" + kind + ":" }

// MembersPrefix is the keyspace of a scope kind: "members:<tenant>:<kind>:".
func MembersPrefix(tenant, kind string) string {
	return membersNamespace + ":" + tenant + ":" + kind + ":"
}

// Kind returns what the key grants; 0 for the zero Key.
func (k Key) Kind() KeyKind { return k.kind }

// Ref returns the work an own or members key names.
func (k Key) Ref() (contentref.ContentRef, bool) {
	if k.kind != KeyOwn && k.kind != KeyMembers {
		return contentref.ContentRef{}, false
	}
	return k.ref.Ref(), true
}

// Tier returns a tier key's name.
func (k Key) Tier() (string, bool) { return k.tier, k.kind == KeyTier }

// String is the key as OpenRails stores it; "" for the zero Key.
func (k Key) String() string {
	switch k.kind {
	case KeyOwn:
		return OwnPrefix(k.ref.TenantID, k.ref.ContentKind) + k.ref.ContentID
	case KeyMembers:
		return MembersPrefix(k.ref.TenantID, k.ref.ContentKind) + k.ref.ContentID
	case KeyTier:
		return k.tier
	}
	return ""
}

// MarshalText refuses the zero Key.
func (k Key) MarshalText() ([]byte, error) {
	if k.kind == 0 {
		return nil, fmt.Errorf("%w: zero Key", ErrNotContentKey)
	}
	return []byte(k.String()), nil
}

// UnmarshalText parses with ParseKey.
func (k *Key) UnmarshalText(b []byte) error {
	parsed, err := ParseKey(string(b))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

func makeRef(kind KeyKind, ref contentref.ContentRef) (Key, error) {
	if ref.ContentVersionID != nil {
		return Key{}, fmt.Errorf("%w: %s is a version; keys are work-level", ErrNotContentKey, ref)
	}
	if !validComponent(ref.TenantID) || !validComponent(ref.ContentKind) {
		return Key{}, fmt.Errorf("%w: tenant %q and kind %q must match [a-z0-9_-]{1,64}", ErrNotContentKey, ref.TenantID, ref.ContentKind)
	}
	if err := contentref.ValidateID(ref.ContentID); err != nil {
		return Key{}, fmt.Errorf("%w: %w", ErrNotContentKey, err)
	}
	return Key{kind: kind, ref: ref.Key()}, nil
}

func must(k Key, err error) Key {
	if err != nil {
		panic(err)
	}
	return k
}

func validComponent(s string) bool {
	if s == "" || len(s) > maxComponent {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validTier(s string) bool {
	return len(s) <= 63 && s != "" && s[0] >= 'a' && s[0] <= 'z' && validComponent(s)
}
