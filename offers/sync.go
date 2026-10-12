package offers

import (
	"context"
	"errors"
	"fmt"

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
		r, err := tx.Query(ctx, `SELECT id, terms, state, revision, synced_revision, coalesce(product_id, ''), coalesce(price_id, '')
			FROM `+s.t.offers+` WHERE tenant_id = $1 AND target_kind = $2 AND target_id = $3`, s.tenant, t.Kind, t.ID)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(r, func(r pgx.CollectableRow) (offerRow, error) {
			var o offerRow
			return o, r.Scan(&o.id, &o.terms, &o.state, &o.revision, &o.synced, &o.productID, &o.priceID)
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
		if !pending {
			return nil
		}
		if t.Kind == VersionTarget {
			if err := tx.QueryRow(ctx, `SELECT content_id FROM `+s.t.versions+` WHERE tenant_id = $1 AND id = $2`, s.tenant, t.ID).Scan(&t.Content); err != nil {
				return err
			}
		}
		productID, priceID, err := s.push(ctx, tx, t, active)
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
				product_id = coalesce(nullif($4, ''), o.product_id),
				price_id = CASE WHEN o.id = $5 THEN nullif($6, '') ELSE o.price_id END
			FROM unnest($2::text[], $3::bigint[]) AS r(id, rev)
			WHERE o.tenant_id = $1 AND o.id = r.id AND o.revision = r.rev`, s.tenant, ids, revs, productID, activeID, priceID)
		return err
	})
}

// push makes OpenRails sell t on active's terms, or nothing when active is
// nil; it answers the product and the live price.
func (s *Service) push(ctx context.Context, tx pgx.Tx, t Target, active *offerRow) (productID, priceID string, err error) {
	key, err := s.key(t)
	if err != nil {
		return "", "", err
	}
	name, err := s.displayName(ctx, tx, t)
	if err != nil {
		return "", "", err
	}
	products, err := s.billing.Products(ctx, []string{key.String()})
	if err != nil {
		return "", "", err
	}
	p, exists := products[key.String()]
	switch {
	case !exists && active == nil:
		return "", "", nil
	case !exists:
		created, err := s.billing.CreateProducts(ctx, []access.NewProduct{{Key: key.String(), DisplayName: name, Entitlements: []string{key.String()}}})
		if err != nil {
			return "", "", err
		}
		p = created[0]
	default:
		u := access.ProductUpdate{ID: p.ID}
		if p.DisplayName != name {
			u.DisplayName = &name
		}
		if archived := active == nil; p.Archived != archived {
			u.Archived = &archived
		}
		if u.DisplayName != nil || u.Archived != nil {
			updated, err := s.billing.UpdateProducts(ctx, []access.ProductUpdate{u})
			if err != nil {
				return "", "", err
			}
			p = updated[0]
		}
	}
	if active == nil {
		return p.ID, "", nil
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
