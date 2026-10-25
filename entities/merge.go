package entities

import (
	"context"
	"fmt"

	"github.com/open-rails/contentkit/internal/codes"
)

// Merge folds entity From into entity Into of the same kind.
type Merge struct {
	From EntityID `json:"from"`
	Into EntityID `json:"into"`
}

// MergeInput names the entity a merge folds into.
type MergeInput struct {
	Into EntityID `json:"into"`
}

// MergeReport summarizes one merge.
type MergeReport struct {
	From             EntityID `json:"from"`
	Into             EntityID `json:"into"`
	MembershipsMoved int64    `json:"memberships_moved"`
	NamesMoved       int64    `json:"names_moved"`
	EdgesMoved       int64    `json:"edges_moved"`
	EntitiesMoved    int64    `json:"groups_moved"`
}

// Merge folds each From into its Into in one transaction: memberships move
// (duplicates collapse, an accepted membership wins over a proposed one),
// names become aliases of Into, edges, children and scoped entities re-point,
// From becomes merged and its code redirects to Into's. Entities of a
// user-owned or entitlement-backed kind are never merged (ErrConflict).
func (s *Store) Merge(ctx context.Context, merges []Merge) ([]MergeReport, error) {
	reports := make([]MergeReport, len(merges))
	if len(merges) == 0 {
		return reports, nil
	}
	var all []EntityID
	for _, m := range merges {
		if m.From == m.Into {
			return nil, fmt.Errorf("%w: cannot merge a group into itself", ErrInvalid)
		}
		all = append(all, m.From, m.Into)
	}
	list, err := uniqueIDs(all)
	if err != nil {
		return nil, err
	}
	err = s.run(ctx, func(q Querier) error {
		if s.references != nil {
			if err := checkReferenceTransaction(ctx, q); err != nil {
				return err
			}
		}
		if err := s.lockHierarchy(ctx, q); err != nil {
			return err
		}
		locked, err := s.lock(ctx, q, list)
		if err != nil {
			return err
		}
		byID := map[EntityID]Entity{}
		for _, g := range locked {
			byID[g.ID] = g
		}
		var touched []string
		for i, m := range merges {
			src, dst := byID[m.From], byID[m.Into]
			if err := s.mergeable(ctx, q, src, dst); err != nil {
				return err
			}
			if reports[i], err = s.merge(ctx, q, src, dst); err != nil {
				return err
			}
			if s.references != nil {
				if err := s.references.Merge(ctx, q, s.tenant, Ref{Kind: src.Kind, ID: src.ID}, Ref{Kind: dst.Kind, ID: dst.ID}); err != nil {
					return err
				}
			}
			src.State, src.MergedInto = StateMerged, dst.ID
			byID[src.ID] = src
			touched = append(touched, string(src.ID), string(dst.ID))
		}
		if err := s.checkLinks(ctx, q, list); err != nil {
			return err
		}
		if err := s.refreshEntityFacets(ctx, q, touched); err != nil {
			return err
		}
		if err := s.syncCodes(ctx, q, list); err != nil {
			return err
		}
		final, err := s.byIDs(ctx, q, list)
		if err != nil {
			return err
		}
		marks := make([]Entity, 0, len(final))
		for _, g := range final {
			marks = append(marks, g)
		}
		return s.markDirty(ctx, q, marks)
	})
	if err != nil {
		return nil, err
	}
	return reports, nil
}

func (s *Store) mergeable(ctx context.Context, q Querier, src, dst Entity) error {
	if src.State != StateActive || dst.State != StateActive {
		return fmt.Errorf("%w: both groups must be active", ErrConflict)
	}
	after := src
	after.State = StateMerged
	if err := s.checkPolicyBinding(src, after); err != nil {
		return err
	}
	if src.Kind != dst.Kind {
		return fmt.Errorf("%w: cannot merge a %s into a %s", ErrConflict, src.Kind, dst.Kind)
	}
	if src.Internal != dst.Internal {
		return fmt.Errorf("%w: internal and public groups cannot be merged; change their classification explicitly first", ErrConflict)
	}
	if s.cfg.EntityKinds[src.Kind].Owner == "user" {
		return fmt.Errorf("%w: %s groups are owned by users and never merged", ErrConflict, src.Kind)
	}
	if s.cfg.EntityKinds[src.Kind].Entitled {
		return fmt.Errorf("%w: entitlement-backed entities cannot be merged; their identity must remain stable", ErrConflict)
	}
	return nil
}

func (s *Store) merge(ctx context.Context, q Querier, src, dst Entity) (MergeReport, error) {
	r := MergeReport{From: src.ID, Into: dst.ID}
	from, into := string(src.ID), string(dst.ID)
	m := s.table("content_memberships")
	if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %[1]s d SET state = 'active' FROM %[1]s o
 WHERE o.tenant_id = $1 AND o.entity_id = $2 AND o.state = 'active' AND d.tenant_id = $1 AND d.entity_id = $3 AND d.state = 'proposed'
   AND d.content_id = o.content_id AND d.version_id IS NOT DISTINCT FROM o.version_id AND d.role = o.role`, m), s.tenant, from, into); err != nil {
		return r, err
	}
	tag, err := q.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s (tenant_id, entity_id, content_id, version_id, role, position, state)
 SELECT tenant_id, $3, content_id, version_id, role, position, state FROM %[1]s WHERE tenant_id = $1 AND entity_id = $2
 ON CONFLICT (tenant_id, entity_id, content_id, version_id, role) DO NOTHING`, m), s.tenant, from, into)
	if err != nil {
		return r, err
	}
	r.MembershipsMoved = tag.RowsAffected()
	if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND entity_id = $2`, m), s.tenant, from); err != nil {
		return r, err
	}
	names := s.table("content_entity_names")
	if tag, err = q.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s (tenant_id, entity_id, language, kind, name)
 SELECT tenant_id, $3, language, 'alias', name FROM %[1]s WHERE tenant_id = $1 AND entity_id = $2
 ON CONFLICT (tenant_id, entity_id, language, normalized) DO NOTHING`, names), s.tenant, from, into); err != nil {
		return r, err
	}
	r.NamesMoved = tag.RowsAffected()
	if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND entity_id = $2`, names), s.tenant, from); err != nil {
		return r, err
	}
	text := s.table("content_entity_text")
	if _, err := q.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s (tenant_id, entity_id, language, description, sort_key)
 SELECT tenant_id, $3, language, description, sort_key FROM %[1]s WHERE tenant_id = $1 AND entity_id = $2
 ON CONFLICT (tenant_id, entity_id, language) DO NOTHING`, text), s.tenant, from, into); err != nil {
		return r, err
	}
	edges := s.table("content_entity_edges")
	if tag, err = q.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s (tenant_id, from_id, relation, to_id)
 SELECT tenant_id, CASE WHEN from_id = $2 THEN $3 ELSE from_id END, relation, CASE WHEN to_id = $2 THEN $3 ELSE to_id END
 FROM %[1]s WHERE tenant_id = $1 AND (from_id = $2 OR to_id = $2) AND from_id <> $3 AND to_id <> $3
 ON CONFLICT DO NOTHING`, edges), s.tenant, from, into); err != nil {
		return r, err
	}
	r.EdgesMoved = tag.RowsAffected()
	if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND (from_id = $2 OR to_id = $2)`, edges), s.tenant, from); err != nil {
		return r, err
	}
	g := s.table("content_entities")
	// Into may sit under From; it takes From's place.
	if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET parent_id = (SELECT parent_id FROM %[1]s WHERE tenant_id = $1 AND id = $2)
 WHERE tenant_id = $1 AND id = $3 AND parent_id = $2`, g), s.tenant, from, into); err != nil {
		return r, err
	}
	if tag, err = q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET parent_id = CASE WHEN parent_id = $2 THEN $3 ELSE parent_id END,
 scope_id = CASE WHEN scope_id = $2 THEN $3 ELSE scope_id END, updated_at = now(), revision = revision + 1
 WHERE tenant_id = $1 AND (parent_id = $2 OR scope_id = $2) AND id <> $3`, g), s.tenant, from, into); err != nil {
		return r, err
	}
	r.EntitiesMoved = tag.RowsAffected()
	if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET merged_into = $3, updated_at = now() WHERE tenant_id = $1 AND merged_into = $2`, g), s.tenant, from, into); err != nil {
		return r, err
	}
	if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET state = 'merged', merged_into = $3, updated_at = now(), revision = revision + 1 WHERE tenant_id = $1 AND id = $2`, g), s.tenant, from, into); err != nil {
		return r, err
	}
	if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET updated_at = now(), revision = revision + 1 WHERE tenant_id = $1 AND id = $2`, g), s.tenant, into); err != nil {
		return r, err
	}
	if err := s.syncCodes(ctx, q, []string{from, into}); err != nil {
		return r, err
	}
	return r, codes.Merge(ctx, codes.Pgx(q), s.qs, s.tenant, codes.Key{Kind: src.Kind, ID: from}, codes.Key{Kind: dst.Kind, ID: into})
}
