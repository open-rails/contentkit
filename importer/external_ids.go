package importer

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// ExternalIDsQuery asks which external ids imports under Source hold, one
// cursor-paged list per entity: what a persistent sync compares with its
// source to find the deletes to send.
type ExternalIDsQuery struct {
	Source string           `json:"source"`
	Lists  []ExternalIDList `json:"lists"`
}

// ExternalIDList asks one page of an entity's external ids.
type ExternalIDList struct {
	// Entity is content, version, entity, comment, poll or poll_option; each
	// at most once per query.
	Entity string `json:"entity"`
	// Cursor is the previous page's next_cursor.
	Cursor string `json:"cursor,omitempty"`
	// Limit defaults to 1000 and is capped at 10,000.
	Limit int `json:"limit,omitempty"`
}

// ExternalIDs answers an ExternalIDsQuery: a page per list, in order.
type ExternalIDs struct {
	Lists []ExternalIDPage `json:"lists"`
}

// ExternalIDPage is one entity's page, in external id order.
type ExternalIDPage struct {
	Entity string       `json:"entity"`
	IDs    []ExternalID `json:"ids"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// ExternalID is one imported record and its id. Deleted: ContentKit holds it
// deleted or no longer holds it, so it needs no delete record.
type ExternalID struct {
	ExternalID string `json:"external_id"`
	ID         string `json:"id"`
	Deleted    bool   `json:"deleted"`
}

// maxExternalIDs caps one list's page.
const maxExternalIDs = 10_000

// gone is, per entity, the table of its records and when one is deleted.
var gone = map[string]struct{ table, join, deleted string }{
	RecordContent: {"content_items", "r.id = x.id", "r.id IS NULL OR r.state = 'deleted'"},
	RecordVersion: {"content_versions", "r.id = x.id", "r.id IS NULL OR r.state = 'deleted'"},
	RecordEntity:  {"content_entities", "r.id = x.id", "r.id IS NULL OR r.state = 'deleted'"},
	RecordChannel: {"content_entities", "r.id = x.id AND r.kind = 'channel'", "r.id IS NULL OR r.state = 'deleted'"},
	RecordComment: {"content_comments", "r.id = x.id::uuid", "r.id IS NULL OR r.deleted_at IS NOT NULL"},
	RecordPoll:    {"content_poll_questions", "r.id = x.id::uuid", "r.id IS NULL OR r.deleted_at IS NOT NULL"},
	RecordOption:  {"content_poll_options", "r.id = x.id::uuid", "r.id IS NULL"},
}

// ExternalIDs pages the external ids imports under q.Source hold, per entity.
func (s *Service) ExternalIDs(ctx context.Context, q ExternalIDsQuery) (ExternalIDs, error) {
	out := ExternalIDs{Lists: []ExternalIDPage{}}
	if !sourceRE.MatchString(q.Source) {
		return out, fmt.Errorf("%w: source %q must match %s", ErrInvalid, q.Source, sourceRE)
	}
	if len(q.Lists) == 0 || len(q.Lists) > len(gone) {
		return out, fmt.Errorf("%w: ask 1 to %d lists", ErrInvalid, len(gone))
	}
	var asked []string
	for _, l := range q.Lists {
		if _, ok := gone[l.Entity]; !ok || slices.Contains(asked, l.Entity) {
			return out, fmt.Errorf("%w: entity %q is unknown or asked twice", ErrInvalid, l.Entity)
		}
		if l.Limit < 0 || l.Limit > maxExternalIDs {
			return out, fmt.Errorf("%w: limit is 1 to %d", ErrInvalid, maxExternalIDs)
		}
		asked = append(asked, l.Entity)
	}
	for _, l := range q.Lists {
		page, err := s.externalIDs(ctx, q.Source, l)
		if err != nil {
			return ExternalIDs{Lists: []ExternalIDPage{}}, err
		}
		out.Lists = append(out.Lists, page)
	}
	return out, nil
}

func (s *Service) externalIDs(ctx context.Context, source string, l ExternalIDList) (ExternalIDPage, error) {
	page := ExternalIDPage{Entity: l.Entity, IDs: []ExternalID{}}
	after, err := base64.RawURLEncoding.DecodeString(l.Cursor)
	if err != nil {
		return page, fmt.Errorf("%w: cursor", ErrInvalid)
	}
	limit := l.Limit
	if limit == 0 {
		limit = 1000
	}
	g := gone[l.Entity]
	rows, err := s.o.Pool.Query(ctx, `SELECT x.external_id, x.id, `+g.deleted+` FROM `+s.ids+` x
LEFT JOIN `+pgx.Identifier{s.o.Schema, g.table}.Sanitize()+` r ON r.tenant_id = x.tenant_id AND `+g.join+`
WHERE x.tenant_id = $1 AND x.source = $2 AND x.entity = $3 AND x.external_id > $4 ORDER BY x.external_id LIMIT $5`,
		s.o.Tenant, source, l.Entity, string(after), limit+1)
	if err != nil {
		return page, err
	}
	page.IDs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ExternalID, error) {
		var x ExternalID
		return x, r.Scan(&x.ExternalID, &x.ID, &x.Deleted)
	})
	if err != nil {
		return page, err
	}
	if len(page.IDs) > limit {
		page.IDs = page.IDs[:limit]
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.IDs[limit-1].ExternalID))
	}
	return page, nil
}
