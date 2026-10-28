package channels

import (
	"time"

	"github.com/open-rails/contentkit/entities"
)

// Import is a channel's explicit imported identity. Source media, public codes
// and authorization group IDs remain stable. Omitted names/text preserve them.
type Import struct {
	ID          string
	Slug        string
	State       entities.State
	Cover       *entities.Cover
	Avatar      *entities.Avatar
	OwnerUserID string
	AuthScope   string
	Names       []entities.Name
	Text        []entities.Text
	Code        string
	CreatedAt   *time.Time
	UpdatedAt   *time.Time
}
