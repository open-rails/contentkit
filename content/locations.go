package content

import (
	"encoding/json"
	"time"
)

// LocationEntry matches one country, subdivision, address or network.
// Exactly one field is set.
type LocationEntry struct {
	Country     string `json:"country,omitempty"`
	Subdivision string `json:"subdivision,omitempty"`
	Network     string `json:"network,omitempty"`
}

type LocationRuleInput struct {
	Locations  []LocationEntry `json:"locations"`
	Target     string          `json:"target"` // site, content, or entities
	ContentIDs []string        `json:"content_ids"`
	EntityIDs  []string        `json:"entity_ids"`
	Domains    []string        `json:"domains"`
	Enabled    bool            `json:"enabled"`
	Note       string          `json:"note"`
}

// LocationRuleUpdate changes only supplied fields.
type LocationRuleUpdate struct {
	Locations  *[]LocationEntry `json:"locations,omitempty"`
	Target     *string          `json:"target,omitempty"`
	ContentIDs *[]string        `json:"content_ids,omitempty"`
	EntityIDs  *[]string        `json:"entity_ids,omitempty"`
	Domains    *[]string        `json:"domains,omitempty"`
	Enabled    *bool            `json:"enabled,omitempty"`
	Note       *string          `json:"note,omitempty"`
}

type LocationRule struct {
	LocationRuleInput
	Targets       []LocationRuleTarget `json:"targets"`
	ID            string               `json:"id"`
	CreatedBy     string               `json:"created_by"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedBy     string               `json:"updated_by"`
	UpdatedByName string               `json:"updated_by_name"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

// LocationRuleTarget names a content item or entity. Missing targets retain
// their ID with an empty kind and title/name.
type LocationRuleTarget struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Name  string `json:"name"`
}

type LocationRulePage struct {
	Items   []LocationRule `json:"items"`
	Total   int            `json:"total"`
	Domains []string       `json:"domains"`
}

type LocationRuleHistory struct {
	ID        int64           `json:"id"`
	RuleID    string          `json:"rule_id"`
	Action    string          `json:"action"`
	Actor     string          `json:"actor"`
	ActorName string          `json:"actor_name"`
	At        time.Time       `json:"at"`
	Snapshot  json.RawMessage `json:"snapshot"`
}

type LocationRuleHistoryPage struct {
	Items []LocationRuleHistory `json:"items"`
	Total int                   `json:"total"`
}

// VisitorLocation is unknown when Country and Subdivision are empty.
type VisitorLocation struct {
	Country         string `json:"country"`
	CountryName     string `json:"country_name"`
	Subdivision     string `json:"subdivision"`
	SubdivisionName string `json:"subdivision_name"`
}

type LocationRuleMatch struct {
	RuleID     string   `json:"rule_id"`
	Kind       string   `json:"kind"` // country, subdivision, or network
	Target     string   `json:"target"`
	ContentIDs []string `json:"content_ids"`
	EntityIDs  []string `json:"entity_ids"`
}

type LocationCheck struct {
	Location VisitorLocation                `json:"location"`
	Domains  map[string][]LocationRuleMatch `json:"domains"`
}
