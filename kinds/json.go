package kinds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// The Go reference stays typed as EntityRef; files use the same flat fields as
// require_entities, with visibility alongside kind and slug.
type contentFilterJSON struct {
	Kind       string `json:"kind"`
	Slug       string `json:"slug"`
	Visible    bool   `json:"visible"`
	UserToggle *bool  `json:"user_toggle,omitempty"`
}

func (f ContentFilter) MarshalJSON() ([]byte, error) {
	return json.Marshal(contentFilterJSON{f.Entity.Kind, f.Entity.Slug, f.Visible, f.UserToggle})
}

func (f *ContentFilter) UnmarshalJSON(data []byte) error {
	var next contentFilterJSON
	if err := strictJSON(data, &next); err != nil {
		return fmt.Errorf("%w: content filter JSON: %w", ErrInvalid, err)
	}
	*f = ContentFilter{Entity: EntityRef{Kind: next.Kind, Slug: next.Slug}, Visible: next.Visible, UserToggle: next.UserToggle}
	return nil
}

func (c *ContentFilters) UnmarshalJSON(data []byte) error {
	type plain ContentFilters
	var next plain
	if err := strictJSON(data, &next); err != nil {
		return fmt.Errorf("%w: content_filters JSON: %w", ErrInvalid, err)
	}
	policy := ContentFilters(next)
	if err := policy.validateDomains(); err != nil {
		return err
	}
	*c = policy
	return nil
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	var next plain
	if err := strictJSON(data, &next); err != nil {
		return fmt.Errorf("%w: JSON: %w", ErrInvalid, err)
	}
	*c = Config(next)
	return nil
}

func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
