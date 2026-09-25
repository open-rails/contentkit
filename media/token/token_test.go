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
	folder = "d/gallery/123/blobs/"
	page   = folder + "sha256-3a"
)

func TestFolderAndFileScopes(t *testing.T) {
	r := ring(t, k1, nil)
	now := time.Unix(1_800_000_000, 0)
	exp := token.Expiry(now, time.Hour, 0)

	full := r.Sign(folder, exp)
	for _, key := range []string{page, folder + "sha256-7d", folder + "u-3b1f"} {
		if err := r.Verify(full, key, "", now); err != nil {
			t.Fatalf("folder token on %s: %v", key, err)
		}
	}
	for _, key := range []string{
		"d/gallery/123/originals/sha256-3a", "d/gallery/123/manifest.json", "d/gallery/123/manifests/v1.json",
		"d/gallery/124/blobs/sha256-3a", folder + "nested/x", folder, "d/gallery/123/public/cover.webp",
	} {
		if err := r.Verify(full, key, "", now); !errors.Is(err, token.ErrInvalid) {
			t.Fatalf("folder token opened %s: %v", key, err)
		}
	}

	one := r.Sign(token.FileScope(page), exp)
	if err := r.Verify(one, page, "", now); err != nil {
		t.Fatal(err)
	}
	if err := r.Verify(one, folder+"sha256-7d", "", now); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("file token opened a sibling: %v", err)
	}
}

// Editor tokens open temp/ only through VerifyEditor; viewer tokens never do.
func TestEditorScope(t *testing.T) {
	r := ring(t, k1, nil)
	now := time.Unix(1_800_000_000, 0)
	exp := token.Expiry(now, time.Hour, 0)
	const temp = "d/gallery/123/temp/"
	view := temp + "e-3a"

	editor := r.Sign(token.EditorScope(temp), exp)
	if err := r.VerifyEditor(editor, view, now); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"d/gallery/124/temp/e-3a", temp + "nested/e-3a", temp, "d/gallery/123/private/sha256-3a"} {
		if err := r.VerifyEditor(editor, key, now); !errors.Is(err, token.ErrInvalid) {
			t.Fatalf("editor token opened %s: %v", key, err)
		}
	}
	if err := r.Verify(editor, view, "", now); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("Verify accepted an editor token: %v", err)
	}
	for _, viewer := range []string{r.Sign(temp, exp), r.Sign(token.FileScope(view), exp), r.Sign("d/gallery/123/private/", exp),
		r.Sign(token.DownloadScope(view, "editor"), exp)} {
		if err := r.VerifyEditor(viewer, view, now); !errors.Is(err, token.ErrInvalid) {
			t.Fatalf("VerifyEditor accepted a viewer token: %v", err)
		}
	}
	if err := r.VerifyEditor(editor, view, exp); !errors.Is(err, token.ErrExpired) {
		t.Fatalf("expired editor token: %v", err)
	}
}

func TestDownloadNameBinding(t *testing.T) {
	r := ring(t, k1, nil)
	now := time.Unix(1_800_000_000, 0)
	exp := token.Expiry(now, time.Hour, 0)
	zip := folder + "sha256-e0"
	name := "[Artist] Title (English).zip"
	dl := r.Sign(token.DownloadScope(zip, name), exp)
	if err := r.Verify(dl, zip, name, now); err != nil {
		t.Fatal(err)
	}
	for _, altered := range []string{"[Artist] Title (English) .zip", "evil.exe", ""} {
		if err := r.Verify(dl, zip, altered, now); !errors.Is(err, token.ErrInvalid) {
			t.Fatalf("dl %q: %v", altered, err)
		}
	}
	// A folder or file token never authorizes a chosen download name.
	if err := r.Verify(r.Sign(folder, exp), zip, name, now); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("folder token with dl: %v", err)
	}
	if got := token.Attachment("[A] 日本語 \"x\".zip"); got != `attachment; filename="[A] ___ _x_.zip"; filename*=UTF-8''%5BA%5D%20%E6%97%A5%E6%9C%AC%E8%AA%9E%20%22x%22.zip` {
		t.Fatal(got)
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
	a := r1.Sign(folder, token.Expiry(base.Add(time.Minute), time.Hour, window))
	b := r1.Sign(folder, token.Expiry(base.Add(2*time.Hour), time.Hour, window))
	if a != b {
		t.Fatal("tokens within one window must be identical")
	}
	exp := token.Expiry(base, time.Hour, window)
	if err := r1.Verify(a, page, "", exp.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r1.Verify(a, page, "", exp); !errors.Is(err, token.ErrExpired) {
		t.Fatalf("at exp: %v", err)
	}

	rotated := ring(t, k2, &k1)
	if err := rotated.Verify(a, page, "", base); err != nil {
		t.Fatalf("previous key refused after rotation: %v", err)
	}
	if !strings.HasPrefix(rotated.Sign(folder, exp), "k2.") {
		t.Fatal("rotation must sign with the new key")
	}
	if err := ring(t, k2, nil).Verify(a, page, "", base); !errors.Is(err, token.ErrUnknownKey) {
		t.Fatalf("retired key: %v", err)
	}
	forged := token.Key{ID: "k1", Secret: bytes.Repeat([]byte{9}, 32)}
	if err := r1.Verify(ring(t, forged, nil).Sign(folder, exp), page, "", base); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("forged secret: %v", err)
	}
	parts := strings.Split(a, ".")
	for _, bad := range []string{"", "k1", "k1.123", "k1..sig", "k1.0123." + parts[2], "k1.x." + parts[2], "k1." + parts[1] + ".!!"} {
		if err := r1.Verify(bad, page, "", base); !errors.Is(err, token.ErrMalformed) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	later := parts[0] + "." + parts[1] + "0." + parts[2]
	if err := r1.Verify(later, page, "", base); !errors.Is(err, token.ErrInvalid) {
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

func TestParseRing(t *testing.T) {
	std := "k2:" + base64.StdEncoding.EncodeToString(k2.Secret)
	url := "k1:" + base64.RawURLEncoding.EncodeToString(k1.Secret)
	r, err := token.ParseRing(std, url)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	exp := token.Expiry(now, time.Hour, 0)
	if err := r.Verify(ring(t, k1, nil).Sign(page, exp), page, "", now); err != nil {
		t.Fatalf("previous key from ParseRing: %v", err)
	}
	if err := r.Verify(r.Sign(page, exp), page, "", now); err != nil || !strings.HasPrefix(r.Sign(page, exp), "k2.") {
		t.Fatalf("current key from ParseRing: %v", err)
	}
	for _, bad := range [][2]string{{"k1", ""}, {"k1:not base64!", ""}, {"k1:" + base64.StdEncoding.EncodeToString([]byte("short")), ""}, {std, std}} {
		if _, err := token.ParseRing(bad[0], bad[1]); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
