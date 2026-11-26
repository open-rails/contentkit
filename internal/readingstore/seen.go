package readingstore

import (
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SeenMode says which of a reader's galleries a list leaves out.
type SeenMode int

const (
	// SeenOpened leaves out what they opened (bounced from, read or
	// finished) or hid: "new to you".
	SeenOpened SeenMode = iota
	// SeenFinished leaves out what they finished or hid.
	SeenFinished
)

// ExcludeSeen is a predicate keeping only the rows whose content id, the SQL
// text expression contentID, subject has not seen under mode, with its pgx
// named args (prefixed seen_). Galleries without a reading_state row pass.
func ExcludeSeen(schema string, app, subject uuid.UUID, mode SeenMode, contentID string) (string, pgx.NamedArgs) {
	seen := "seen.last_seen_at IS NOT NULL"
	if mode == SeenFinished {
		seen = "seen.best_outcome = " + strconv.Itoa(int(OutcomeComplete))
	}
	return `NOT EXISTS (SELECT 1 FROM ` + pgx.Identifier{schema, "reading_state"}.Sanitize() + ` seen
 WHERE seen.app_id = @seen_app AND seen.subject = @seen_subject AND seen.content_id = (` + contentID + `)::uuid AND (seen.hidden OR ` + seen + `))`,
		pgx.NamedArgs{"seen_app": app, "seen_subject": subject}
}
