package access

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/contentref"
)

const id1 = "01920000-0000-7000-8000-000000000001"

func TestKeysRoundTrip(t *testing.T) {
	post := contentref.New("onlydemo", "post", id1)
	channel := contentref.New("onlydemo", "channel", id1)
	for _, tc := range []struct {
		key  Key
		want string
		kind KeyKind
	}{
		{Own(post), "content:onlydemo:post:" + id1, KeyOwn},
		{Members(channel), "members:onlydemo:channel:" + id1, KeyMembers},
		{Tier("premium"), "premium", KeyTier},
		{Tier("gold_2-x"), "gold_2-x", KeyTier},
		{Own(contentref.New("hentai0", "voice_actor", id1)), "content:hentai0:voice_actor:" + id1, KeyOwn},
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
	if r, ok := Own(post).Ref(); !ok || !r.Equal(post) {
		t.Fatal("Own.Ref")
	}
	if _, ok := Own(post).Tier(); ok {
		t.Fatal("own key has a tier")
	}
	if n, ok := Tier("premium").Tier(); !ok || n != "premium" {
		t.Fatal("Tier.Tier")
	}
	if _, ok := Tier("premium").Ref(); ok {
		t.Fatal("tier key has a ref")
	}
	if OwnPrefix("onlydemo", "post") != "content:onlydemo:post:" || MembersPrefix("onlydemo", "channel") != "members:onlydemo:channel:" {
		t.Fatal("prefixes")
	}
	if !strings.HasPrefix(Own(post).String(), OwnPrefix("onlydemo", "post")) {
		t.Fatal("own key outside its prefix")
	}
	if _, err := (Key{}).MarshalText(); !errors.Is(err, ErrNotContentKey) {
		t.Fatal("zero key marshals")
	}
	long := strings.Repeat("a", 64)
	k := Members(contentref.New(long, long, id1))
	if len(k.String()) != MaxKeyBytes {
		t.Fatalf("longest key is %d bytes, MaxKeyBytes %d", len(k.String()), MaxKeyBytes)
	}
}

func TestKeysRefuse(t *testing.T) {
	upper := "01920000-0000-7000-8000-00000000000A"
	v4 := "01920000-0000-4000-8000-000000000001"
	long := strings.Repeat("a", 65)
	for _, ref := range []contentref.ContentRef{
		contentref.NewVersion("t", "post", id1, "v1"), // versioned
		contentref.New("t", "post", upper),
		contentref.New("t", "post", v4),
		contentref.New("", "post", id1),
		contentref.New("t", "", id1),
		contentref.New("T", "post", id1),
		contentref.New("t", "po:st", id1),
		contentref.New("t:x", "post", id1),
		contentref.New(long, "post", id1),
		contentref.New("t", long, id1),
	} {
		if _, err := MakeOwn(ref); !errors.Is(err, ErrNotContentKey) {
			t.Errorf("MakeOwn(%+v) = %v", ref, err)
		}
		if _, err := MakeMembers(ref); !errors.Is(err, ErrNotContentKey) {
			t.Errorf("MakeMembers(%+v) = %v", ref, err)
		}
	}
	if _, err := MakeOwn(contentref.New("t", "post", upper)); !errors.Is(err, contentref.ErrInvalidID) {
		t.Errorf("upper-case id: %v", err)
	}
	for _, name := range []string{"", "Premium", "1premium", "_x", "pre:mium", "pre mium", strings.Repeat("a", 64), "prémium"} {
		if _, err := MakeTier(name); !errors.Is(err, ErrNotContentKey) {
			t.Errorf("MakeTier(%q) = %v", name, err)
		}
	}
	for _, s := range []string{
		"", ":", "content:", "content:t:post", "content:t:post:", "content:t:post:" + id1 + ":x",
		"content:t:post:" + upper, "content::post:" + id1, "Content:t:post:" + id1, "member:t:channel:" + id1,
		"site:premium", "premium:", "content:t:post;" + id1, "content:" + long + ":post:" + id1,
		"members:t:channel:" + id1 + strings.Repeat("0", MaxKeyBytes),
	} {
		if k, err := ParseKey(s); !errors.Is(err, ErrNotContentKey) {
			t.Errorf("ParseKey(%q) = %v, %v", s, k, err)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("Own panics on an invalid ref")
		}
	}()
	Own(contentref.New("t", "post", "x"))
}

// Every string ParseKey accepts is canonical: it prints back to itself and
// stays within MaxKeyBytes.
func FuzzParseKey(f *testing.F) {
	for _, s := range []string{"premium", "content:t:post:" + id1, "members:onlydemo:channel:" + id1, "content:t:post:", "a:b:c:d", ""} {
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
		if r, ok := k.Ref(); ok && r.Validate() != nil {
			t.Fatalf("%q yields invalid ref %+v", s, r)
		}
	})
}

func TestRuleKeys(t *testing.T) {
	post := contentref.New("t", "post", id1)
	ch := contentref.New("t", "channel", "01920000-0000-7000-8000-000000000002")
	for _, tc := range []struct {
		rule     Rule
		unlocks  []Key
		sell     []Key
		requires *Key
	}{
		{Rule{Level: Public}, []Key{Own(post)}, nil, nil},
		{Rule{Level: TierLevel, Tiers: []string{"premium", "gold"}}, []Key{Own(post), Tier("premium"), Tier("gold")}, []Key{Tier("premium"), Tier("gold")}, nil},
		{Rule{Level: MembersLevel, Scope: ch}, []Key{Own(post), Members(ch)}, []Key{Members(ch)}, nil},
		{Rule{Level: PPV, Scope: ch}, []Key{Own(post)}, []Key{Own(post)}, nil},
		{Rule{Level: MembersPPV, Scope: ch}, []Key{Own(post)}, []Key{Own(post)}, new(Members(ch))},
	} {
		u, err := tc.rule.Unlocks(post)
		if err != nil || !equalKeys(u, tc.unlocks) {
			t.Errorf("%s Unlocks = %v %v", tc.rule.Level, u, err)
		}
		sell, req, err := tc.rule.Sells(post)
		if err != nil || !equalKeys(sell, tc.sell) || (req == nil) != (tc.requires == nil) || (req != nil && *req != *tc.requires) {
			t.Errorf("%s Sells = %v %v %v", tc.rule.Level, sell, req, err)
		}
	}
	for _, bad := range []Rule{{}, {Level: "vip"}, {Level: TierLevel}, {Level: TierLevel, Tiers: []string{"Bad"}},
		{Level: MembersLevel}, {Level: MembersPPV, Scope: contentref.NewVersion("t", "channel", id1, "v")}} {
		if _, err := bad.Unlocks(post); !errors.Is(err, ErrRule) {
			t.Errorf("%+v Unlocks: %v", bad, err)
		}
	}
	if _, err := (Rule{Level: Public}).Unlocks(contentref.NewVersion("t", "post", id1, "v")); !errors.Is(err, ErrNotContentKey) {
		t.Errorf("versioned item: %v", err)
	}
}

func equalKeys(a, b []Key) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
