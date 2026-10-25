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
	KeyContent KeyKind = iota + 1 // <tenant>:content:<id>: legacy parent grant; audit/migration only
	KeyVersion                    // <tenant>:content:<id>/<version>: one version
	KeyEntity                     // <tenant>:group:<id>: every member of an entity, and content whose rule names it
	KeyTier                       // <tier>: a tier from config, such as premium
)

const (
	contentNamespace = "content"
	// Existing billing products and purchases retain this opaque namespace.
	entityNamespace = "group"
	maxTenant       = 64
	// MaxKeyBytes bounds a key: <tenant>:content:<id>/<version>.
	MaxKeyBytes = maxTenant + len(":content:") + 36 + 1 + 36
)

// ErrNotContentKey is a string outside the key grammar.
var ErrNotContentKey = errors.New("access: not a content key")

// Key is an entitlement key ContentKit gives meaning to. OpenRails stores keys
// opaquely; construct them here, never by hand. The zero Key is invalid.
type Key struct {
	kind    KeyKind
	tenant  string
	id      string // the content or entity id
	version string // KeyVersion
}

// ContentKey identifies a legacy parent grant for audit and explicit migration.
// It never opens a version. New products must use VersionKey for each item.
//
// Deprecated: Use VersionKey for audience access. ContentKey remains for
// explicitly auditing and migrating old billing records.
func ContentKey(tenant, id string) (Key, error) { return makeKey(KeyContent, tenant, id, "") }

// VersionKey is the key of one version of content.
func VersionKey(tenant, content, version string) (Key, error) {
	return makeKey(KeyVersion, tenant, content, version)
}

// EntityKey is the key of entity id.
func EntityKey(tenant, id string) (Key, error) { return makeKey(KeyEntity, tenant, id, "") }

// TierKey is a tier's key, its bare name: ^[a-z][a-z0-9_-]{0,62}$.
func TierKey(name string) (Key, error) {
	if !validTier(name) {
		return Key{}, fmt.Errorf("%w: tier %q must match [a-z][a-z0-9_-]{0,62}", ErrNotContentKey, name)
	}
	return Key{kind: KeyTier, id: name}, nil
}

// ContentPrefix is the keyspace of a tenant's content and version keys.
func ContentPrefix(tenant string) string { return tenant + ":" + contentNamespace + ":" }

// EntityPrefix is the keyspace of a tenant's entity keys.
func EntityPrefix(tenant string) string { return tenant + ":" + entityNamespace + ":" }

// ParseKey parses a key string. Anything outside the grammar is
// ErrNotContentKey, a bare string that is not a valid tier name included.
func ParseKey(s string) (Key, error) {
	if len(s) > MaxKeyBytes {
		return Key{}, fmt.Errorf("%w: longer than %d bytes", ErrNotContentKey, MaxKeyBytes)
	}
	tenant, rest, ok := strings.Cut(s, ":")
	if !ok {
		return TierKey(s)
	}
	ns, id, ok := strings.Cut(rest, ":")
	switch {
	case !ok:
		return Key{}, fmt.Errorf("%w: %q", ErrNotContentKey, s)
	case ns == entityNamespace:
		return EntityKey(tenant, id)
	case ns != contentNamespace:
		return Key{}, fmt.Errorf("%w: namespace %q", ErrNotContentKey, ns)
	}
	if content, version, ok := strings.Cut(id, "/"); ok {
		return VersionKey(tenant, content, version)
	}
	return ContentKey(tenant, id)
}

// Kind returns what the key grants; 0 for the zero Key.
func (k Key) Kind() KeyKind { return k.kind }

// Tenant is the tenant of a content, version or entity key.
func (k Key) Tenant() string { return k.tenant }

// Content is the content id of a content or version key.
func (k Key) Content() string {
	if k.kind == KeyContent || k.kind == KeyVersion {
		return k.id
	}
	return ""
}

// Version is the version id of a version key.
func (k Key) Version() string { return k.version }

// Entity is the entity id of an entity key.
func (k Key) Entity() string {
	if k.kind == KeyEntity {
		return k.id
	}
	return ""
}

// Tier is the name of a tier key.
func (k Key) Tier() string {
	if k.kind == KeyTier {
		return k.id
	}
	return ""
}

// String is the key as OpenRails stores it; "" for the zero Key.
func (k Key) String() string {
	switch k.kind {
	case KeyContent:
		return ContentPrefix(k.tenant) + k.id
	case KeyVersion:
		return ContentPrefix(k.tenant) + k.id + "/" + k.version
	case KeyEntity:
		return EntityPrefix(k.tenant) + k.id
	case KeyTier:
		return k.id
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

func makeKey(kind KeyKind, tenant, id, version string) (Key, error) {
	if !ValidTenant(tenant) {
		return Key{}, fmt.Errorf("%w: tenant %q must match [a-z0-9_-]{1,64}", ErrNotContentKey, tenant)
	}
	if err := contentref.ValidateID(id); err != nil {
		return Key{}, fmt.Errorf("%w: %w", ErrNotContentKey, err)
	}
	if kind == KeyVersion {
		if err := contentref.ValidateID(version); err != nil {
			return Key{}, fmt.Errorf("%w: version: %w", ErrNotContentKey, err)
		}
	}
	return Key{kind: kind, tenant: tenant, id: id, version: version}, nil
}

// ValidTenant reports a tenant id usable in keys: [a-z0-9_-]{1,64}.
func ValidTenant(s string) bool {
	if s == "" || len(s) > maxTenant {
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
	return len(s) <= 63 && s != "" && s[0] >= 'a' && s[0] <= 'z' && ValidTenant(s)
}
