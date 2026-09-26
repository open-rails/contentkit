package taxonomy

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		invalid bool
	}{
		{name: "single value", body: `{"slug":"x"}`},
		{name: "trailing whitespace", body: " {\"slug\":\"x\"} \n\t"},
		{name: "second value", body: `{"slug":"x"}{}`, invalid: true},
		{name: "trailing garbage", body: `{"slug":"x"}x`, invalid: true},
		{name: "oversized suffix", body: `{"slug":"x"}` + strings.Repeat(" ", maxBody), invalid: true},
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
