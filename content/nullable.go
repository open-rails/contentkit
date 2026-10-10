package content

import "encoding/json"

// Nullable is a PATCH member that tells absent (leave it) from null (clear
// it): Set reports the member was present, Value is nil for null. Tag it
// omitzero, so an unset one is left out when encoded.
type Nullable[T any] struct {
	Set   bool
	Value *T
}

func (n Nullable[T]) IsZero() bool { return !n.Set }

func (n Nullable[T]) MarshalJSON() ([]byte, error) {
	if n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*n.Value)
}

func (n *Nullable[T]) UnmarshalJSON(b []byte) error {
	n.Set, n.Value = true, nil
	if string(b) == "null" {
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}
