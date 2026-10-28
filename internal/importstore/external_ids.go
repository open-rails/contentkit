package importstore

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	importer "github.com/open-rails/contentkit/importer"
)

// maxExternalIDs caps one list's page.
const maxExternalIDs = 10_000

// gone is, per entity, the table of its records and when one is deleted.
var gone = map[string]struct{ table, join, deleted string }{
	importer.RecordContent: {"content_items", "r.id = x.id", "r.id IS NULL OR r.state = 'deleted'"},
	importer.RecordVersion: {"content_versions", "r.id = x.id", "r.id IS NULL OR r.state = 'deleted'"},
	importer.RecordEntity:  {"content_entities", "r.id = x.id", "r.id IS NULL OR r.state = 'deleted'"},
	importer.RecordChannel: {"content_entities", "r.id = x.id AND r.kind = 'channel'", "r.id IS NULL OR r.state = 'deleted'"},
	importer.RecordComment: {"content_comments", "r.id = x.id::uuid", "r.id IS NULL OR r.deleted_at IS NOT NULL"},
	importer.RecordPoll:    {"content_poll_questions", "r.id = x.id::uuid", "r.id IS NULL OR r.deleted_at IS NOT NULL"},
	importer.RecordOption:  {"content_poll_options", "r.id = x.id::uuid", "r.id IS NULL"},
}

// ExternalIDs pages the external ids imports under q.Source hold, per entity.
func (s *Service) ExternalIDs(ctx context.Context, q importer.ExternalIDsQuery) (importer.ExternalIDs, error) {
	out := importer.ExternalIDs{Lists: []importer.ExternalIDPage{}}
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
			return importer.ExternalIDs{Lists: []importer.ExternalIDPage{}}, err
		}
		out.Lists = append(out.Lists, page)
	}
	return out, nil
}

func (s *Service) externalIDs(ctx context.Context, source string, l importer.ExternalIDList) (importer.ExternalIDPage, error) {
	page := importer.ExternalIDPage{Entity: l.Entity, IDs: []importer.ExternalID{}}
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
	page.IDs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (importer.ExternalID, error) {
		var x importer.ExternalID
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
