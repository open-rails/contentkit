// Package readerprefs carries a guest reader's local preferences through one
// request. They affect discovery only; they never grant access or persist data.
package readerprefs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const Header = "ContentKit-Reader-Preferences"

type key struct{}

// WithHeader binds the original header values. The route gate calls it only
// for anonymous requests, so account preferences always remain authoritative.
func WithHeader(ctx context.Context, values []string) context.Context {
	return context.WithValue(ctx, key{}, append([]string(nil), values...))
}

// Overrides validates the request's guest choices against this app's configured
// filters. Missing filters keep their entrypoint defaults.
func Overrides(ctx context.Context, allowed map[string]bool) (map[string]bool, error) {
	values, _ := ctx.Value(key{}).([]string)
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) != 1 || len(values[0]) > 8192 {
		return nil, fmt.Errorf("invalid %s header", Header)
	}
	if strings.TrimSpace(values[0]) == "" {
		return nil, nil
	}
	var input *struct {
		Filters []struct {
			GroupID string `json:"group_id"`
			Visible *bool  `json:"visible"`
		} `json:"filters"`
	}
	d := json.NewDecoder(strings.NewReader(values[0]))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil || input == nil {
		return nil, fmt.Errorf("invalid %s JSON", Header)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("invalid %s JSON", Header)
	}
	if len(input.Filters) > 128 {
		return nil, fmt.Errorf("at most 128 guest content filters")
	}
	out := make(map[string]bool, len(input.Filters))
	for _, f := range input.Filters {
		if !allowed[f.GroupID] || f.Visible == nil {
			return nil, fmt.Errorf("guest content filter must name a configured entity and visibility")
		}
		if _, duplicate := out[f.GroupID]; duplicate {
			return nil, fmt.Errorf("guest content filter is set twice")
		}
		out[f.GroupID] = *f.Visible
	}
	return out, nil
}
