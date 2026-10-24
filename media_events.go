package contentkit

import (
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/helpers/eventlog"
)

func (e *Engine) mediaEvents(ref contentref.ContentRef) (eventlog.Reader, error) {
	if e.media.jobs.Manifests().Registry().IsAccount(ref) {
		return e.AccountMediaEvents()
	}
	return e.catalog.EventReader(), nil
}
