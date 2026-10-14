package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// idError wraps its sentinel as contentref.IDError does.
type idError struct{ sentinel error }

func (e idError) Error() string { return fmt.Sprintf("%v, got %q", e.sentinel, "x") }
func (e idError) Unwrap() error { return e.sentinel }

// People read the rule a request broke, never a package's error chain.
func TestRule(t *testing.T) {
	invalid := errors.New("groups: invalid input")
	badID := errors.New("contentref: content_id must be a canonical lowercase UUIDv7")
	attrs := errors.New("kinds: invalid config")
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"rule":          {fmt.Errorf("%w: slug %q is taken", invalid, "x"), `slug "x" is taken`},
		"wrapped again": {fmt.Errorf("groups: save: %w", fmt.Errorf("%w: sort %q", invalid, "random")), `sort "random"`},
		"the innermost": {fmt.Errorf("%w: %w", invalid, fmt.Errorf("%w: attribute %q is not declared", attrs, "x")), `attribute "x" is not declared`},
		"two sentinels": {fmt.Errorf("%w: group id: %w", invalid, badID), "group id: content_id must be a canonical lowercase UUIDv7"},
		"a typed error": {fmt.Errorf("%w: group id: %w", invalid, idError{badID}), `group id: content_id must be a canonical lowercase UUIDv7, got "x"`},
		"bare sentinel": {invalid, Message(CodeInvalidRequest)},
		"not after it":  {fmt.Errorf("body: %w", invalid), Message(CodeInvalidRequest)},
	} {
		if got := Rule(c.err, CodeInvalidRequest); got != c.want {
			t.Errorf("%s: Rule(%q) = %q, want %q", name, c.err, got, c.want)
		}
	}
	if Message("no_such_code") != Message(CodeInternal) || Message(CodeNotFound) == "" {
		t.Fatal("Message falls back to internal_error's")
	}
	for _, c := range ErrorCodes() {
		if c.Message == "" || pkgPrefix.MatchString(c.Message) {
			t.Errorf("%s: message %q", c.Code, c.Message)
		}
	}
}

// A body that fails to decode says what is wrong, in JSON terms.
func TestBodyError(t *testing.T) {
	type body struct {
		Title string   `json:"title"`
		Tags  []string `json:"tags"`
		N     *int     `json:"n"`
	}
	decode := func(s string, limit int64) error {
		r := httptest.NewRequest("POST", "/", strings.NewReader(s))
		dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit))
		dec.DisallowUnknownFields()
		var b body
		return dec.Decode(&b)
	}
	for in, want := range map[string]string{
		``:              "the body is empty",
		`{"title": 1}`:  "title must be a string",
		`{"tags": "x"}`: "tags must be a list",
		`{"n": "x"}`:    "n must be a number",
		`{"nope": 1}`:   `unknown field "nope"`,
		`{"title": `:    "the body is not valid JSON",
		`{"title": "` + strings.Repeat("x", 64) + `"}`: "the body is larger than 32 bytes",
	} {
		if got := BodyError(decode(in, 32)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
