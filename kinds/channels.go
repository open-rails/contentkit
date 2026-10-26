package kinds

import "maps"

// Channels declares authoring channels, independently from descriptive entities.
// Every channel has an AuthKit authoring group; paid reader membership is optional.
type Channels struct {
	Creation    string       `json:"creation" yaml:"creation"`
	Route       string       `json:"route,omitempty" yaml:"route"`
	Entities    []string     `json:"entities,omitempty" yaml:"entities"`
	Entitled    bool         `json:"entitled,omitempty" yaml:"entitled"`
	MaxPerOwner int          `json:"max_per_owner,omitempty" yaml:"max_per_owner"`
	Uploads     UploadLimits `json:"uploads,omitzero" yaml:"uploads"`
}

// ChannelPolicy associates a content kind with channels. Fixed selects and
// provisions one staff channel; Required requires a channel on every new item.
// A nil policy disables channels for the kind.
type ChannelPolicy struct {
	Fixed    string `json:"fixed,omitempty" yaml:"fixed"`
	Required bool   `json:"required,omitempty" yaml:"required"`
}

// StorageModel supplies the private channel storage descriptor to existing
// persistence/media machinery. Public models never expose this synthetic entity.
func (c Config) StorageModel() Config {
	if c.Channels == nil || c.channelStorage {
		return c
	}
	c.EntityKinds = maps.Clone(c.EntityKinds)
	if c.EntityKinds == nil {
		c.EntityKinds = map[string]EntityKind{}
	}
	ch := c.Channels
	owner := "site"
	if ch.Creation == "users" {
		owner = "user"
	}
	c.EntityKinds["channel"] = EntityKind{Route: ch.Route, Owner: owner, AuthScope: true, Entitled: ch.Entitled, Avatar: true, MaxPerOwner: ch.MaxPerOwner, Uploads: ch.Uploads}
	c.channelStorage = true
	return c
}
