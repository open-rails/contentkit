package contentstore

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/versionid"
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
	qs     string // quoted schema
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
	favorites     string
	counts        string
	commentBans   string
	codes         string
	items         string
	entities      string
	bannedWords   string
	ipBans        string

	preferenceSync string
}

func newStore(pool *pgxpool.Pool, schema, tenant string) *store {
	q := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	return &store{
		pool:   pool,
		schema: schema,
		qs:     pgx.Identifier{schema}.Sanitize(),
		tenant: tenant,
		t: tables{
			reactions:     q("content_reactions"),
			comments:      q("content_comments"),
			pollQuestions: q("content_poll_questions"),
			pollOptions:   q("content_poll_options"),
			pollVotes:     q("content_poll_votes"),
			pollAnswers:   q("content_poll_answers"),
			favorites:     q("content_favorites"),
			counts:        q("content_interaction_counts"),
			commentBans:   q("content_comment_bans"),
			codes:         q("content_codes"),
			items:         q("content_items"),
			entities:      q("content_entities"),
			bannedWords:   q("content_banned_words"),
			ipBans:        q("content_ip_bans"),

			preferenceSync: q("content_preference_sync"),
		},
		revisionSeq: revisionSeqName(schema),
	}
}

// keyCols is the content key of every keyed interaction table.
const keyCols = "tenant_id, content_kind, content_id, content_version_id"

// keyPred renders the key predicate with placeholders $n..$n+3 (see keyArgs);
// no version is NULL.
func keyPred(n int) string {
	return "tenant_id = $" + strconv.Itoa(n) + " AND content_kind = $" + strconv.Itoa(n+1) +
		" AND content_id = $" + strconv.Itoa(n+2) + " AND content_version_id IS NOT DISTINCT FROM $" + strconv.Itoa(n+3) + "::text"
}

// keyArgs are the bound values of keyPred and keyCols, in order.
func keyArgs(k contentref.ContentKey) []any {
	return []any{k.TenantID, k.ContentKind, k.ContentID, versionid.Arg(k.ContentVersionID)}
}

// beginMutation fixes the isolation required by the source-fence protocol.
// After an advisory-lock wait, the fence query must see the eraser's committed
// row, even when the host configured a repeatable-read session default.
func (s *store) beginMutation(ctx context.Context) (pgx.Tx, error) {
	return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
}
