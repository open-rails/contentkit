package signalstore

import (
	signal "github.com/open-rails/contentkit/signal"
)

func checkLen(field, value string, limit int) error {
	if len(value) > limit {
		return &signal.LimitError{Field: field, Limit: limit, Got: len(value)}
	}
	return nil
}

func checkIdentifiers(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if err := checkLen(pairs[i], pairs[i+1], signal.MaxIdentifierBytes); err != nil {
			return err
		}
	}
	return nil
}
