package token_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media/token"
)

var (
	k1 = token.Key{ID: "k1", Secret: bytes.Repeat([]byte{1}, 32)}
	k2 = token.Key{ID: "k2", Secret: bytes.Repeat([]byte{2}, 32)}
)

func ring(t *testing.T, cur token.Key, prev *token.Key) token.Ring {
	t.Helper()
	r, err := token.NewRing(cur, prev)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const (
	item = "d/gallery/123"
	page = item + "/private/sha256-3a"
)

func TestVerifyPrivate(t *testing.T) {
	r := ring(t, k2, &k1)
	now := time.Unix(1_800_000_000, 0)
	exp := token.Expiry(now, time.Hour, 0)
	const item = "doujins/gallery/456/private/"
	a, b, other := item+"sha256-3a", item+"sha256-7d", "doujins/gallery/789/private/sha256-3a"
	whole := r.Sign(token.ItemScope("doujins", "gallery", "456"), exp)
	for _, c := range []struct {
		tok, key string
		want     error
	}{
		{whole, a, nil},
		{whole, b, nil},
		{ring(t, k1, nil).Sign(token.ItemScope("doujins", "gallery", "456"), exp), b, nil},
		{whole, other, token.ErrInvalid},
		{r.Sign(a, exp), a, token.ErrInvalid},                 // a file's own key: no such scope
		{r.Sign(a+"#dl=Title.zip", exp), a, token.ErrInvalid}, // nor a download's
		{r.Sign(item, exp), a, token.ErrInvalid},              // the old folder scope
		{whole, "doujins/gallery/456/public/cover-460.webp", token.ErrInvalid},
		{whole, "doujins/gallery/456/private/", token.ErrInvalid},
		{whole, "doujins/gallery/456/private/a/b", token.ErrInvalid},
		{whole, "doujins/gallery/456", token.ErrInvalid},
		{r.Sign(token.ItemScope("doujins", "gallery", "456"), now), a, token.ErrExpired},
		{ring(t, token.Key{ID: "k3", Secret: k1.Secret}, nil).Sign(token.ItemScope("doujins", "gallery", "456"), exp), a, token.ErrUnknownKey},
		{token.Ring{}.Sign(token.ItemScope("doujins", "gallery", "456"), exp), a, token.ErrMalformed}, // a zero Ring signs with an empty key id
	} {
		if err := r.VerifyPrivate(c.tok, c.key, now); !errors.Is(err, c.want) {
			t.Errorf("VerifyPrivate(%.12s…, %s) = %v, want %v", c.tok, c.key, err, c.want)
		}
	}
	if err := (token.Ring{}).VerifyPrivate(token.Ring{}.Sign(token.ItemScope("doujins", "gallery", "456"), exp), a, now); !errors.Is(err, token.ErrMalformed) {
		t.Fatalf("a zero Ring verified its own token: %v", err)
	}
}

func TestExpiryWindowsAndRotation(t *testing.T) {
	window := 4 * time.Hour
	base := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC) // on a 4 h boundary
	for _, c := range []struct {
		now  time.Time
		want time.Time
	}{
		{base, base.Add(4 * time.Hour)},
		{base.Add(-time.Second), base.Add(4 * time.Hour)},
		{base.Add(time.Second), base.Add(8 * time.Hour)},
		{base.Add(3*time.Hour + 59*time.Minute), base.Add(8 * time.Hour)},
	} {
		if got := token.Expiry(c.now, 4*time.Hour, window); !got.Equal(c.want) {
			t.Fatalf("Expiry(%s) = %s, want %s", c.now, got, c.want)
		}
	}
	r1 := ring(t, k1, nil)
	a := r1.Sign(item, token.Expiry(base.Add(time.Minute), time.Hour, window))
	b := r1.Sign(item, token.Expiry(base.Add(2*time.Hour), time.Hour, window))
	if a != b {
		t.Fatal("tokens within one window must be identical")
	}
	exp := token.Expiry(base, time.Hour, window)
	if err := r1.VerifyPrivate(a, page, exp.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r1.VerifyPrivate(a, page, exp); !errors.Is(err, token.ErrExpired) {
		t.Fatalf("at exp: %v", err)
	}

	rotated := ring(t, k2, &k1)
	if err := rotated.VerifyPrivate(a, page, base); err != nil {
		t.Fatalf("previous key refused after rotation: %v", err)
	}
	if !strings.HasPrefix(rotated.Sign(item, exp), "k2.") {
		t.Fatal("rotation must sign with the new key")
	}
	if err := ring(t, k2, nil).VerifyPrivate(a, page, base); !errors.Is(err, token.ErrUnknownKey) {
		t.Fatalf("retired key: %v", err)
	}
	forged := token.Key{ID: "k1", Secret: bytes.Repeat([]byte{9}, 32)}
	if err := r1.VerifyPrivate(ring(t, forged, nil).Sign(item, exp), page, base); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("forged secret: %v", err)
	}
	parts := strings.Split(a, ".")
	for _, bad := range []string{"", "k1", "k1.123", "k1..sig", "k1.0123." + parts[2], "k1.x." + parts[2], "k1." + parts[1] + ".!!"} {
		if err := r1.VerifyPrivate(bad, page, base); !errors.Is(err, token.ErrMalformed) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	later := parts[0] + "." + parts[1] + "0." + parts[2]
	if err := r1.VerifyPrivate(later, page, base); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("extended expiry accepted: %v", err)
	}
	for _, bad := range []struct {
		cur  token.Key
		prev *token.Key
	}{
		{token.Key{ID: "a.b", Secret: k1.Secret}, nil},
		{token.Key{ID: "k", Secret: []byte("short")}, nil},
		{k1, &token.Key{ID: "k1", Secret: k2.Secret}},
	} {
		if _, err := token.NewRing(bad.cur, bad.prev); err == nil {
			t.Fatalf("ring %+v accepted", bad)
		}
	}
}

func TestExpiryFractionalBoundary(t *testing.T) {
	base := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	now := base.Add(500 * time.Millisecond)
	want := base.Add(8 * time.Hour)
	if got := token.Expiry(now, 4*time.Hour, 4*time.Hour); !got.Equal(want) {
		t.Fatalf("Expiry(%s) = %s, want %s", now, got, want)
	}
}

func TestExpiryFractionalWindows(t *testing.T) {
	base := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		now    time.Time
		window time.Duration
		want   time.Time
	}{
		{name: "subsecond", now: base.Add(100 * time.Millisecond), window: 500 * time.Millisecond, want: base.Add(time.Second)},
		{name: "fractional second", now: base.Add(1100 * time.Millisecond), window: 1500 * time.Millisecond, want: base.Add(2 * time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := token.Expiry(tc.now, 200*time.Millisecond, tc.window); !got.Equal(tc.want) {
				t.Fatalf("Expiry(%s) = %s, want %s", tc.now, got, tc.want)
			}
		})
	}
}

func TestParseRing(t *testing.T) {
	std := "k2:" + base64.StdEncoding.EncodeToString(k2.Secret)
	url := "k1:" + base64.RawURLEncoding.EncodeToString(k1.Secret)
	r, err := token.ParseRing(std, url)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	exp := token.Expiry(now, time.Hour, 0)
	if err := r.VerifyPrivate(ring(t, k1, nil).Sign(item, exp), page, now); err != nil {
		t.Fatalf("previous key from ParseRing: %v", err)
	}
	if err := r.VerifyPrivate(r.Sign(item, exp), page, now); err != nil || !strings.HasPrefix(r.Sign(item, exp), "k2.") {
		t.Fatalf("current key from ParseRing: %v", err)
	}
	for _, bad := range [][2]string{{"k1", ""}, {"k1:not base64!", ""}, {"k1:" + base64.StdEncoding.EncodeToString([]byte("short")), ""}, {std, std}} {
		if _, err := token.ParseRing(bad[0], bad[1]); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
