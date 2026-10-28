package discoverystore

import (
	discovery "github.com/open-rails/contentkit/discovery"
)

var _ discovery.Candidates = discovery.Fallback{}

func reportFallback(f discovery.Fallback, err error) {
	if f.OnError != nil {
		f.OnError(err)
	}
}
