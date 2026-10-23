package access

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/contentref"
)

const (
	id1 = "01920000-0000-7000-8000-000000000001"
	id2 = "01920000-0000-7000-8000-000000000002"
)

func must(k Key, err error) Key {
	if err != nil {
		panic(err)
	}
	return k
}

func TestKeysRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		key  Key
		want string
		kind KeyKind
	}{
		{must(ContentKey("onlydemo", id1)), "onlydemo:content:" + id1, KeyContent},
		{must(VersionKey("onlydemo", id1, id2)), "onlydemo:content:" + id1 + "/" + id2, KeyVersion},
		{must(EntityKey("hentai0", id1)), "hentai0:group:" + id1, KeyEntity},
		{must(TierKey("premium")), "premium", KeyTier},
		{must(TierKey("gold_2-x")), "gold_2-x", KeyTier},
	} {
		if s := tc.key.String(); s != tc.want || tc.key.Kind() != tc.kind {
			t.Fatalf("%v: String %q Kind %d, want %q %d", tc.key, s, tc.key.Kind(), tc.want, tc.kind)
		}
		back, err := ParseKey(tc.want)
		if err != nil || back != tc.key {
			t.Fatalf("ParseKey(%q) = %v, %v", tc.want, back, err)
		}
		b, err := json.Marshal(map[string]Key{"k": tc.key})
		if err != nil || string(b) != `{"k":"`+tc.want+`"}` {
			t.Fatalf("json %s %v", b, err)
		}
		var m map[string]Key
		if err := json.Unmarshal(b, &m); err != nil || m["k"] != tc.key {
			t.Fatalf("json round trip %v %v", m, err)
		}
	}
	v := must(VersionKey("t", id1, id2))
	if v.Content() != id1 || v.Version() != id2 || v.Tenant() != "t" || v.Entity() != "" || v.Tier() != "" {
		t.Fatalf("version key accessors: %+v", v)
	}
	if g := must(EntityKey("t", id2)); g.Entity() != id2 || g.Content() != "" {
		t.Fatal("group key accessors")
	}
	if !strings.HasPrefix(v.String(), ContentPrefix("t")) || !strings.HasPrefix(must(EntityKey("t", id1)).String(), EntityPrefix("t")) {
		t.Fatal("key outside its prefix")
	}
	if _, err := (Key{}).MarshalText(); !errors.Is(err, ErrNotContentKey) {
		t.Fatal("zero key marshals")
	}
	if k := must(VersionKey(strings.Repeat("a", 64), id1, id2)); len(k.String()) != MaxKeyBytes {
		t.Fatalf("longest key is %d bytes, MaxKeyBytes %d", len(k.String()), MaxKeyBytes)
	}
}

func TestKeysRefuse(t *testing.T) {
	upper := "01920000-0000-7000-8000-00000000000A"
	v4 := "01920000-0000-4000-8000-000000000001"
	long := strings.Repeat("a", 65)
	for _, tc := range []struct{ tenant, id string }{
		{"t", upper}, {"t", v4}, {"", id1}, {"T", id1}, {"t:x", id1}, {long, id1}, {"t", "x"},
	} {
		if _, err := ContentKey(tc.tenant, tc.id); !errors.Is(err, ErrNotContentKey) {
			t.Errorf("ContentKey(%q, %q) = %v", tc.tenant, tc.id, err)
		}
		if _, err := EntityKey(tc.tenant, tc.id); !errors.Is(err, ErrNotContentKey) {
			t.Errorf("GroupKey(%q, %q) = %v", tc.tenant, tc.id, err)
		}
		if _, err := VersionKey("t", id1, tc.id); !errors.Is(err, ErrNotContentKey) && tc.id != id1 {
			t.Errorf("VersionKey(t, %q) = %v", tc.id, err)
		}
	}
	if _, err := ContentKey("t", upper); !errors.Is(err, contentref.ErrInvalidID) {
		t.Errorf("upper-case id: %v", err)
	}
	for _, name := range []string{"", "Premium", "1premium", "_x", "pre:mium", "pre mium", strings.Repeat("a", 64), "prémium"} {
		if _, err := TierKey(name); !errors.Is(err, ErrNotContentKey) {
			t.Errorf("TierKey(%q) = %v", name, err)
		}
	}
	for _, s := range []string{
		"", ":", "t:content:", "t:content:" + id1 + "/", "t:content:" + id1 + "/" + id2 + "/" + id2,
		"t:content:" + upper, ":content:" + id1, "t:Content:" + id1, "t:groups:" + id1, "t:group:" + id1 + "/" + id2,
		"content:t:post:" + id1, "members:t:channel:" + id1, "premium:", "t:content;" + id1, long + ":content:" + id1,
		"t:content:" + id1 + strings.Repeat("0", MaxKeyBytes),
	} {
		if k, err := ParseKey(s); !errors.Is(err, ErrNotContentKey) {
			t.Errorf("ParseKey(%q) = %v, %v", s, k, err)
		}
	}
}

// Every string ParseKey accepts is canonical: it prints back to itself and
// stays within MaxKeyBytes.
func FuzzParseKey(f *testing.F) {
	for _, s := range []string{"premium", "t:content:" + id1, "t:content:" + id1 + "/" + id2, "t:group:" + id1, "t:content:", "a:b:c", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		k, err := ParseKey(s)
		if err != nil {
			if !errors.Is(err, ErrNotContentKey) || k != (Key{}) {
				t.Fatalf("ParseKey(%q) = %v, %v", s, k, err)
			}
			return
		}
		if k.String() != s || len(s) > MaxKeyBytes || k.Kind() == 0 {
			t.Fatalf("ParseKey(%q) = %v (%q)", s, k, k.String())
		}
		if back, err := ParseKey(k.String()); err != nil || back != k {
			t.Fatalf("round trip %q: %v %v", s, back, err)
		}
	})
}
