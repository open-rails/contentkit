package taxonomy

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	const value = `{"slug":"x"}`
	tests := []struct {
		name    string
		body    string
		invalid bool
	}{
		{name: "single value", body: value},
		{name: "trailing whitespace", body: " {\"slug\":\"x\"} \n\t"},
		{name: "second value", body: `{"slug":"x"}{}`, invalid: true},
		{name: "trailing garbage", body: `{"slug":"x"}x`, invalid: true},
		{name: "exact body limit", body: value + strings.Repeat(" ", maxBody-len(value))},
		{name: "over body limit", body: value + strings.Repeat(" ", maxBody-len(value)+1), invalid: true},
		{name: "unknown field", body: `{"slug":"x","unknown":1}`, invalid: true},
		{name: "empty body", body: "", invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/nodes", strings.NewReader(tt.body))
			var input struct {
				Slug string `json:"slug"`
			}
			err := decode(r, &input)
			if tt.invalid {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("decode() error = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil || input.Slug != "x" {
				t.Fatalf("decode() = %+v, %v; want slug x", input, err)
			}
		})
	}
}
