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

// Overrides validates guest choices and applies only this domain's allowed
// filters. Choices from other domains and obsolete or locked filters are
// ignored; missing filters retain this domain's defaults.
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
	seen := map[string]bool{}
	for _, f := range input.Filters {
		if f.GroupID == "" || f.Visible == nil {
			return nil, fmt.Errorf("guest content filter must name an entity and visibility")
		}
		if seen[f.GroupID] {
			return nil, fmt.Errorf("guest content filter is set twice")
		}
		seen[f.GroupID] = true
		if allowed[f.GroupID] {
			out[f.GroupID] = *f.Visible
		}
	}
	return out, nil
}
