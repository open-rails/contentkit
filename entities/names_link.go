package entities

// NewEntity is a name a content write links, creating the entity when no
// active entity of its kind carries that name or alias in any language.
type NewEntity struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Language of a created entity's canonical name; the catalog's first by default.
	Language string `json:"language,omitempty"`
}

// NameSeparators are ignored when names match. Removing all punctuation would
// conflate distinct names such as C/C++ or AC/DC and ACDC.
const NameSeparators = `[[:space:]_‐‑‒–—―-]+`
