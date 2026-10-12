package server

import (
	"fmt"

	"github.com/goccy/go-yaml"

	contentkit "github.com/open-rails/contentkit"
)

// Secrets are read from the environment, never the config file: each
// variable, when set, fills its field.
var Secrets = []struct {
	Env  string
	Fill func(*Config, string)
}{
	{"CONTENTKIT_MEDIA_TOKEN_KEY", func(c *Config, v string) { media(c).Delivery.SigningKey = v }},
	{"CONTENTKIT_MEDIA_TOKEN_PREVIOUS_KEY", func(c *Config, v string) { media(c).Delivery.PreviousKey = v }},
	{"CONTENTKIT_S3_ACCESS_KEY_ID", func(c *Config, v string) { c.S3.AccessKeyID = v }},
	{"CONTENTKIT_S3_SECRET_ACCESS_KEY", func(c *Config, v string) { c.S3.SecretAccessKey = v }},
	{"CONTENTKIT_CLICKHOUSE_PASSWORD", func(c *Config, v string) { c.Engine.ClickHouse.Password = v }},
	{"CONTENTKIT_OPENRAILS_API_KEY", func(c *Config, v string) { c.OpenRails.APIKey = v }},
}

func media(c *Config) *contentkit.MediaConfig {
	if c.Engine.Media == nil {
		return &contentkit.MediaConfig{} // no media: the secret has nowhere to go
	}
	return c.Engine.Media
}

// Load reads a YAML config file, refusing unknown fields, then fills the
// secrets from getenv.
func Load(raw []byte, getenv func(string) string) (Config, error) {
	var c Config
	if err := yaml.UnmarshalWithOptions(raw, &c, yaml.Strict()); err != nil {
		return Config{}, fmt.Errorf("server: config: %w", err)
	}
	for _, s := range Secrets {
		if v := getenv(s.Env); v != "" {
			s.Fill(&c, v)
		}
	}
	return c, nil
}
