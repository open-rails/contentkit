package content

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
)

// querier is the subset of pgx shared by *pgxpool.Pool and pgx.Tx, so store
// helpers run either standalone or inside a caller's transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// store holds the shared pool, the tenant and the pre-qualified, schema-scoped
// table names. Every query lands in the host schema given at construction.
type store struct {
	pool   *pgxpool.Pool
	schema string
	tenant string
	t      tables
	// revisionSeq is the schema-qualified preference revision sequence.
	revisionSeq string
}

// tables are the fully-qualified ("schema"."name") identifiers, sanitized once.
type tables struct {
	reactions     string
	comments      string
	pollQuestions string
	pollOptions   string
	pollVotes     string
	pollAnswers   string
	posts         string
	favorites     string
	counts        string

	preferenceSnapshots string
	preferenceArchive   string
}

func newStore(pool *pgxpool.Pool, schema, tenant string) *store {
	q := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	return &store{
		pool:   pool,
		schema: schema,
		tenant: tenant,
		t: tables{
			reactions:     q("social_reactions"),
			comments:      q("social_comments"),
			pollQuestions: q("social_poll_questions"),
			pollOptions:   q("social_poll_options"),
			pollVotes:     q("social_poll_votes"),
			pollAnswers:   q("social_poll_answers"),
			posts:         q("social_posts"),
			favorites:     q("social_favorites"),
			counts:        q("social_entity_counts"),

			preferenceSnapshots: q("content_preference_snapshots"),
			preferenceArchive:   q("content_preference_key_archive"),
		},
		revisionSeq: revisionSeqName(schema),
	}
}

// keyCols is the content key of every keyed social table.
const keyCols = "tenant_id, content_kind, content_id, content_version_id"

// keyPred renders the key predicate with placeholders $n..$n+3 (see keyArgs).
func keyPred(n int) string {
	return "tenant_id = $" + strconv.Itoa(n) + " AND content_kind = $" + strconv.Itoa(n+1) +
		" AND content_id = $" + strconv.Itoa(n+2) + " AND content_version_id = $" + strconv.Itoa(n+3)
}

// keyArgs are the bound values of keyPred, in order.
func keyArgs(k contentref.ContentKey) []any {
	return []any{k.TenantID, k.ContentKind, k.ContentID, k.ContentVersionID}
}

// refColumns unzips references into positional arrays for unnest pairing.
func refColumns(refs []contentref.ContentRef) (kinds, ids, versions []string) {
	kinds, ids, versions = make([]string, len(refs)), make([]string, len(refs)), make([]string, len(refs))
	for i, r := range refs {
		kinds[i], ids[i], versions[i] = r.ContentKind, r.ContentID, r.Version()
	}
	return kinds, ids, versions
}

// refsIn renders "(content_kind, content_id, content_version_id) IN (...)" over
// positional arrays bound at $n..$n+2; tenant is bound separately.
func refsIn(n int) string {
	return "(content_kind, content_id, content_version_id) IN (SELECT * FROM unnest($" + strconv.Itoa(n) +
		"::text[], $" + strconv.Itoa(n+1) + "::text[], $" + strconv.Itoa(n+2) + "::text[]))"
}

// beginMutation fixes the isolation required by the source-fence protocol.
// After an advisory-lock wait, the fence query must see the eraser's committed
// row, even when the host configured a repeatable-read session default.
func (s *store) beginMutation(ctx context.Context) (pgx.Tx, error) {
	return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
}
