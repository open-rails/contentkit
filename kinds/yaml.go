package kinds

import (
	"fmt"
	"slices"

	"github.com/goccy/go-yaml"
)

// Domain YAML places content kinds directly alongside its metadata. Decode
// each policy strictly rather than allowing a typo to silently weaken a rule.
func (d *Domain) UnmarshalYAML(data []byte) error {
	var fields map[string]yaml.RawMessage
	if err := yaml.UnmarshalWithOptions(data, &fields, yaml.Strict()); err != nil {
		return fmt.Errorf("%w: domain YAML: %w", ErrInvalid, err)
	}
	next := Domain{Kinds: make(map[string]KindPolicy)}
	for key, raw := range fields {
		var target any
		var policy KindPolicy
		switch key {
		case "name":
			target = &next.Name
		case "hosts":
			target = &next.Hosts
		default:
			target = &policy
		}
		if err := yaml.UnmarshalWithOptions(raw, target, yaml.Strict()); err != nil {
			return fmt.Errorf("%w: domain field %q: %w", ErrInvalid, key, err)
		}
		if key != "name" && key != "hosts" {
			next.Kinds[key] = policy
		}
	}
	*d = next
	return nil
}

// MarshalYAML mirrors the native flat domain syntax and keeps metadata first.
func (d Domain) MarshalYAML() (any, error) {
	fields := yaml.MapSlice{{Key: "name", Value: d.Name}, {Key: "hosts", Value: d.Hosts}}
	names := make([]string, 0, len(d.Kinds))
	for kind := range d.Kinds {
		if kind == "name" || kind == "hosts" {
			return nil, fmt.Errorf("%w: content kind %q is reserved for domain metadata", ErrInvalid, kind)
		}
		names = append(names, kind)
	}
	slices.Sort(names)
	for _, kind := range names {
		fields = append(fields, yaml.MapItem{Key: kind, Value: d.Kinds[kind]})
	}
	return fields, nil
}

// UnmarshalYAML rejects unknown fields even when the embedding application
// uses a permissive decoder. A retired policy key must not silently disappear.
func (c *Config) UnmarshalYAML(data []byte) error {
	type plain Config
	var next plain
	if err := yaml.UnmarshalWithOptions(data, &next, yaml.Strict()); err != nil {
		return fmt.Errorf("%w: YAML: %w", ErrInvalid, err)
	}
	*c = Config(next)
	return nil
}
