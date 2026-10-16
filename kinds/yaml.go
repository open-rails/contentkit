package kinds

import (
	"fmt"

	"github.com/goccy/go-yaml"
)

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
