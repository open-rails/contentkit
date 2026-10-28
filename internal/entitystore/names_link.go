package entitystore

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	entities "github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/codes"
	"github.com/open-rails/contentkit/kinds"
)

// LockNames serializes name resolution in this catalog, so concurrent writers
// that find no match never mint the same entity twice.
func (s *Store) LockNames(ctx context.Context, q Querier) error {
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "contentkit:groups:names:"+s.tenant)
	return err
}

// MatchName returns at most two active entities of kind within scope whose
// canonical name or alias matches name, ignoring case, accents and word
// separators; in language only when it is set.
func (s *Store) MatchName(ctx context.Context, q Querier, kind, scopeID, language, name string) ([]string, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT DISTINCT g.id FROM %[1]s g JOIN %[2]s n ON n.tenant_id=g.tenant_id AND n.entity_id=g.id
 WHERE g.tenant_id=$1 AND g.kind=$2 AND g.scope_id=$3 AND g.state='active' AND ($4='' OR n.language=$4)
   AND regexp_replace(n.normalized,'%[3]s','','g')=regexp_replace(%[4]s.contentkit_keyword_normalize($5),'%[3]s','','g')
 ORDER BY g.id LIMIT 2`, s.table("content_entities"), s.table("content_entity_names"), entities.NameSeparators, s.qs), s.tenant, kind, scopeID, language, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// linkNames resolves set.Create in q, creating missing entities of creatable
// kinds, and adds a membership in the set's ownership scope for each.
// The caller holds the content's lock; this takes the catalog's name lock.
func (s *Store) linkNames(ctx context.Context, q pgx.Tx, contentKind string, set *entities.ContentEntities) error {
	if len(set.Create) == 0 {
		return nil
	}
	if err := s.LockNames(ctx, q); err != nil {
		return err
	}
	declared := s.cfg.Kinds[contentKind].Entities
	// Scoping kinds first: a new character can sit under a series named alongside it.
	in := slices.Clone(set.Create)
	slices.SortStableFunc(in, func(a, b entities.NewEntity) int {
		return boolRank(s.cfg.EntityKinds[a.Kind].ScopedBy != "") - boolRank(s.cfg.EntityKinds[b.Kind].ScopedBy != "")
	})
	for _, ne := range in {
		name := strings.TrimSpace(ne.Name)
		gk, ok := s.cfg.EntityKinds[ne.Kind]
		switch {
		case name == "":
			return fmt.Errorf("%w: a new %s needs a name", entities.ErrInvalid, ne.Kind)
		case !ok || !slices.Contains(declared, ne.Kind) || len(set.Kinds) > 0 && !slices.Contains(set.Kinds, ne.Kind):
			return fmt.Errorf("%w: %s content does not join %s entities", entities.ErrInvalid, contentKind, ne.Kind)
		}
		scopeID := ""
		if gk.ScopedBy != "" {
			var err error
			if scopeID, err = s.soleScope(ctx, q, gk.ScopedBy, set.Memberships); err != nil {
				return fmt.Errorf("%w: %s %q: %w", entities.ErrInvalid, ne.Kind, name, err)
			}
		}
		ids, err := s.MatchName(ctx, q, ne.Kind, scopeID, "", name)
		if err != nil {
			return err
		}
		var id string
		switch len(ids) {
		case 2:
			return fmt.Errorf("%w: several %s entities are named %q; pick one", ErrConflict, ne.Kind, name)
		case 1:
			id = ids[0]
		default:
			if id, err = s.createNamed(ctx, q, ne.Kind, gk, scopeID, name, ne.Language); err != nil {
				return err
			}
		}
		version := ""
		if set.VersionScope != nil {
			version = *set.VersionScope
		}
		if !slices.ContainsFunc(set.Memberships, func(m entities.Membership) bool {
			return m.EntityID == entities.EntityID(id) && m.VersionID == version && m.Role == ""
		}) {
			set.Memberships = append(set.Memberships, entities.Membership{EntityID: entities.EntityID(id), ContentID: set.ContentID, VersionID: version})
		}
	}
	return nil
}

// createNamed creates one entity of a creatable kind; required and filter
// entities, user-owned and authorization kinds are created deliberately only.
func (s *Store) createNamed(ctx context.Context, q pgx.Tx, kind string, gk kinds.EntityKind, scopeID, name, language string) (string, error) {
	if !gk.Creatable || gk.Owner == "user" || gk.AuthScope {
		return "", fmt.Errorf("%w: no %s is named %q, and content edits do not create %s entities", entities.ErrInvalid, kind, name, kind)
	}
	slug := codes.Slugify(name)
	for _, d := range s.cfg.ContentFilters.Domains {
		for _, f := range d.Filters {
			if f.Entity == (kinds.EntityRef{Kind: kind, Slug: slug}) {
				return "", fmt.Errorf("%w: %s %q is a configured filter; create it in entity management", entities.ErrInvalid, kind, name)
			}
		}
		if d.Requires(kinds.EntityRef{Kind: kind, Slug: slug}) {
			return "", fmt.Errorf("%w: %s %q is a required entity; create it in entity management", entities.ErrInvalid, kind, name)
		}
	}
	if slug == "" {
		slug = kind + "-" + uuid.NewString()[:8]
	}
	var taken bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("content_entities")+` WHERE tenant_id=$1 AND kind=$2 AND scope_id=$3 AND slug=$4)`,
		s.tenant, kind, scopeID, slug).Scan(&taken); err != nil {
		return "", err
	}
	if taken {
		slug += "-" + uuid.NewString()[:8]
	}
	if language == "" && len(s.cfg.Languages) > 0 {
		language = s.cfg.Languages[0]
	}
	made, err := s.bound(q).CreateEntities(ctx, []entities.EntityInput{{Kind: kind, Slug: slug, ScopeID: entities.EntityID(scopeID), Names: []entities.Name{{Language: language, Kind: entities.NameCanonical, Name: name}}}})
	if err != nil {
		return "", err
	}
	return string(made[0].ID), nil
}

// soleScope is the one entity of scopeKind among the memberships.
func (s *Store) soleScope(ctx context.Context, q Querier, scopeKind string, ms []entities.Membership) (string, error) {
	ids := []string{}
	for _, m := range ms {
		if id := string(m.EntityID); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	rows, err := q.Query(ctx, `SELECT id FROM `+s.table("content_entities")+` WHERE tenant_id=$1 AND kind=$2 AND id=ANY($3::text[]) AND state='active'`, s.tenant, scopeKind, ids)
	if err != nil {
		return "", err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	if len(found) != 1 {
		return "", fmt.Errorf("link exactly one %s first", scopeKind)
	}
	return found[0], nil
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
