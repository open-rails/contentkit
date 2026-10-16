package offers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// Put sets each target's offer, in one transaction: a target without an
// active offer gets one; new terms move the active offer to a new revision;
// equal terms change nothing. The target's ever_sold is set and a sync job
// is queued. It answers the active offers, in order.
func (s *Service) Put(ctx context.Context, puts []Put) ([]Offer, error) {
	if err := s.tx(ctx, func(tx pgx.Tx) error { return s.PutTx(ctx, tx, puts) }); err != nil {
		return nil, err
	}
	targets := make([]Target, len(puts))
	for i, p := range puts {
		targets[i] = p.Target
	}
	return s.active(ctx, targets)
}

// PutTx is Put in tx (the bulk import's).
func (s *Service) PutTx(ctx context.Context, tx pgx.Tx, puts []Put) error {
	targets := make([]Target, len(puts))
	terms := make([]Terms, len(puts))
	for i, p := range puts {
		var err error
		if terms[i], err = s.cfg.normalize(p.Target.Kind, p.Terms); err != nil {
			return err
		}
		targets[i] = p.Target
	}
	if err := s.resolve(ctx, tx, targets); err != nil {
		return err
	}
	var changed []Target
	for i, t := range targets {
		body, err := json.Marshal(terms[i])
		if err != nil {
			return err
		}
		var id string
		var old Terms
		err = tx.QueryRow(ctx, `SELECT id, terms FROM `+s.t.offers+` WHERE tenant_id = $1 AND target_kind = $2 AND target_id = $3 AND state = 'active' FOR UPDATE`,
			s.tenant, t.Kind, t.ID).Scan(&id, &old)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = tx.Exec(ctx, `INSERT INTO `+s.t.offers+` (tenant_id, id, target_kind, target_id, terms) VALUES ($1, $2, $3, $4, $5)`,
				s.tenant, contentref.NewID(), t.Kind, t.ID, body)
		case err != nil:
		case old.equal(terms[i]):
			continue
		default:
			_, err = tx.Exec(ctx, `UPDATE `+s.t.offers+` SET terms = $3, revision = revision + 1, updated_at = now() WHERE tenant_id = $1 AND id = $2`,
				s.tenant, id, body)
		}
		if err != nil {
			return conflict(err)
		}
		changed = append(changed, t)
	}
	if err := s.markSold(ctx, tx, changed); err != nil {
		return err
	}
	return s.enqueue(ctx, tx, changed)
}

// Withdraw ends each target's active offer, in one transaction, and queues
// its sync, which archives the product: buyers keep theirs, and a group's
// subscribers theirs until their paid period ends, without renewing.
// A target without an active offer is ErrNotFound.
func (s *Service) Withdraw(ctx context.Context, targets []Target) ([]Offer, error) {
	var ids []string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		ids = ids[:0]
		for _, t := range targets {
			var id string
			err := tx.QueryRow(ctx, `UPDATE `+s.t.offers+` SET state = 'withdrawn', revision = revision + 1, updated_at = now()
				WHERE tenant_id = $1 AND target_kind = $2 AND target_id = $3 AND state = 'active'
					AND ($2 <> 'version' OR EXISTS (SELECT 1 FROM `+s.t.versions+` v WHERE v.tenant_id = $1 AND v.id = $3 AND v.content_id = $4))
				RETURNING id`, s.tenant, t.Kind, t.ID, t.Content).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: %s %s has no offer", ErrNotFound, t.Kind, t.ID)
			}
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return s.enqueue(ctx, tx, targets)
	})
	if err != nil {
		return nil, err
	}
	return s.byID(ctx, ids)
}

// Live answers the active offers selling keys, by key string: what a paywall
// (access.Decision.Sell) can sell. Tier keys are the site catalog's.
func (s *Service) Live(ctx context.Context, keys []access.Key) (map[string]Offer, error) {
	var kinds, ids []string
	for _, k := range keys {
		if t, ok := targetOf(k); ok && k.Tenant() == s.tenant {
			kinds, ids = append(kinds, string(t.Kind)), append(ids, t.ID)
		}
	}
	out := map[string]Offer{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := s.query(ctx, `o.state = 'active' AND (o.target_kind, o.target_id) IN (SELECT * FROM unnest($2::text[], $3::text[])) AND (o.target_kind <> 'group' OR EXISTS (SELECT 1 FROM `+s.t.groups+` g WHERE g.tenant_id=o.tenant_id AND g.id=o.target_id AND g.state='active' AND NOT g.internal))`, kinds, ids)
	for _, o := range list {
		out[o.Key.String()] = o
	}
	return out, err
}

// ListQuery pages offers for staff.
type ListQuery struct {
	State    State // empty: any
	Unsynced bool  // only those OpenRails does not match yet
	Cursor   string
	Limit    int
}

// OfferPage is one page of offers, newest first.
type OfferPage struct {
	Offers     []Offer `json:"offers"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// List pages offers, newest first.
func (s *Service) List(ctx context.Context, q ListQuery) (OfferPage, error) {
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 50
	}
	list, err := s.query(ctx, `($2 = '' OR o.state = $2) AND (NOT $3 OR o.synced_revision < o.revision) AND ($4 = '' OR o.id < $4)
		ORDER BY o.id DESC LIMIT `+fmt.Sprint(q.Limit+1), string(q.State), q.Unsynced, q.Cursor)
	if err != nil {
		return OfferPage{}, err
	}
	page := OfferPage{Offers: list}
	if len(list) > q.Limit {
		page.Offers, page.NextCursor = list[:q.Limit], list[q.Limit-1].ID
	}
	return page, nil
}

func (s *Service) active(ctx context.Context, targets []Target) ([]Offer, error) {
	kinds, ids := make([]string, len(targets)), make([]string, len(targets))
	for i, t := range targets {
		kinds[i], ids[i] = string(t.Kind), t.ID
	}
	list, err := s.query(ctx, `o.state = 'active' AND (o.target_kind, o.target_id) IN (SELECT * FROM unnest($2::text[], $3::text[])) AND (o.target_kind <> 'group' OR EXISTS (SELECT 1 FROM `+s.t.groups+` g WHERE g.tenant_id=o.tenant_id AND g.id=o.target_id AND g.state='active' AND NOT g.internal))`, kinds, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Offer, len(targets))
	for i, t := range targets {
		j := slices.IndexFunc(list, func(o Offer) bool { return o.Target.Kind == t.Kind && o.Target.ID == t.ID })
		if j < 0 {
			return nil, fmt.Errorf("%w: %s %s has no offer", ErrNotFound, t.Kind, t.ID)
		}
		out[i] = list[j]
	}
	return out, nil
}

func (s *Service) byID(ctx context.Context, ids []string) ([]Offer, error) {
	list, err := s.query(ctx, `o.id = ANY($2)`, ids)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(list, func(a, b Offer) int { return slices.Index(ids, a.ID) - slices.Index(ids, b.ID) })
	return list, nil
}

// query reads offers matching where ($1 is the tenant).
func (s *Service) query(ctx context.Context, where string, args ...any) ([]Offer, error) {
	rows, err := s.pool.Query(ctx, `SELECT o.id, o.target_kind, o.target_id, coalesce(v.content_id, ''), o.terms, o.state,
			o.synced_revision >= o.revision, coalesce(o.product_id, ''), coalesce(o.price_id, ''), coalesce(o.last_error, ''), o.revision, o.updated_at
		FROM `+s.t.offers+` o LEFT JOIN `+s.t.versions+` v ON o.target_kind = 'version' AND v.tenant_id = o.tenant_id AND v.id = o.target_id
		WHERE o.tenant_id = $1 AND `+where, append([]any{s.tenant}, args...)...)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Offer, error) {
		var o Offer
		err := r.Scan(&o.ID, &o.Target.Kind, &o.Target.ID, &o.Target.Content, &o.Terms, &o.State, &o.Synced,
			&o.ProductID, &o.PriceID, &o.LastError, &o.Revision, &o.UpdatedAt)
		return o, err
	})
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Key, err = s.key(list[i].Target); err != nil {
			return nil, err
		}
	}
	return list, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// resolve checks each target exists and may be sold: a version must belong
// to the content it names, a group's kind must be sellable.
func (s *Service) resolve(ctx context.Context, q querier, targets []Target) error {
	for _, t := range targets {
		if _, err := s.key(Target{Kind: t.Kind, ID: t.ID, Content: orID(t)}); err != nil {
			return fmt.Errorf("%w: %w", ErrNotFound, err)
		}
		var ok bool
		var err error
		switch t.Kind {
		case ContentTarget:
			err = q.QueryRow(ctx, `SELECT true FROM `+s.t.items+` WHERE tenant_id = $1 AND id = $2 AND state <> 'deleted'`, s.tenant, t.ID).Scan(&ok)
		case VersionTarget:
			err = q.QueryRow(ctx, `SELECT v.content_id = $3 FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
				WHERE v.tenant_id = $1 AND v.id = $2 AND v.state <> 'deleted' AND c.state <> 'deleted'`, s.tenant, t.ID, t.Content).Scan(&ok)
		case GroupTarget:
			var kind string
			err = q.QueryRow(ctx, `SELECT kind FROM `+s.t.groups+` WHERE tenant_id = $1 AND id = $2 AND state = 'active'`, s.tenant, t.ID).Scan(&kind)
			if err == nil && !s.kinds.GroupKinds[kind].Sellable {
				return fmt.Errorf("%w: group kind %s is not sellable", ErrConflict, kind)
			}
			ok = err == nil
		}
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !ok {
			return fmt.Errorf("%w: %s %s", ErrNotFound, t.Kind, t.ID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func versionOf(t Target) string {
	if t.Kind == VersionTarget {
		return t.ID
	}
	return ""
}

func orID(t Target) string {
	if t.Content == "" {
		return t.ID
	}
	return t.Content
}

func (s *Service) markSold(ctx context.Context, tx pgx.Tx, targets []Target) error {
	var contents, groups []string
	for _, t := range targets {
		switch t.Kind {
		case ContentTarget:
			contents = append(contents, t.ID)
		case VersionTarget:
			contents = append(contents, t.Content)
		case GroupTarget:
			groups = append(groups, t.ID)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET ever_sold = true WHERE tenant_id = $1 AND id = ANY($2) AND NOT ever_sold`, s.tenant, contents); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE `+s.t.groups+` SET ever_sold = true WHERE tenant_id = $1 AND id = ANY($2) AND NOT ever_sold`, s.tenant, groups)
	return err
}

// Resync queues a sync of the active offers of targets in tx, for a change
// that moves what OpenRails shows (a content or group renamed); targets
// without one cost nothing.
func (s *Service) Resync(ctx context.Context, tx pgx.Tx, targets []Target) error {
	kinds, ids := make([]string, len(targets)), make([]string, len(targets))
	byKey := map[Target]Target{}
	for i, t := range targets {
		kinds[i], ids[i] = string(t.Kind), t.ID
		byKey[Target{Kind: t.Kind, ID: t.ID}] = t
	}
	rows, err := tx.Query(ctx, `UPDATE `+s.t.offers+` SET revision = revision + 1, updated_at = now()
		WHERE tenant_id = $1 AND state = 'active' AND (target_kind, target_id) IN (SELECT * FROM unnest($2::text[], $3::text[]))
		RETURNING target_kind, target_id`, s.tenant, kinds, ids)
	if err != nil {
		return err
	}
	active, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Target, error) {
		var t Target
		err := r.Scan(&t.Kind, &t.ID)
		return byKey[t], err
	})
	if err != nil {
		return err
	}
	return s.enqueue(ctx, tx, active)
}

// Authorize admits actor to change targets' offers (editors): staff, the
// content's creators, and editors of the user-owned group sold or owning the
// content.
func (s *Service) Authorize(ctx context.Context, a auth.Authenticator, actor access.Actor, targets []Target) error {
	if actor.Anonymous {
		return ErrForbidden
	}
	ok, err := s.editors(ctx, a, actor, targets)
	if err != nil {
		return err
	}
	if slices.Contains(ok, false) {
		return ErrForbidden
	}
	return nil
}

func conflict(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return fmt.Errorf("%w: a concurrent offer: %w", ErrConflict, err)
	}
	return err
}

// enqueue queues the sync of targets in tx when River is bound; otherwise the
// periodic reconcile finds them.
func (s *Service) enqueue(ctx context.Context, tx pgx.Tx, targets []Target) error {
	client := s.bound()
	if client == nil || len(targets) == 0 {
		return nil
	}
	params := make([]river.InsertManyParams, len(targets))
	for i, t := range targets {
		params[i] = river.InsertManyParams{Args: syncArgs{Tenant: s.tenant, Target: t}, InsertOpts: s.syncOpts()}
	}
	_, err := client.InsertManyTx(ctx, tx, params)
	return err
}
