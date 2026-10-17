// Package readerprefs carries shared app-scoped entity preferences. Preferences
// never grant access or weaken a domain's required, fixed or internal rules.
package readerprefs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const Header = "ContentKit-Reader-Preferences"
const Show = "show"
const Hide = "hide"

// Default follows the current domain while suppressing every legacy choice.
const Default = "default"

// A colon cannot occur in the legacy <kind>/<UUID> key grammar, including
// when an application has a content kind literally named "entity".
const sharedPrefix = "entity:"

type key struct{}

func Key(entity string) string { return sharedPrefix + entity }

// Stored reads a canonical shared choice first. For old per-kind data, hiding
// wins conflicting explicit choices; scoped defaults suppress the older global
// fallback. Reading never rewrites or discards the original account document.
func Stored(values map[string]string, entity string) (visible, overridden bool) {
	value, exists := values[Key(entity)]
	if !exists {
		rank := 0
		for k, v := range values {
			kind, id, scoped := strings.Cut(k, "/")
			if !scoped || kind == "" || id != entity {
				continue
			}
			n := 0
			switch v {
			case Default:
				n = 1
			case Show:
				n = 2
			case Hide:
				n = 3
			}
			if n > rank {
				rank = n
				value = v
			}
		}
		if rank == 0 {
			value = values[entity]
		}
	}
	switch value {
	case Show:
		return true, true
	case Hide:
		return false, true
	default:
		return false, false
	}
}

func WithHeader(ctx context.Context, values []string) context.Context {
	return context.WithValue(ctx, key{}, append([]string(nil), values...))
}

// Overrides accepts only the current domain's public controls. Unknown or
// fixed entities are ignored, and account readers never consult this header.
func Overrides(ctx context.Context, allowed map[string]bool) (map[string]string, error) {
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
			EntityID string `json:"entity_id"`
			Visible  *bool  `json:"visible"`
			Default  *bool  `json:"default"`
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
	out := map[string]string{}
	seen := map[string]bool{}
	for _, f := range input.Filters {
		if f.EntityID == "" || (f.Visible == nil) == (f.Default == nil) || f.Default != nil && !*f.Default {
			return nil, fmt.Errorf("guest content filter needs visible or default:true")
		}
		if seen[f.EntityID] {
			return nil, fmt.Errorf("guest content filter is set twice")
		}
		seen[f.EntityID] = true
		if !allowed[f.EntityID] {
			continue
		}
		value := Default
		if f.Visible != nil {
			value = Hide
			if *f.Visible {
				value = Show
			}
		}
		out[Key(f.EntityID)] = value
	}
	return out, nil
}
