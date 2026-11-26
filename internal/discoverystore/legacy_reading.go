package discoverystore

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/readingstore"
	"github.com/open-rails/contentkit/internal/subject"
)

// ImportLegacyReading merges a legacy source's reading into accounts'
// reading state in the import's transaction, as the analytics job merges a
// day: by maximum and latest, so a re-sent batch converges. It is called
// only by the authorized bulk importer.
func (s *Service) ImportLegacyReading(ctx context.Context, tx pgx.Tx, in []discovery.ImportLegacyReading) error {
	if len(in) == 0 {
		return nil
	}
	ids := make([]string, len(in))
	for i, row := range in {
		if err := contentref.ValidateID(row.ContentID); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		ids[i] = row.ContentID
	}
	kinds, err := s.kindsOf(ctx, ids)
	if err != nil {
		return err
	}
	var ups []readingstore.StateUpdate
	var removedSubjects, removedContents []uuid.UUID
	for _, row := range in {
		if _, ok := kinds[row.ContentID]; !ok {
			return ErrNotFound
		}
		who, ok := subject.Account(row.AccountID)
		if !ok {
			return fmt.Errorf("%w: legacy reading belongs to an account id", ErrInvalid)
		}
		content := uuid.MustParse(row.ContentID)
		if row.Removed {
			removedSubjects, removedContents = append(removedSubjects, who), append(removedContents, content)
			continue
		}
		first := row.LastReadAt
		if row.FirstReadAt != nil {
			first = *row.FirstReadAt
		}
		outcome := readingstore.OutcomeNeutral
		if row.Completed || row.ProgressMax > 0 && row.Progress >= row.ProgressMax {
			outcome = readingstore.OutcomeComplete
		}
		position := row.Progress
		ups = append(ups, readingstore.StateUpdate{Subject: who, ContentID: content, Day: first, LastSeen: row.LastReadAt,
			Outcome: outcome, Furthest: row.Progress, Position: &position})
	}
	if err := s.o.Reading.MergeTx(ctx, tx, ups); err != nil {
		return err
	}
	return s.o.Reading.ForgetPairsTx(ctx, tx, removedSubjects, removedContents)
}
