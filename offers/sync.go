package offers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
)

// Sync brings OpenRails in line with each target's offers: one product per
// access key (created when first sold, renamed after the target's title,
// archived while no offer is active) and, for a paid active offer, the
// price of its terms; a free offer leaves no price on sale. It is idempotent:
// prices are keyed by their terms, so a retry changes nothing. Targets are
// synced one at a time under a lock of their own.
func (s *Service) Sync(ctx context.Context, targets []Target) error {
	var errs []error
	for _, t := range targets {
		if err := s.syncTarget(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("offers: sync %s %s: %w", t.Kind, t.ID, err))
		}
	}
	return errors.Join(errs...)
}

type offerRow struct {
	id                 string
	terms              Terms
	state              State
	revision, synced   int64
	productID, priceID string
	contentID          string
}

func (s *Service) syncTarget(ctx context.Context, t Target) (err error) {
	var rows []offerRow
	defer func() {
		if err == nil || len(rows) == 0 {
			return
		}
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.id
		}
		if _, uerr := s.pool.Exec(context.WithoutCancel(ctx), `UPDATE `+s.t.offers+` SET last_error = $3 WHERE tenant_id = $1 AND id = ANY($2) AND synced_revision < revision`,
			s.tenant, ids, err.Error()); uerr != nil {
			s.log.ErrorContext(ctx, "offers: recording a sync failure", "err", uerr)
		}
	}()
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "contentkit:offer:"+s.tenant+":"+string(t.Kind)+":"+t.ID); err != nil {
			return err
		}
		r, err := tx.Query(ctx, `SELECT id, terms, state, revision, synced_revision, coalesce(product_id, ''), coalesce(price_id, ''), coalesce(content_id, '')
			FROM `+s.t.offers+` WHERE tenant_id = $1 AND target_kind = $2 AND target_id = $3`, s.tenant, t.Kind, t.ID)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(r, func(r pgx.CollectableRow) (offerRow, error) {
			var o offerRow
			return o, r.Scan(&o.id, &o.terms, &o.state, &o.revision, &o.synced, &o.productID, &o.priceID, &o.contentID)
		})
		if err != nil {
			return err
		}
		var active *offerRow
		pending := false
		for i := range rows {
			pending = pending || rows[i].synced < rows[i].revision
			if rows[i].state == Active {
				active = &rows[i]
			}
		}
		if !pending && active == nil {
			return nil
		}
		if t.Kind == VersionTarget && t.Content == "" {
			for _, row := range rows {
				if row.contentID != "" {
					t.Content = row.contentID
					break
				}
			}
			if t.Content == "" {
				err := tx.QueryRow(ctx, `SELECT content_id FROM `+s.t.versions+` WHERE tenant_id=$1 AND id=$2`, s.tenant, t.ID).Scan(&t.Content)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
		}
		if active != nil && t.Kind != GroupTarget {
			var kind string
			err := tx.QueryRow(ctx, `SELECT kind FROM `+s.t.items+` c WHERE tenant_id = $1 AND id = $2 AND state <> 'deleted'
 AND ($3::text = '' OR EXISTS (SELECT 1 FROM `+s.t.versions+` v WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.id=$3 AND v.state <> 'deleted'))`, s.tenant, orID(t), versionOf(t)).Scan(&kind)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if spec, exists := s.kinds.Kinds[kind]; !exists || !spec.Sellable() {
				// A missing/deleted target or stricter host policy withdraws the
				// offer. Retained rows keep the billing archive retryable.
				var revision int64
				if err := tx.QueryRow(ctx, `UPDATE `+s.t.offers+` SET state = 'withdrawn', revision = revision + 1, updated_at = now()
 WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND state = 'active' RETURNING revision`, s.tenant, active.id, active.revision).Scan(&revision); err != nil {
					return err
				}
				active.state, active.revision = Withdrawn, revision
				active, pending = nil, true
			}
		}
		if !pending {
			return nil
		}
		var productID, priceID string
		if active == nil {
			productID, err = s.archive(ctx, t, rows)
		} else {
			productID, priceID, err = s.push(ctx, tx, t, active)
		}
		if err != nil {
			return err
		}
		ids, revs := make([]string, len(rows)), make([]int64, len(rows))
		for i, o := range rows {
			ids[i], revs[i] = o.id, o.revision
		}
		activeID := ""
		if active != nil {
			activeID = active.id
		}
		_, err = tx.Exec(ctx, `UPDATE `+s.t.offers+` o SET synced_revision = o.revision, last_error = NULL,
				product_id = CASE WHEN $5 = '' THEN coalesce(o.product_id, nullif($4, '')) ELSE coalesce(nullif($4, ''), o.product_id) END,
				price_id = CASE WHEN o.id = $5 THEN nullif($6, '') ELSE o.price_id END
			FROM unnest($2::text[], $3::bigint[]) AS r(id, rev)
			WHERE o.tenant_id = $1 AND o.id = r.id AND o.revision = r.rev`, s.tenant, ids, revs, productID, activeID, priceID)
		return err
	})
}

// archive changes only the billing product's archived flag. It does not need
// source content, rename a historical purchase, or revoke a purchased key.
func (s *Service) archive(ctx context.Context, t Target, rows []offerRow) (string, error) {
	var ids []string
	for _, row := range rows {
		if row.productID != "" && !slices.Contains(ids, row.productID) {
			ids = append(ids, row.productID)
		}
	}
	if len(ids) == 0 {
		// A provider write might have succeeded before our transaction failed.
		// The retained version parent lets a retry find that product by key.
		key, err := s.key(t)
		if err != nil {
			return "", err
		}
		products, err := s.billing.Products(ctx, []string{key.String()})
		if err != nil {
			return "", err
		}
		if product, exists := products[key.String()]; exists {
			ids = append(ids, product.ID)
		}
	}
	if len(ids) == 0 {
		return "", nil // an offer withdrawn before its product ever existed
	}
	archived := true
	updates := make([]access.ProductUpdate, len(ids))
	for i, id := range ids {
		updates[i] = access.ProductUpdate{ID: id, Archived: &archived}
	}
	if _, err := s.billing.UpdateProducts(ctx, updates); err != nil {
		return "", err
	}
	if t.Kind == GroupTarget {
		if err := s.billing.StopRenewals(ctx, ids); err != nil {
			return "", err
		}
	}
	return ids[0], nil
}

// push makes OpenRails sell t on active's terms; it answers the product and
// the live price. Archival uses only the retained product identity instead.
func (s *Service) push(ctx context.Context, tx pgx.Tx, t Target, active *offerRow) (productID, priceID string, err error) {
	key, err := s.key(t)
	if err != nil {
		return "", "", err
	}
	name, err := s.displayName(ctx, tx, t)
	if err != nil {
		return "", "", err
	}
	required, err := s.required(ctx, tx, t)
	if err != nil {
		return "", "", err
	}
	products, err := s.billing.Products(ctx, []string{key.String()})
	if err != nil {
		return "", "", err
	}
	p, exists := products[key.String()]
	switch {
	case !exists:
		created, err := s.billing.CreateProducts(ctx, []access.NewProduct{{Key: key.String(), DisplayName: name, Entitlements: []string{key.String()},
			RequiredEntitlements: required}})
		if err != nil {
			return "", "", err
		}
		p = created[0]
	default:
		u := access.ProductUpdate{ID: p.ID}
		if p.DisplayName != name {
			u.DisplayName = &name
		}
		if archived := false; p.Archived != archived {
			u.Archived = &archived
		}
		if !slices.Equal(p.RequiredEntitlements, required) {
			u.RequiredEntitlements = &required
		}
		if u.DisplayName != nil || u.Archived != nil || u.RequiredEntitlements != nil {
			updated, err := s.billing.UpdateProducts(ctx, []access.ProductUpdate{u})
			if err != nil {
				return "", "", err
			}
			p = updated[0]
		}
	}
	if t.Kind == GroupTarget && active.terms.RenewHours == nil {
		// Free or one-time: no membership renews any more.
		if err := s.billing.StopRenewals(ctx, []string{p.ID}); err != nil {
			return "", "", err
		}
	}
	if active.terms.Amount == 0 {
		var live []string
		for _, pr := range p.Prices {
			live = append(live, pr.ID)
		}
		if len(live) > 0 {
			if err := s.billing.ArchivePrices(ctx, live); err != nil {
				return "", "", err
			}
		}
		return p.ID, "", nil
	}
	tm := active.terms
	prices, err := s.billing.CreatePrices(ctx, []access.NewPrice{{ProductID: p.ID, Key: PriceKey, Amount: int64(tm.Amount), Currency: tm.Currency,
		AccessHours: tm.AccessHours, IntervalHours: tm.RenewHours, PSPs: s.cfg.PSPs}})
	if err != nil {
		return "", "", err
	}
	return p.ID, prices[0].ID, nil
}

// required is what a buyer of t must already hold: the group's key when the
// rule it sells under is group_ppv (a content's own rule, a version's
// effective one).
func (s *Service) required(ctx context.Context, tx pgx.Tx, t Target) ([]string, error) {
	out := []string{}
	if t.Kind == GroupTarget {
		return out, nil
	}
	var crule, vrule []byte
	err := tx.QueryRow(ctx, `SELECT c.access, (SELECT v.access FROM `+s.t.versions+` v WHERE v.tenant_id = c.tenant_id AND v.id = $3)
		FROM `+s.t.items+` c WHERE c.tenant_id = $1 AND c.id = $2`, s.tenant, orID(t), versionOf(t)).Scan(&crule, &vrule)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var rule access.Rule
	if err := json.Unmarshal(crule, &rule); err != nil {
		return nil, err
	}
	if vrule != nil {
		var o access.Rule
		if err := json.Unmarshal(vrule, &o); err != nil {
			return nil, err
		}
		rule = access.Effective(rule, &o)
	}
	if rule.Level != access.GroupPPV {
		return out, nil
	}
	k, err := access.GroupKey(s.tenant, rule.Group)
	if err != nil {
		return nil, err
	}
	return []string{k.String()}, nil
}

// displayName is the product name: the title of the content, with a
// version's label, or the group's name, in the first configured language
// that has one.
func (s *Service) displayName(ctx context.Context, tx pgx.Tx, t Target) (string, error) {
	var name string
	var err error
	switch t.Kind {
	case ContentTarget, VersionTarget:
		err = tx.QueryRow(ctx, `SELECT coalesce((SELECT title FROM `+s.t.text+` WHERE tenant_id = $1 AND content_id = $2 AND title <> ''
			ORDER BY array_position($3::text[], language), language LIMIT 1), '')`, s.tenant, orID(t), s.languages()).Scan(&name)
		if err == nil && t.Kind == VersionTarget {
			var label string
			if err = tx.QueryRow(ctx, `SELECT label FROM `+s.t.versions+` WHERE tenant_id = $1 AND id = $2`, s.tenant, t.ID).Scan(&label); err == nil && label != "" {
				name += " (" + label + ")"
			}
		}
	case GroupTarget:
		err = tx.QueryRow(ctx, `SELECT coalesce((SELECT name FROM `+s.t.groupNames+` WHERE tenant_id = $1 AND group_id = $2 AND kind = 'name'
			ORDER BY array_position($3::text[], language), language LIMIT 1), (SELECT slug FROM `+s.t.groups+` WHERE tenant_id = $1 AND id = $2), '')`,
			s.tenant, t.ID, s.languages()).Scan(&name)
	}
	if err != nil {
		return "", err
	}
	if name == "" {
		name = string(t.Kind) + " " + t.ID
	}
	return name, nil
}
