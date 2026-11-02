package token

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testRing(t *testing.T) Ring {
	t.Helper()
	ring, err := NewRing(Key{ID: "k1", Secret: []byte(strings.Repeat("s", 32))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func TestVerifyScopeUntilReturnsExpiry(t *testing.T) {
	ring := testRing(t)
	now := time.Unix(1_800_000_000, 0)
	exp := now.Add(time.Hour)
	tok := ring.Sign("scope", exp)

	got, err := ring.VerifyScopeUntil(tok, "scope", now)
	if err != nil || !got.Equal(exp) {
		t.Fatalf("VerifyScopeUntil = %v, %v; want %v", got, err, exp)
	}
	if err := ring.VerifyScope(tok, "scope", now); err != nil {
		t.Fatalf("VerifyScope = %v", err)
	}
}

func TestVerifyScopeUntilRefusesWithoutAnExpiry(t *testing.T) {
	ring := testRing(t)
	now := time.Unix(1_800_000_000, 0)
	tok := ring.Sign("scope", now.Add(time.Hour))

	for name, c := range map[string]struct {
		tok, scope string
		at         time.Time
		want       error
	}{
		"wrong scope": {tok, "other", now, ErrInvalid},
		"expired":     {tok, "scope", now.Add(2 * time.Hour), ErrExpired},
		"malformed":   {"nope", "scope", now, ErrMalformed},
	} {
		got, err := ring.VerifyScopeUntil(c.tok, c.scope, c.at)
		if !errors.Is(err, c.want) || !got.IsZero() {
			t.Errorf("%s: = %v, %v; want zero time and %v", name, got, err, c.want)
		}
	}
}
