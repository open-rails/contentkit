package catalog

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/search"
)

// ManagementName is the current name of an item its caller may edit. No body,
// media, public-listing policy or audience billing check is needed to navigate.
type ManagementName struct{ ID, Kind, Title, Language string }

type ManagementNames struct {
	Contents []ManagementName
	Entities []entities.EntityCard
}

type ManagementSearchQuery struct {
	Query, Language                  string
	ContentKinds, EntityKinds        []string
	IncludeContents, IncludeEntities bool
	Limit                            int
}

// ManagementSearch uses current operator text, not public approved snapshots.
// Write/creator/owner-entity eligibility runs inside every indexed SQL query
// before its limit; entity names require the same authority as /manage/entities.
func (s *Store) ManagementSearch(ctx context.Context, actor access.Actor, q ManagementSearchQuery) (ManagementNames, error) {
	out := ManagementNames{Contents: []ManagementName{}, Entities: []entities.EntityCard{}}
	if actor.Anonymous || actor.ID == "" {
		return out, ErrForbidden
	}
	if !q.IncludeContents && !q.IncludeEntities {
		return out, invalid("select include_contents or include_entities")
	}
	if q.Limit == 0 {
		q.Limit = 8
	}
	if q.Limit < 1 || q.Limit > 20 {
		return out, invalid("limit must be between 1 and 20")
	}
	if q.Language == "" {
		q.Language = s.defaultLanguage()
	}
	if err := s.checkLanguage(q.Language); err != nil {
		return out, err
	}
	validateKinds := func(names []string, entity bool) ([]string, error) {
		names = slices.Clone(names)
		if len(names) == 0 {
			if entity {
				for name := range s.cfg.EntityKinds {
					names = append(names, name)
				}
			} else {
				for name := range s.cfg.Kinds {
					names = append(names, name)
				}
			}
		}
		for i, name := range names {
			name = strings.TrimSpace(name)
			if entity {
				if _, ok := s.cfg.EntityKinds[name]; !ok {
					return nil, invalid("entity kind %q is not configured", name)
				}
			} else if _, ok := s.cfg.Kinds[name]; !ok {
				return nil, invalid("content kind %q is not configured", name)
			}
			names[i] = name
		}
		slices.Sort(names)
		return slices.Compact(names), nil
	}
	var err error
	if q.IncludeContents {
		if q.ContentKinds, err = validateKinds(q.ContentKinds, false); err != nil {
			return out, err
		}
	}
	if q.IncludeEntities {
		if q.EntityKinds, err = validateKinds(q.EntityKinds, true); err != nil {
			return out, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rights, err := s.staffRights(ctx, actor)
	if err != nil {
		return out, err
	}
	canContents := q.IncludeContents && (rights.write || actor.Member())
	canEntities := false
	if q.IncludeEntities {
		canEntities, err = gate.Can(ctx, s.guard.Auth, actor, string(access.EntitiesWrite))
		if err != nil {
			return out, &AuthError{err}
		}
	}
	if !canContents && !canEntities {
		return out, ErrForbidden
	}
	if strings.TrimSpace(q.Query) == "" {
		return out, nil
	}
	client, err := hub.NewClient(hub.ClientConfig{Pool: s.pool, Schema: s.schema, Tenant: s.tenant, DefaultLanguage: s.defaultLanguage()})
	if err != nil {
		return out, err
	}
	mode := hub.LanguageModeExact
	if canContents && len(q.ContentKinds) > 0 {
		var editorEntities []string
		if !rights.write {
			if source, ok := s.entities.(interface {
				ContentEditorEntities(context.Context, auth.Authenticator, access.Actor) ([]entities.EntityID, error)
			}); ok {
				ids, err := source.ContentEditorEntities(ctx, s.guard.Auth, actor)
				if err != nil {
					return out, err
				}
				for _, id := range ids {
					editorEntities = append(editorEntities, string(id))
				}
			}
		}
		own, err := s.ownAccess(ctx, actor, false)
		if err != nil {
			return out, err
		}
		args := pgx.NamedArgs{"ms_staff": rights.write, "ms_account": actor.AccountID(), "ms_groups": editorEntities, "ms_own_app": own.app, "ms_own_channels": own.entities}
		allowed := `(@ms_staff OR (c.created_by=@ms_account AND @ms_account<>'' AND ((c.owner_entity_id IS NULL AND @ms_own_app::boolean) OR c.owner_entity_id=ANY(@ms_own_channels::text[])))
 OR EXISTS (SELECT 1 FROM ` + s.t.entities + ` og WHERE og.tenant_id=c.tenant_id AND og.id=c.owner_entity_id AND og.state='active' AND og.auth_scope IS NULL AND og.owner_user_id=@ms_account AND @ms_account<>'')
 OR c.owner_entity_id=ANY(@ms_groups::text[]))`
		eligible := &search.Eligibility{SQL: `SELECT 0 AS priority WHERE EXISTS (SELECT 1 FROM ` + s.t.items + ` c
 WHERE c.tenant_id=sd.tenant_id AND c.id=sd.content_id AND c.kind=sd.content_kind AND c.state<>'deleted' AND ` + allowed + `)`, Args: args}
		hits, err := client.Typeahead(ctx, q.Query, hub.TypeaheadOptions{Language: q.Language, LanguageMode: mode, ContentKinds: q.ContentKinds, Limit: q.Limit, Eligibility: eligible, Source: search.CurrentContentNames})
		if err != nil {
			return out, err
		}
		ids := make([]string, 0, len(hits))
		for _, hit := range hits {
			ids = append(ids, hit.ContentID)
		}
		if len(ids) > 0 {
			args["ms_ids"], args["ms_tenant"], args["ms_language"], args["ms_default"] = ids, s.tenant, q.Language, s.defaultLanguage()
			titleKinds := []string{}
			for kind, spec := range s.cfg.Kinds {
				if spec.TitlePolicy == kinds.TitleOriginalAndVersion {
					titleKinds = append(titleKinds, kind)
				}
			}
			args["ms_title_kinds"] = titleKinds
			rows, err := s.pool.Query(ctx, `SELECT c.id,c.kind,coalesce(vt.title,''),coalesce(vt.language,'') FROM `+s.t.items+` c
 LEFT JOIN LATERAL (SELECT coalesce(nullif(t.title,''),CASE WHEN c.kind=ANY(@ms_title_kinds::text[]) THEN nullif(v.original_title,'') END,'') title,
 coalesce(t.language,v.languages[1],'') language FROM `+s.t.versions+` v LEFT JOIN `+s.t.versionText+` t ON t.tenant_id=v.tenant_id AND t.version_id=v.id
 WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.state<>'deleted'
 ORDER BY coalesce(@ms_language=ANY(v.languages),false) DESC,v.released DESC,v.position,t.language=@ms_language DESC,t.language LIMIT 1) vt ON true
 WHERE c.tenant_id=@ms_tenant AND c.id=ANY(@ms_ids::text[]) AND c.state<>'deleted' AND `+allowed+`
 ORDER BY array_position(@ms_ids::text[],c.id)`, args)
			if err != nil {
				return out, err
			}
			for rows.Next() {
				var name ManagementName
				if err = rows.Scan(&name.ID, &name.Kind, &name.Title, &name.Language); err != nil {
					rows.Close()
					return out, err
				}
				out.Contents = append(out.Contents, name)
			}
			rows.Close()
			if err = rows.Err(); err != nil {
				return out, err
			}
		}
	}
	if canEntities && len(q.EntityKinds) > 0 {
		hits, err := client.Typeahead(ctx, q.Query, hub.TypeaheadOptions{Language: q.Language, LanguageMode: mode, ContentKinds: q.EntityKinds, Limit: q.Limit, Source: search.CurrentEntityNames,
			Eligibility: &search.Eligibility{SQL: `SELECT 0 AS priority WHERE EXISTS (SELECT 1 FROM ` + s.t.entities + ` g WHERE g.tenant_id=sd.tenant_id AND g.id=sd.content_id AND g.kind=sd.content_kind AND g.state='active')`}})
		if err != nil {
			return out, err
		}
		ids := make([]entities.EntityID, 0, len(hits))
		rank := map[entities.EntityID]int{}
		for i, hit := range hits {
			id := entities.EntityID(hit.ContentID)
			ids = append(ids, id)
			rank[id] = i
		}
		if len(ids) > 0 {
			if s.entities == nil {
				return out, fmt.Errorf("catalog: entity management is not configured")
			}
			out.Entities, err = s.entities.Cards(ctx, ids, entities.CardOptions{Language: q.Language, IncludeInternal: true})
			if err != nil {
				return out, err
			}
			slices.SortFunc(out.Entities, func(a, b entities.EntityCard) int { return rank[a.ID] - rank[b.ID] })
		}
	}
	return out, nil
}
