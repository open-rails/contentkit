// Package readerprefs carries guest preferences and the canonical kind/entity
// preference key. Preferences never grant access or weaken domain policy.
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

// Default prevents an old unscoped preference from returning after a reset.
const Default = "default"

type key struct{}

func Key(kind, entity string) string { return kind + "/" + entity }

// Stored reads an explicit scoped choice, then a legacy global choice. A
// scoped default marker intentionally suppresses the legacy fallback.
func Stored(values map[string]string, kind, entity string) (visible, overridden bool) {
	value, ok := values[Key(kind, entity)]
	if !ok {
		value, ok = values[entity]
	}
	switch value {
	case Show:
		return true, ok
	case Hide:
		return false, ok
	default:
		return false, false
	}
}

func WithHeader(ctx context.Context, values []string) context.Context {
	return context.WithValue(ctx, key{}, append([]string(nil), values...))
}

// Overrides applies scoped choices and legacy fallback only to the current
// domain's permitted kind/entity pairs. Unknown and locked pairs are ignored.
func Overrides(ctx context.Context, allowed map[string]map[string]bool) (map[string]string, error) {
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
	type choice struct {
		EntityID string `json:"entity_id"`
		Visible  *bool  `json:"visible"`
		Default  *bool  `json:"default"`
	}
	var input *struct {
		Kinds  map[string][]choice `json:"kinds"`
		Legacy []struct {
			EntityID string `json:"entity_id"`
			Visible  *bool  `json:"visible"`
		} `json:"legacy_filters"`
		Old []struct {
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
	n := len(input.Legacy) + len(input.Old)
	for _, v := range input.Kinds {
		n += len(v)
	}
	if n > 128 {
		return nil, fmt.Errorf("at most 128 guest content filters")
	}
	legacy := map[string]bool{}
	addLegacy := func(id string, visible *bool) error {
		if id == "" || visible == nil {
			return fmt.Errorf("guest content filter must name an entity and visibility")
		}
		if _, ok := legacy[id]; ok {
			return fmt.Errorf("guest content filter is set twice")
		}
		legacy[id] = *visible
		return nil
	}
	for _, f := range input.Legacy {
		if err := addLegacy(f.EntityID, f.Visible); err != nil {
			return nil, err
		}
	}
	for _, f := range input.Old {
		if err := addLegacy(f.GroupID, f.Visible); err != nil {
			return nil, err
		}
	}
	out := map[string]string{}
	for kind, entities := range allowed {
		for id := range entities {
			if v, ok := legacy[id]; ok {
				out[Key(kind, id)] = Hide
				if v {
					out[Key(kind, id)] = Show
				}
			}
		}
	}
	for kind, choices := range input.Kinds {
		if kind == "" {
			return nil, fmt.Errorf("guest content filter must name a content kind")
		}
		seen := map[string]bool{}
		for _, f := range choices {
			if f.EntityID == "" || (f.Visible == nil) == (f.Default == nil) || f.Default != nil && !*f.Default {
				return nil, fmt.Errorf("guest content filter needs either visible or default:true")
			}
			if seen[f.EntityID] {
				return nil, fmt.Errorf("guest content filter is set twice")
			}
			seen[f.EntityID] = true
			if allowed[kind][f.EntityID] {
				value := Default
				if f.Visible != nil {
					value = Hide
					if *f.Visible {
						value = Show
					}
				}
				out[Key(kind, f.EntityID)] = value
			}
		}
	}
	return out, nil
}
