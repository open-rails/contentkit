package media

import (
	"encoding/json"
	"fmt"
)

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ParseConfig reads a registry written by Registry.MarshalJSON; unknown
// keys are ignored.
func ParseConfig(b []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("media: registry JSON: %w", err)
	}
	return c, nil
}
