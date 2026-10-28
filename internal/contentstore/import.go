package contentstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	content "github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/versionid"
)

var (
	ErrInvalid = errors.New("content: invalid import")
)

var (
	ErrConflict = errors.New("content: import conflict")
)

func invalidImport(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

type keyRow struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
}

func (rt *Runtime) importRef(r contentref.ContentRef) (keyRow, error) {
	if err := rt.checkRef(r); err != nil {
		return keyRow{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return keyRow{r.ContentKind, r.ContentID, r.Version()}, nil
}

func stamp(t *time.Time, or time.Time) time.Time {
	if t != nil {
		return *t
	}
	return or
}

// fenceSubjects locks the users' erasure fences, as every write by them
// does, and refuses an erased one.
func (rt *Runtime) fenceSubjects(ctx context.Context, tx pgx.Tx, users []string) error {
	users = slices.Compact(slices.Sorted(slices.Values(slices.DeleteFunc(slices.Clone(users), func(u string) bool { return u == "" }))))
	if len(users) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || u, 0)) FROM unnest($2::text[]) AS u ORDER BY u`,
		rt.schema+"\x1ferasure\x1f"+rt.tenant+"\x1f", users); err != nil {
		return err
	}
	var erased string
	if err := tx.QueryRow(ctx, `SELECT coalesce(min(actor_id), '') FROM `+rt.erasedSubjectsTable()+` WHERE tenant_id = $1 AND actor_id = ANY($2)`,
		rt.tenant, users).Scan(&erased); err != nil {
		return err
	}
	if erased != "" {
		return fmt.Errorf("%w: %s", ErrSubjectErased, erased)
	}
	return nil
}

// ImportComments writes comments in tx, inserting new ones and updating
// stored ones whose import changed (a moderator's decision stays), then
// recounts their references' comment counts and their threads' replies.
func (rt *Runtime) ImportComments(ctx context.Context, tx pgx.Tx, in []content.ImportComment) error {
	if len(in) == 0 {
		return nil
	}
	type row struct {
		ID        string     `json:"id"`
		Kind      string     `json:"kind"`
		ContentID string     `json:"content_id"`
		Version   string     `json:"version,omitempty"`
		Language  string     `json:"language"`
		ReplyTo   *string    `json:"reply_to"`
		UserID    *string    `json:"user_id"`
		AnonName  *string    `json:"anon_name"`
		Body      string     `json:"body"`
		DeletedAt *time.Time `json:"deleted_at"`
		CreatedAt time.Time  `json:"created_at"`
		UpdatedAt time.Time  `json:"updated_at"`
	}
	now := time.Now()
	rows := make([]row, 0, len(in))
	byID := map[string]int{}
	var users, parents []string
	for _, c := range in {
		if err := contentref.ValidateID(c.ID); err != nil {
			return invalidImport("comment id: %v", err)
		}
		if _, dup := byID[c.ID]; dup {
			return invalidImport("comment %s is imported twice", c.ID)
		}
		k, err := rt.importRef(c.Ref)
		if err != nil {
			return err
		}
		if k.Kind == content.KindComment {
			return invalidImport("a comment is on content, not on a comment")
		}
		r := row{ID: c.ID, Kind: k.Kind, ContentID: k.ID, Version: k.Version, Language: strings.TrimSpace(c.Language), DeletedAt: c.DeletedAt}
		if len(r.Language) > 16 {
			return invalidImport("language %q", r.Language)
		}
		switch user, anon := strings.TrimSpace(c.UserID), strings.TrimSpace(c.AnonName); {
		case user != "":
			r.UserID = &user
			users = append(users, user)
		case anon != "":
			r.AnonName = &anon
		default:
			return invalidImport("comment %s needs a user_id or an anon_name", c.ID)
		}
		body := strings.TrimSpace(c.Body)
		if body == "" {
			return invalidImport("comment %s has no body", c.ID)
		}
		if r.Body, err = rt.processor.Sanitize(ctx, body); err != nil {
			return err
		}
		if r.Body = strings.TrimSpace(r.Body); r.Body == "" {
			r.Body = commentTombstone
		}
		if c.ReplyToID != "" {
			if err := contentref.ValidateID(c.ReplyToID); err != nil {
				return invalidImport("reply_to: %v", err)
			}
			if c.ReplyToID == c.ID {
				return invalidImport("comment %s replies to itself", c.ID)
			}
			parent := c.ReplyToID
			r.ReplyTo = &parent
			parents = append(parents, parent)
		}
		r.CreatedAt = stamp(c.CreatedAt, now)
		r.UpdatedAt = stamp(c.UpdatedAt, r.CreatedAt)
		byID[c.ID] = len(rows)
		rows = append(rows, r)
	}
	if err := rt.fenceSubjects(ctx, tx, users); err != nil {
		return err
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	s := rt.store
	// Stored parents and the stored comments themselves (another tenant's id
	// is a conflict; a moved comment recounts its old reference too).
	type stored struct {
		tenant, replyTo string
		key             keyRow
	}
	known := map[string]stored{}
	res, err := tx.Query(ctx, `SELECT id::text, tenant_id, coalesce(reply_to_id::text, ''), content_kind, content_id, content_version_id FROM `+s.t.comments+`
 WHERE id = ANY($1::uuid[])`, append(slices.Clone(ids), parents...))
	if err != nil {
		return err
	}
	for res.Next() {
		var id string
		var c stored
		if err := res.Scan(&id, &c.tenant, &c.replyTo, &c.key.Kind, &c.key.ID, versionid.Into(&c.key.Version)); err != nil {
			res.Close()
			return err
		}
		known[id] = c
	}
	res.Close()
	if err := res.Err(); err != nil {
		return err
	}
	keys := map[keyRow]bool{}
	threads := map[string]bool{}
	for i := range rows {
		r := &rows[i]
		if c, ok := known[r.ID]; ok {
			if c.tenant != rt.tenant {
				return fmt.Errorf("%w: comment %s belongs to another tenant", ErrConflict, r.ID)
			}
			keys[c.key] = true
			if c.replyTo != "" {
				threads[c.replyTo] = true
			}
		}
		key := keyRow{r.Kind, r.ContentID, r.Version}
		if r.ReplyTo == nil {
			keys[key] = true
			continue
		}
		// The thread's root: the parent, or the comment the parent replies to.
		root, parent := *r.ReplyTo, keyRow{}
		for hops := 0; ; hops++ {
			var up string
			if j, ok := byID[root]; ok {
				parent = keyRow{rows[j].Kind, rows[j].ContentID, rows[j].Version}
				if rows[j].ReplyTo != nil {
					up = *rows[j].ReplyTo
				}
			} else if c, ok := known[root]; ok && c.tenant == rt.tenant {
				parent, up = c.key, c.replyTo
			} else {
				return fmt.Errorf("%w: comment %s replies to %s, which is not imported", ErrNotFound, r.ID, root)
			}
			if up == "" {
				break
			}
			if hops > 16 || up == r.ID {
				return invalidImport("comment %s replies in a loop", r.ID)
			}
			root = up
		}
		if parent != key {
			return invalidImport("comment %s replies to a comment on another reference", r.ID)
		}
		r.ReplyTo = &root
		threads[root] = true
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.comments+` AS c (id, tenant_id, content_kind, content_id, content_version_id, language, reply_to_id,
 user_id, anon_name, body, deleted_at, created_at, updated_at, author_hidden)
 SELECT r.id::uuid, $1, r.kind, r.content_id, r.version, r.language, r.reply_to::uuid, r.user_id, r.anon_name, r.body, r.deleted_at,
  r.created_at, r.updated_at, EXISTS (SELECT 1 FROM `+rt.hiddenAuthorsTable()+` h WHERE h.tenant_id=$1 AND h.actor_id=r.user_id)
 FROM jsonb_to_recordset($2::jsonb) AS r(id text, kind text, content_id text, version text, language text, reply_to text, user_id text,
  anon_name text, body text, deleted_at timestamptz, created_at timestamptz, updated_at timestamptz)
 ON CONFLICT (tenant_id, id) DO UPDATE SET content_kind = EXCLUDED.content_kind, content_id = EXCLUDED.content_id, content_version_id = EXCLUDED.content_version_id,
  author_hidden = EXCLUDED.author_hidden, language = EXCLUDED.language, reply_to_id = EXCLUDED.reply_to_id, user_id = EXCLUDED.user_id, anon_name = EXCLUDED.anon_name, body = EXCLUDED.body,
  deleted_at = EXCLUDED.deleted_at, created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at
 WHERE c.tenant_id = $1 AND (c.content_kind, c.content_id, c.content_version_id, c.language, c.reply_to_id, c.user_id, c.anon_name, c.body, c.deleted_at, c.created_at, c.author_hidden)
  IS DISTINCT FROM (EXCLUDED.content_kind, EXCLUDED.content_id, EXCLUDED.content_version_id, EXCLUDED.language, EXCLUDED.reply_to_id, EXCLUDED.user_id,
  EXCLUDED.anon_name, EXCLUDED.body, EXCLUDED.deleted_at, EXCLUDED.created_at, EXCLUDED.author_hidden)`, rt.tenant, string(data)); err != nil {
		return err
	}
	if err := rt.recount(ctx, tx, keys); err != nil {
		return err
	}
	return rt.recountReplies(ctx, tx, threads)
}

// ImportReactions writes reactions in tx: a new value takes a fresh
// preference revision, so preference sync exports it. Counts follow.
func (rt *Runtime) ImportReactions(ctx context.Context, tx pgx.Tx, in []content.ImportReaction) error {
	if len(in) == 0 {
		return nil
	}
	type row struct {
		keyRow
		UserID    *string   `json:"user_id"`
		IP        *string   `json:"ip"`
		Value     int16     `json:"value"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	now := time.Now()
	index := map[string]int{}
	var byUser, byIP []row
	var users []string
	keys := map[keyRow]bool{}
	for _, r := range in {
		k, err := rt.importRef(r.Ref)
		if err != nil {
			return err
		}
		if r.Value < -1 || r.Value > 1 {
			return invalidImport("reaction value %d is -1, 0 or 1", r.Value)
		}
		x := row{keyRow: k, Value: r.Value, CreatedAt: stamp(r.CreatedAt, now)}
		x.UpdatedAt = stamp(r.UpdatedAt, x.CreatedAt)
		user, ip := strings.TrimSpace(r.UserID), strings.TrimSpace(r.IP)
		var id string
		switch {
		case user != "":
			x.UserID, id = &user, "u\x00"+user
			users = append(users, user)
		case ip != "":
			x.IP, id = &ip, "i\x00"+ip
		default:
			return invalidImport("a reaction needs a user_id or an ip")
		}
		keys[k] = true
		// The last of a repeated reaction wins.
		id += "\x00" + k.Kind + "\x00" + k.ID + "\x00" + k.Version
		list := &byIP
		if x.UserID != nil {
			list = &byUser
		}
		if i, ok := index[id]; ok {
			(*list)[i] = x
			continue
		}
		index[id] = len(*list)
		*list = append(*list, x)
	}
	if err := rt.fenceSubjects(ctx, tx, users); err != nil {
		return err
	}
	s := rt.store
	for _, part := range []struct {
		rows   []row
		target string
	}{{byUser, `(tenant_id, content_kind, content_id, content_version_id, user_id) WHERE user_id IS NOT NULL`},
		{byIP, `(tenant_id, content_kind, content_id, content_version_id, ip) WHERE user_id IS NULL AND ip IS NOT NULL`}} {
		if len(part.rows) == 0 {
			continue
		}
		data, err := json.Marshal(part.rows)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.reactions+` AS x (tenant_id, content_kind, content_id, content_version_id, user_id, ip, value, revision,
 created_at, updated_at, author_hidden)
 SELECT $1, r.kind, r.id, r.version, r.user_id, r.ip, r.value, `+s.nextRevision()+`, r.created_at, r.updated_at,
 EXISTS (SELECT 1 FROM `+rt.hiddenAuthorsTable()+` h WHERE h.tenant_id=$1 AND h.actor_id=r.user_id)
 FROM jsonb_to_recordset($2::jsonb) AS r(kind text, id text, version text, user_id text, ip text, value smallint, created_at timestamptz, updated_at timestamptz)
 ON CONFLICT `+part.target+` DO UPDATE SET value = EXCLUDED.value, revision = `+s.nextRevision()+`, updated_at = EXCLUDED.updated_at
 WHERE x.value <> EXCLUDED.value`, rt.tenant, string(data)); err != nil {
			return err
		}
	}
	return rt.recount(ctx, tx, keys)
}

// ImportFavorites writes bookmarks in tx: a new one takes a fresh preference
// revision. Counts follow.
func (rt *Runtime) ImportFavorites(ctx context.Context, tx pgx.Tx, in []content.ImportFavorite) error {
	if len(in) == 0 {
		return nil
	}
	type row struct {
		keyRow
		UserID    string    `json:"user_id"`
		CreatedAt time.Time `json:"created_at"`
	}
	now := time.Now()
	index := map[string]int{}
	var rows []row
	var users []string
	keys := map[keyRow]bool{}
	for _, f := range in {
		k, err := rt.importRef(f.Ref)
		if err != nil {
			return err
		}
		if k.Kind == content.KindComment {
			return invalidImport("comments are not favorited")
		}
		user := strings.TrimSpace(f.UserID)
		if user == "" {
			return invalidImport("a favorite needs a user_id")
		}
		users, keys[k] = append(users, user), true
		id := user + "\x00" + k.Kind + "\x00" + k.ID + "\x00" + k.Version
		x := row{keyRow: k, UserID: user, CreatedAt: stamp(f.CreatedAt, now)}
		if i, ok := index[id]; ok {
			rows[i] = x
			continue
		}
		index[id] = len(rows)
		rows = append(rows, x)
	}
	if err := rt.fenceSubjects(ctx, tx, users); err != nil {
		return err
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	s := rt.store
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.favorites+` AS f (tenant_id, user_id, content_kind, content_id, content_version_id, value, revision,
 created_at, updated_at)
 SELECT $1, r.user_id, r.kind, r.id, r.version, 1, `+s.nextRevision()+`, r.created_at, r.created_at
 FROM jsonb_to_recordset($2::jsonb) AS r(user_id text, kind text, id text, version text, created_at timestamptz)
 ON CONFLICT (tenant_id, user_id, content_kind, content_id, content_version_id) WHERE user_id IS NOT NULL DO UPDATE SET value = 1, revision = `+s.nextRevision()+`,
  created_at = EXCLUDED.created_at, updated_at = clock_timestamp()
 WHERE f.value <> 1`, rt.tenant, string(data)); err != nil {
		return err
	}
	return rt.recount(ctx, tx, keys)
}

// recount sets the references' counts to what their rows hold: likes and
// dislikes, favorites, published top-level comments, and a comment's own
// like and dislike counters.
func (rt *Runtime) recount(ctx context.Context, tx pgx.Tx, keys map[keyRow]bool) error {
	if len(keys) == 0 {
		return nil
	}
	list := make([]keyRow, 0, len(keys))
	for k := range keys {
		list = append(list, k)
	}
	slices.SortFunc(list, func(a, b keyRow) int {
		return strings.Compare(a.Kind+"\x00"+a.ID+"\x00"+a.Version, b.Kind+"\x00"+b.ID+"\x00"+b.Version)
	})
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	var comments []string
	for _, k := range list {
		if k.Kind == content.KindComment && k.Version == "" {
			comments = append(comments, k.ID)
		}
	}
	// Each count below is its own statement: under READ COMMITTED it reads
	// a snapshot taken after the locks are held, so it includes the live
	// writes it waited for. Comments lock before rollups, comments in id order
	// and rollups in list order, as erasure and live reactions lock them.
	s := rt.store
	if len(comments) > 0 {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM `+s.t.comments+` WHERE tenant_id = $1 AND id = ANY($2::uuid[]) ORDER BY id FOR UPDATE`, rt.tenant, comments); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.counts+` (tenant_id, content_kind, content_id, content_version_id)
SELECT $1, k.kind, k.id, k.version FROM ROWS FROM (jsonb_to_recordset($2::jsonb) AS (kind text, id text, version text)) WITH ORDINALITY AS k(kind, id, version, n) ORDER BY k.n
ON CONFLICT (tenant_id, content_kind, content_id, content_version_id) DO NOTHING`, rt.tenant, string(data)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM `+s.t.counts+` x JOIN ROWS FROM (jsonb_to_recordset($2::jsonb) AS (kind text, id text, version text)) WITH ORDINALITY AS k(kind, id, version, n)
 ON x.content_kind = k.kind AND x.content_id = k.id AND x.content_version_id IS NOT DISTINCT FROM k.version
 WHERE x.tenant_id = $1 ORDER BY k.n FOR UPDATE OF x`, rt.tenant, string(data)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `WITH k AS (SELECT * FROM jsonb_to_recordset($2::jsonb) AS k(kind text, id text, version text)),
n AS (SELECT k.kind, k.id, k.version,
 (SELECT count(*) FROM `+s.t.reactions+` r WHERE r.tenant_id = $1 AND r.content_kind = k.kind AND r.content_id = k.id AND r.content_version_id IS NOT DISTINCT FROM k.version AND r.value = 1 AND NOT r.author_hidden)::int AS likes,
 (SELECT count(*) FROM `+s.t.reactions+` r WHERE r.tenant_id = $1 AND r.content_kind = k.kind AND r.content_id = k.id AND r.content_version_id IS NOT DISTINCT FROM k.version AND r.value = -1 AND NOT r.author_hidden)::int AS dislikes,
 (SELECT count(*) FROM `+s.t.favorites+` f WHERE f.tenant_id = $1 AND f.content_kind = k.kind AND f.content_id = k.id AND f.content_version_id IS NOT DISTINCT FROM k.version AND f.value = 1)::int AS favorites,
 (SELECT count(*) FROM `+s.t.comments+` c WHERE c.tenant_id = $1 AND c.content_kind = k.kind AND c.content_id = k.id AND c.content_version_id IS NOT DISTINCT FROM k.version
   AND c.reply_to_id IS NULL AND c.deleted_at IS NULL AND c.moderation = 'approved' AND NOT c.author_hidden)::int AS comments
 FROM k)
UPDATE `+s.t.counts+` x SET likes = n.likes, dislikes = n.dislikes, favorites = n.favorites, comment_count = n.comments, updated_at = now()
FROM n WHERE x.tenant_id = $1 AND x.content_kind = n.kind AND x.content_id = n.id AND x.content_version_id IS NOT DISTINCT FROM n.version
 AND (x.likes, x.dislikes, x.favorites, x.comment_count) IS DISTINCT FROM (n.likes, n.dislikes, n.favorites, n.comments)`,
		rt.tenant, string(data)); err != nil {
		return err
	}
	if len(comments) == 0 {
		return nil
	}
	_, err = tx.Exec(ctx, `UPDATE `+s.t.comments+` c SET likes = n.likes, dislikes = n.dislikes FROM (SELECT x.id,
 (SELECT count(*) FROM `+s.t.reactions+` r WHERE r.tenant_id = $1 AND r.content_kind = $3 AND r.content_id = x.id AND r.content_version_id IS NULL AND r.value = 1 AND NOT r.author_hidden)::int AS likes,
 (SELECT count(*) FROM `+s.t.reactions+` r WHERE r.tenant_id = $1 AND r.content_kind = $3 AND r.content_id = x.id AND r.content_version_id IS NULL AND r.value = -1 AND NOT r.author_hidden)::int AS dislikes
 FROM unnest($2::text[]) AS x(id)) n
 WHERE c.tenant_id = $1 AND c.id = n.id::uuid AND (c.likes, c.dislikes) IS DISTINCT FROM (n.likes, n.dislikes)`, rt.tenant, comments, content.KindComment)
	return err
}

// recountReplies sets top-level comments' reply counts to their published
// replies.
func (rt *Runtime) recountReplies(ctx context.Context, tx pgx.Tx, threads map[string]bool) error {
	if len(threads) == 0 {
		return nil
	}
	ids := make([]string, 0, len(threads))
	for id := range threads {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	s := rt.store
	_, err := tx.Exec(ctx, `UPDATE `+s.t.comments+` p SET reply_count = n.replies FROM (SELECT x.id,
 (SELECT count(*) FROM `+s.t.comments+` r WHERE r.tenant_id = $1 AND r.reply_to_id = x.id AND r.deleted_at IS NULL AND r.moderation = 'approved' AND NOT r.author_hidden)::int AS replies
 FROM unnest($2::uuid[]) AS x(id)) n
 WHERE p.tenant_id = $1 AND p.id = n.id AND p.reply_count <> n.replies`, rt.tenant, ids)
	return err
}

// ImportPolls writes polls and their options in tx; options the import no
// longer lists go, with their votes.
func (rt *Runtime) ImportPolls(ctx context.Context, tx pgx.Tx, in []content.ImportPoll) error {
	if len(in) == 0 {
		return nil
	}
	type row struct {
		ID        string     `json:"id"`
		Question  string     `json:"question"`
		Language  string     `json:"language"`
		Active    bool       `json:"active"`
		LiveAt    time.Time  `json:"live_at"`
		ClosesAt  *time.Time `json:"closes_at"`
		DeletedAt *time.Time `json:"deleted_at"`
		CreatedAt time.Time  `json:"created_at"`
		UpdatedAt time.Time  `json:"updated_at"`
	}
	type optionRow struct {
		content.ImportPollOption
		PollID string `json:"poll_id"`
	}
	now := time.Now()
	var rows []row
	var options []optionRow
	ids := make([]string, 0, len(in))
	for _, p := range in {
		if err := contentref.ValidateID(p.ID); err != nil {
			return invalidImport("poll id: %v", err)
		}
		if slices.Contains(ids, p.ID) {
			return invalidImport("poll %s is imported twice", p.ID)
		}
		ids = append(ids, p.ID)
		r := row{ID: p.ID, Question: strings.TrimSpace(p.Question), Language: strings.TrimSpace(p.Language), Active: p.Active,
			ClosesAt: p.ClosesAt, DeletedAt: p.DeletedAt}
		if p.Kind != "" && p.Kind != content.PollMultipleChoice {
			return invalidImport("poll kind %q", p.Kind)
		}
		if r.Question == "" {
			return invalidImport("poll %s has no question", p.ID)
		}
		r.CreatedAt = stamp(p.CreatedAt, now)
		r.UpdatedAt = stamp(p.UpdatedAt, r.CreatedAt)
		r.LiveAt = stamp(p.LiveAt, r.CreatedAt)
		if r.ClosesAt != nil && !r.ClosesAt.After(r.LiveAt) {
			return invalidImport("poll %s closes before it opens", p.ID)
		}
		rows = append(rows, r)
		for _, o := range p.Options {
			if err := contentref.ValidateID(o.ID); err != nil {
				return invalidImport("poll option id: %v", err)
			}
			if o.Label = strings.TrimSpace(o.Label); o.Label == "" {
				return invalidImport("poll %s has an option without a label", p.ID)
			}
			options = append(options, optionRow{ImportPollOption: o, PollID: p.ID})
		}
	}
	s := rt.store
	var foreign string
	if err := tx.QueryRow(ctx, `SELECT coalesce(min(id::text), '') FROM `+s.t.pollQuestions+` WHERE id = ANY($1::uuid[]) AND tenant_id <> $2`, ids, rt.tenant).Scan(&foreign); err != nil {
		return err
	}
	if foreign != "" {
		return fmt.Errorf("%w: poll %s belongs to another tenant", ErrConflict, foreign)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.pollQuestions+` AS q (id, tenant_id, question, language, is_active, live_at, closes_at, deleted_at,
 created_at, updated_at)
 SELECT r.id::uuid, $1, r.question, r.language, r.active, r.live_at, r.closes_at, r.deleted_at, r.created_at, r.updated_at
 FROM jsonb_to_recordset($2::jsonb) AS r(id text, question text, language text, active bool, live_at timestamptz, closes_at timestamptz,
  deleted_at timestamptz, created_at timestamptz, updated_at timestamptz)
 ON CONFLICT (tenant_id, id) DO UPDATE SET question = EXCLUDED.question, language = EXCLUDED.language, is_active = EXCLUDED.is_active,
  live_at = EXCLUDED.live_at, closes_at = EXCLUDED.closes_at, deleted_at = EXCLUDED.deleted_at, created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at
 WHERE q.tenant_id = $1 AND (q.question, q.language, q.is_active, q.live_at, q.closes_at, q.deleted_at, q.created_at)
  IS DISTINCT FROM (EXCLUDED.question, EXCLUDED.language, EXCLUDED.is_active, EXCLUDED.live_at, EXCLUDED.closes_at, EXCLUDED.deleted_at,
  EXCLUDED.created_at)`, rt.tenant, string(data)); err != nil {
		return err
	}
	odata, err := json.Marshal(options)
	if err != nil {
		return err
	}
	var moved string
	if err := tx.QueryRow(ctx, `SELECT coalesce(min(o.id::text), '') FROM jsonb_to_recordset($1::jsonb) AS r(id text, poll_id text)
 JOIN `+s.t.pollOptions+` o ON o.tenant_id = $2 AND o.id = r.id::uuid WHERE o.question_id <> r.poll_id::uuid`, string(odata), rt.tenant).Scan(&moved); err != nil {
		return err
	}
	if moved != "" {
		return fmt.Errorf("%w: poll option %s belongs to another poll", ErrConflict, moved)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.pollOptions+` o WHERE o.tenant_id = $3 AND o.question_id = ANY($1::uuid[])
 AND NOT EXISTS (SELECT 1 FROM jsonb_to_recordset($2::jsonb) AS r(id text) WHERE r.id::uuid = o.id)`, ids, string(odata), rt.tenant); err != nil {
		return err
	}
	if len(options) == 0 {
		return nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO `+s.t.pollOptions+` AS o (tenant_id, id, question_id, label, position)
 SELECT $2, r.id::uuid, r.poll_id::uuid, r.label, r.position FROM jsonb_to_recordset($1::jsonb) AS r(id text, poll_id text, label text, position int)
 ON CONFLICT (tenant_id, id) DO UPDATE SET label = EXCLUDED.label, position = EXCLUDED.position
 WHERE (o.label, o.position) IS DISTINCT FROM (EXCLUDED.label, EXCLUDED.position)`, string(odata), rt.tenant)
	return err
}

// ImportVotes writes votes in tx (one per person, or IP, per poll; the last
// wins) and recounts their polls' options.
func (rt *Runtime) ImportVotes(ctx context.Context, tx pgx.Tx, in []content.ImportVote) error {
	if len(in) == 0 {
		return nil
	}
	type row struct {
		PollID    string    `json:"poll_id"`
		OptionID  string    `json:"option_id"`
		UserID    *string   `json:"user_id"`
		IP        *string   `json:"ip"`
		CreatedAt time.Time `json:"created_at"`
	}
	now := time.Now()
	index := map[string]int{}
	var byUser, byIP []row
	var users, polls []string
	for _, v := range in {
		if contentref.ValidateID(v.PollID) != nil || contentref.ValidateID(v.OptionID) != nil {
			return invalidImport("a vote names a poll and an option by id")
		}
		x := row{PollID: v.PollID, OptionID: v.OptionID, CreatedAt: stamp(v.CreatedAt, now)}
		user, ip := strings.TrimSpace(v.UserID), strings.TrimSpace(v.IP)
		id, list := v.PollID+"\x00", &byUser
		switch {
		case user != "":
			x.UserID, id = &user, id+"u"+user
			users = append(users, user)
		case ip != "":
			x.IP, id, list = &ip, id+"i"+ip, &byIP
		default:
			return invalidImport("a vote needs a user_id or an ip")
		}
		polls = append(polls, v.PollID)
		if i, ok := index[id]; ok {
			(*list)[i] = x
			continue
		}
		index[id] = len(*list)
		*list = append(*list, x)
	}
	if err := rt.fenceSubjects(ctx, tx, users); err != nil {
		return err
	}
	s := rt.store
	all, err := json.Marshal(append(slices.Clone(byUser), byIP...))
	if err != nil {
		return err
	}
	var bad string
	if err := tx.QueryRow(ctx, `SELECT coalesce(min(r.option_id), '') FROM jsonb_to_recordset($1::jsonb) AS r(poll_id text, option_id text)
 WHERE NOT EXISTS (SELECT 1 FROM `+s.t.pollOptions+` o
  WHERE o.tenant_id = $2 AND o.id = r.option_id::uuid AND o.question_id = r.poll_id::uuid)`, string(all), rt.tenant).Scan(&bad); err != nil {
		return err
	}
	if bad != "" {
		return fmt.Errorf("%w: option %s of a multiple-choice poll", ErrNotFound, bad)
	}
	for _, part := range []struct {
		rows   []row
		target string
	}{{byUser, `(tenant_id, question_id, user_id) WHERE user_id IS NOT NULL`}, {byIP, `(tenant_id, question_id, ip) WHERE user_id IS NULL AND ip IS NOT NULL`}} {
		if len(part.rows) == 0 {
			continue
		}
		data, err := json.Marshal(part.rows)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.pollVotes+` AS v (tenant_id, question_id, option_id, user_id, ip, created_at)
 SELECT $2, r.poll_id::uuid, r.option_id::uuid, r.user_id, r.ip, r.created_at
 FROM jsonb_to_recordset($1::jsonb) AS r(poll_id text, option_id text, user_id text, ip text, created_at timestamptz)
 ON CONFLICT `+part.target+` DO UPDATE SET option_id = EXCLUDED.option_id, created_at = EXCLUDED.created_at
 WHERE (v.option_id, v.created_at) IS DISTINCT FROM (EXCLUDED.option_id, EXCLUDED.created_at)`, string(data), rt.tenant); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE `+s.t.pollOptions+` o SET vote_count = n.votes FROM (SELECT x.id, coalesce(c.votes, 0) AS votes
 FROM `+s.t.pollOptions+` x LEFT JOIN (SELECT option_id, count(*)::int AS votes FROM `+s.t.pollVotes+` WHERE tenant_id = $2 AND question_id = ANY($1::uuid[])
  GROUP BY option_id) c ON c.option_id = x.id WHERE x.tenant_id = $2 AND x.question_id = ANY($1::uuid[])) n
 WHERE o.tenant_id = $2 AND o.id = n.id AND o.vote_count <> n.votes`, slices.Compact(slices.Sorted(slices.Values(polls))), rt.tenant)
	return err
}

// RaisePreferenceFloor advances the preference revision sequence so every
// later revision exceeds floor: after a Postgres restore, past the newest
// revision a retained signal plane holds. It never moves it back and fails
// outside the safe range.
func (rt *Runtime) RaisePreferenceFloor(ctx context.Context, tx pgx.Tx, floor int64) error {
	if floor < 0 || floor > maxPreferenceRevisionFloor {
		return invalidImport("preference floor %d is outside [0, %d]", floor, int64(maxPreferenceRevisionFloor))
	}
	s := rt.store
	_, err := tx.Exec(ctx, `SELECT setval('`+s.revisionSeq+`', greatest($1::bigint, (SELECT last_value FROM `+s.revisionSeq+`),
 (SELECT coalesce(max(revision), 0) FROM `+s.t.reactions+`), (SELECT coalesce(max(revision), 0) FROM `+s.t.favorites+`)))
 WHERE $1::bigint > (SELECT CASE WHEN is_called THEN last_value ELSE last_value - 1 END FROM `+s.revisionSeq+`)`, floor)
	return err
}

// ImportDeletes deletes comments and polls in tx as their delete routes do:
// a comment is tombstoned and its reference's and thread's counts follow; a
// poll tombstone and exact media retirement intent commit together. The
// existing content-deletion sweep erases files; deleted rows remain for history.
func (rt *Runtime) ImportDeletes(ctx context.Context, tx pgx.Tx, comments, polls []string) error {
	s := rt.store
	if len(comments) > 0 {
		rows, err := tx.Query(ctx, `UPDATE `+s.t.comments+` SET deleted_at = now(), updated_at = now()
 WHERE tenant_id = $1 AND id = ANY($2::uuid[]) AND deleted_at IS NULL
 RETURNING coalesce(reply_to_id::text, ''), content_kind, content_id, coalesce(content_version_id, '')`, rt.tenant, comments)
		if err != nil {
			return err
		}
		keys, threads := map[keyRow]bool{}, map[string]bool{}
		for rows.Next() {
			var thread string
			var k keyRow
			if err := rows.Scan(&thread, &k.Kind, &k.ID, &k.Version); err != nil {
				rows.Close()
				return err
			}
			if thread == "" {
				keys[k] = true
			} else {
				threads[thread] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rt.recount(ctx, tx, keys); err != nil {
			return err
		}
		if err := rt.recountReplies(ctx, tx, threads); err != nil {
			return err
		}
	}
	if len(polls) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `UPDATE `+s.t.pollQuestions+` SET deleted_at = now(), updated_at = now()
 WHERE tenant_id = $1 AND id = ANY($2::uuid[]) AND deleted_at IS NULL RETURNING id::text`, rt.tenant, polls)
	if err != nil {
		return err
	}
	gone, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range gone {
		if err := rt.deleteMediaTx(ctx, tx, pollFolder, id); err != nil {
			return err
		}
	}
	return nil
}
