package kinds

import (
	"fmt"
	"github.com/goccy/go-yaml"
)

// UnmarshalYAML keeps the shared policy strict even inside permissively decoded
// app configuration. Retired per-content-kind domain blocks are rejected.
func (c *Config) UnmarshalYAML(data []byte) error {
	type plain Config
	var next plain
	if err := yaml.UnmarshalWithOptions(data, &next, yaml.Strict()); err != nil {
		return fmt.Errorf("%w: YAML: %w", ErrInvalid, err)
	}
	*c = Config(next)
	return nil
}

// DecodeContentFilters reads an isolated YAML or JSON policy block. Hosts can
// pass their root config's content_filters section directly without copying
// the policy model or weakening its strict unknown-field validation.
func DecodeContentFilters(data []byte) (ContentFilters, error) {
	var policy ContentFilters
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return ContentFilters{}, err
	}
	return policy, nil
}

// UnmarshalYAML remains strict when an app uses a permissive outer decoder.
func (c *ContentFilters) UnmarshalYAML(data []byte) error {
	type plain ContentFilters
	var next plain
	if err := yaml.UnmarshalWithOptions(data, &next, yaml.Strict()); err != nil {
		return fmt.Errorf("%w: content_filters YAML: %w", ErrInvalid, err)
	}
	*c = ContentFilters(next)
	return nil
}
