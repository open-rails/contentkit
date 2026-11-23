package contentstore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/locationpolicy"
	"github.com/open-rails/contentkit/kinds"
	"golang.org/x/text/language"
)

func (rt *Runtime) locationTable(name string) string {
	return pgx.Identifier{rt.schema, "content_location_" + name}.Sanitize()
}

var subdivisionCode = regexp.MustCompile(`^[A-Z]{2}-[A-Z0-9]{1,3}$`)

func validCountry(s string) bool {
	r, err := language.ParseRegion(s)
	return err == nil && len(s) == 2 && r.IsCountry() && r.String() == s
}

func (rt *Runtime) validateLocationRule(ctx context.Context, tx pgx.Tx, in *content.LocationRuleInput) error {
	if len(in.Locations) == 0 || len(in.Locations) > maxRuleBatch {
		return badRequest("locations needs 1-%d entries", maxRuleBatch)
	}
	for i, e := range in.Locations {
		n := 0
		if e.Country != "" {
			n++
			if !validCountry(e.Country) {
				return badRequest("country must be an ISO 3166-1 alpha-2 code")
			}
		}
		if e.Subdivision != "" {
			n++
			if !subdivisionCode.MatchString(e.Subdivision) || !validCountry(e.Subdivision[:2]) {
				return badRequest("subdivision must be an ISO 3166-2 code")
			}
		}
		if e.Network != "" {
			n++
			p, err := network(e.Network)
			if err != nil {
				return err
			}
			in.Locations[i].Network = p.String()
		}
		if n != 1 {
			return badRequest("each location needs exactly one of country, subdivision or network")
		}
	}
	if len(in.Domains) == 0 || len(in.Domains) > 100 {
		return badRequest("domains needs 1-100 configured domain names")
	}
	for _, name := range in.Domains {
		if !slices.ContainsFunc(rt.cfg.ContentFilters.Domains, func(d kinds.Domain) bool { return d.Name == name }) && !(name == "main" && len(rt.cfg.ContentFilters.Domains) == 0) {
			return badRequest("unknown domain %q", name)
		}
	}
	if len(in.ContentIDs) > maxRuleBatch || len(in.EntityIDs) > maxRuleBatch {
		return badRequest("at most %d target ids", maxRuleBatch)
	}
	switch in.Target {
	case "site":
		if len(in.ContentIDs) != 0 || len(in.EntityIDs) != 0 {
			return badRequest("site target has no content or entity ids")
		}
	case "content":
		if len(in.ContentIDs) == 0 || len(in.EntityIDs) != 0 {
			return badRequest("content target needs content ids only")
		}
	case "entities":
		if len(in.EntityIDs) == 0 || len(in.ContentIDs) != 0 {
			return badRequest("entities target needs entity ids only")
		}
	default:
		return badRequest("target must be site, content or entities")
	}
	for _, target := range []struct {
		table string
		ids   []string
	}{{"content_items", in.ContentIDs}, {"content_entities", in.EntityIDs}} {
		if len(target.ids) == 0 {
			continue
		}
		var exists bool
		err := tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM unnest($2::text[]) wanted(id) WHERE NOT EXISTS (SELECT 1 FROM `+pgx.Identifier{rt.schema, target.table}.Sanitize()+` t WHERE t.tenant_id=$1 AND t.id=wanted.id))`, rt.tenant, target.ids).Scan(&exists)
		if err != nil {
			return err
		}
		if !exists {
			return badRequest("target ids must exist in this tenant")
		}
	}
	in.Note = strings.TrimSpace(in.Note)
	if utf8.RuneCountInString(in.Note) > 2000 {
		return badRequest("note exceeds 2000 characters")
	}
	in.ContentIDs = dedup(in.ContentIDs)
	in.EntityIDs = dedup(in.EntityIDs)
	in.Domains = dedup(in.Domains)
	slices.Sort(in.ContentIDs)
	slices.Sort(in.EntityIDs)
	slices.Sort(in.Domains)
	return nil
}

func (rt *Runtime) locationSelect() string {
	return `SELECT r.id,r.target,
 ARRAY(SELECT c.content_id FROM ` + rt.locationTable("rule_contents") + ` c WHERE c.tenant_id=r.tenant_id AND c.rule_id=r.id ORDER BY c.content_id),
 ARRAY(SELECT e.entity_id FROM ` + rt.locationTable("rule_entities") + ` e WHERE e.tenant_id=r.tenant_id AND e.rule_id=r.id ORDER BY e.entity_id),
 ARRAY(SELECT d.domain FROM ` + rt.locationTable("rule_domains") + ` d WHERE d.tenant_id=r.tenant_id AND d.rule_id=r.id ORDER BY d.domain),r.enabled,r.note,r.created_by,r.created_at,r.updated_by,r.updated_at,
 coalesce((SELECT jsonb_agg(jsonb_strip_nulls(jsonb_build_object('country',e.country,'subdivision',e.subdivision,'network',e.network::text)) ORDER BY e.position) FROM ` + rt.locationTable("rule_entries") + ` e WHERE e.tenant_id=r.tenant_id AND e.rule_id=r.id),'[]'::jsonb)
 FROM ` + rt.locationTable("rules") + ` r`
}
func scanLocationRule(row pgx.Row) (content.LocationRule, error) {
	var r content.LocationRule
	var raw []byte
	err := row.Scan(&r.ID, &r.Target, &r.ContentIDs, &r.EntityIDs, &r.Domains, &r.Enabled, &r.Note, &r.CreatedBy, &r.CreatedAt, &r.UpdatedBy, &r.UpdatedAt, &raw)
	if err == nil {
		err = json.Unmarshal(raw, &r.Locations)
	}
	return r, err
}
func (rt *Runtime) locationRule(ctx context.Context, tx pgx.Tx, id string) (content.LocationRule, error) {
	// Lock before reading child rows so a waiting patch sees the prior save's
	// committed targets, domains and entries in a fresh READ COMMITTED snapshot.
	var lockedID string
	err := tx.QueryRow(ctx, `SELECT id FROM `+rt.locationTable("rules")+` WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, rt.tenant, id).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return content.LocationRule{}, ErrNotFound
	}
	if err != nil {
		return content.LocationRule{}, err
	}
	return scanLocationRule(tx.QueryRow(ctx, rt.locationSelect()+` WHERE r.tenant_id=$1 AND r.id=$2`, rt.tenant, id))
}
func (rt *Runtime) locationRules(ctx context.Context, limit, offset int) (content.LocationRulePage, error) {
	page := content.LocationRulePage{Items: []content.LocationRule{}, Domains: []string{}}
	for _, d := range rt.cfg.ContentFilters.Domains {
		page.Domains = append(page.Domains, d.Name)
	}
	if len(page.Domains) == 0 {
		page.Domains = []string{"main"}
	}
	err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.locationTable("rules")+` WHERE tenant_id=$1`, rt.tenant).Scan(&page.Total)
	if err != nil {
		return page, err
	}
	rows, err := rt.store.pool.Query(ctx, rt.locationSelect()+` WHERE r.tenant_id=$1 ORDER BY r.created_at DESC,r.id DESC LIMIT $2 OFFSET $3`, rt.tenant, limit, offset)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanLocationRule(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, r)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if err := rt.locationTargets(ctx, rt.store.pool, page.Items); err != nil {
		return page, err
	}
	ids := make([]string, 0, len(page.Items))
	for _, r := range page.Items {
		ids = append(ids, r.UpdatedBy)
	}
	names := rt.locationActorNames(ctx, ids)
	for i := range page.Items {
		page.Items[i].UpdatedByName = names[page.Items[i].UpdatedBy]
	}
	return page, nil
}

func (rt *Runtime) locationActorNames(ctx context.Context, ids []string) map[string]string {
	if rt.userInfo == nil {
		return nil
	}
	ids = dedup(slices.DeleteFunc(ids, func(id string) bool { return id == "" }))
	if len(ids) == 0 {
		return nil
	}
	users, err := rt.userInfo.Get(ctx, ids)
	if err != nil {
		rt.log.WarnContext(ctx, "content: location actors lookup failed", "err", err.Error())
		return nil
	}
	names := make(map[string]string, len(ids))
	for _, id := range ids {
		u := users[id]
		names[id] = cmp.Or(u.Name, u.Username)
	}
	return names
}

// locationTargets reads staff labels directly, without visitor visibility rules.
func (rt *Runtime) locationTargets(ctx context.Context, q querier, rules []content.LocationRule) error {
	contentIDs, entityIDs := []string{}, []string{}
	for _, r := range rules {
		contentIDs = append(contentIDs, r.ContentIDs...)
		entityIDs = append(entityIDs, r.EntityIDs...)
	}
	contents, entities := map[string]content.LocationRuleTarget{}, map[string]content.LocationRuleTarget{}
	language := rt.cfg.Languages[0]
	for _, batch := range []struct {
		ids     []string
		sql     string
		targets map[string]content.LocationRuleTarget
	}{
		{contentIDs, `SELECT c.id,c.kind,coalesce(label.title,''),'' FROM ` + rt.store.t.items + ` c
 LEFT JOIN LATERAL (SELECT t.title FROM ` + pgx.Identifier{rt.schema, "content_versions"}.Sanitize() + ` v
 JOIN ` + pgx.Identifier{rt.schema, "content_version_text"}.Sanitize() + ` t ON t.tenant_id=v.tenant_id AND t.version_id=v.id
 WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.state<>'deleted' AND t.title<>''
 ORDER BY (t.language=$3) DESC,v.position,v.id,t.language LIMIT 1) label ON true
 WHERE c.tenant_id=$1 AND c.id=ANY($2::text[]) AND c.state<>'deleted'`, contents},
		{entityIDs, `SELECT e.id,e.kind,'',coalesce(label.name,'') FROM ` + rt.store.t.entities + ` e
 LEFT JOIN LATERAL (SELECT n.name FROM ` + pgx.Identifier{rt.schema, "content_entity_names"}.Sanitize() + ` n
 WHERE n.tenant_id=e.tenant_id AND n.entity_id=e.id AND n.kind='name'
 ORDER BY (n.language=$3) DESC,n.language LIMIT 1) label ON true
 WHERE e.tenant_id=$1 AND e.id=ANY($2::text[]) AND e.state<>'deleted'`, entities},
	} {
		if len(batch.ids) == 0 {
			continue
		}
		rows, err := q.Query(ctx, batch.sql, rt.tenant, batch.ids, language)
		if err != nil {
			return err
		}
		for rows.Next() {
			var target content.LocationRuleTarget
			if err := rows.Scan(&target.ID, &target.Kind, &target.Title, &target.Name); err != nil {
				rows.Close()
				return err
			}
			batch.targets[target.ID] = target
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for i := range rules {
		r := &rules[i]
		r.Targets = []content.LocationRuleTarget{}
		ids, targets := r.ContentIDs, contents
		if r.Target == "entities" {
			ids, targets = r.EntityIDs, entities
		}
		for _, id := range ids {
			if target, ok := targets[id]; ok {
				r.Targets = append(r.Targets, target)
			}
		}
	}
	return nil
}
func (rt *Runtime) saveLocationRule(ctx context.Context, by, id string, in *content.LocationRuleInput, patch *content.LocationRuleUpdate, remove bool) (content.LocationRule, error) {
	var r content.LocationRule
	tx, err := rt.store.beginMutation(ctx)
	if err != nil {
		return r, err
	}
	defer tx.Rollback(ctx)
	if err = rt.lockErasureSubject(ctx, tx, by); err != nil {
		return r, err
	}
	if by, err = rt.provenance(ctx, tx, by); err != nil {
		return r, err
	}
	action := "create"
	if id != "" {
		r, err = rt.locationRule(ctx, tx, id)
		if err != nil {
			return r, err
		}
		action = "update"
		if remove {
			action = "delete"
		} else {
			oldEnabled := r.Enabled
			if patch.Locations != nil {
				r.Locations = *patch.Locations
			}
			if patch.Target != nil {
				r.Target = *patch.Target
			}
			if patch.ContentIDs != nil {
				r.ContentIDs = *patch.ContentIDs
			}
			if patch.EntityIDs != nil {
				r.EntityIDs = *patch.EntityIDs
			}
			if patch.Domains != nil {
				r.Domains = *patch.Domains
			}
			if patch.Enabled != nil {
				r.Enabled = *patch.Enabled
			}
			if patch.Note != nil {
				r.Note = *patch.Note
			}
			if oldEnabled != r.Enabled {
				if r.Enabled {
					action = "enable"
				} else {
					action = "disable"
				}
			}
		}
	} else {
		r.ID = contentref.NewID()
		r.LocationRuleInput = *in
	}
	if remove {
		_, err = tx.Exec(ctx, `DELETE FROM `+rt.locationTable("rules")+` WHERE tenant_id=$1 AND id=$2`, rt.tenant, r.ID)
	} else {
		if err = rt.validateLocationRule(ctx, tx, &r.LocationRuleInput); err != nil {
			return r, err
		}
		err = tx.QueryRow(ctx, `INSERT INTO `+rt.locationTable("rules")+` (tenant_id,id,target,enabled,note,created_by,updated_by)
 VALUES ($1,$2,$3,$4,$5,$6,$6) ON CONFLICT (tenant_id,id) DO UPDATE SET target=EXCLUDED.target,enabled=EXCLUDED.enabled,note=EXCLUDED.note,updated_by=EXCLUDED.updated_by,updated_at=now()
 RETURNING created_by,created_at,updated_by,updated_at`, rt.tenant, r.ID, r.Target, r.Enabled, r.Note, by).Scan(&r.CreatedBy, &r.CreatedAt, &r.UpdatedBy, &r.UpdatedAt)
		if err != nil {
			return r, err
		}
		for _, rows := range []struct {
			table, column string
			values        []string
		}{
			{"rule_domains", "domain", r.Domains},
			{"rule_contents", "content_id", r.ContentIDs},
			{"rule_entities", "entity_id", r.EntityIDs},
		} {
			table := rt.locationTable(rows.table)
			if _, err = tx.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id=$1 AND rule_id=$2`, rt.tenant, r.ID); err != nil {
				return r, err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO `+table+` (tenant_id,rule_id,`+rows.column+`) SELECT $1,$2,value FROM unnest($3::text[]) value`, rt.tenant, r.ID, rows.values); err != nil {
				return r, err
			}
		}
		if _, err = tx.Exec(ctx, `DELETE FROM `+rt.locationTable("rule_entries")+` WHERE tenant_id=$1 AND rule_id=$2`, rt.tenant, r.ID); err != nil {
			return r, err
		}
		for i, e := range r.Locations {
			_, err = tx.Exec(ctx, `INSERT INTO `+rt.locationTable("rule_entries")+` (tenant_id,rule_id,position,country,subdivision,network) VALUES ($1,$2,$3,nullif($4,''),nullif($5,''),nullif($6,'')::cidr)`, rt.tenant, r.ID, i, e.Country, e.Subdivision, e.Network)
			if err != nil {
				return r, err
			}
		}
	}
	if err != nil {
		return r, err
	}
	snapshot, err := json.Marshal(struct {
		content.LocationRule
		Targets       []content.LocationRuleTarget `json:"targets,omitempty"`
		UpdatedByName string                       `json:"updated_by_name,omitempty"`
	}{LocationRule: r})
	if err != nil {
		return r, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO `+rt.locationTable("rule_history")+` (tenant_id,rule_id,action,actor,snapshot) VALUES ($1,$2,$3,$4,$5::jsonb)`, rt.tenant, r.ID, action, by, snapshot); err != nil {
		return r, err
	}
	if !remove {
		rules := []content.LocationRule{r}
		if err := rt.locationTargets(ctx, tx, rules); err != nil {
			return r, err
		}
		r = rules[0]
	}
	if err := tx.Commit(ctx); err != nil {
		return r, err
	}
	if !remove {
		r.UpdatedByName = rt.locationActorNames(ctx, []string{r.UpdatedBy})[r.UpdatedBy]
	}
	return r, nil
}
func (rt *Runtime) locationHistory(ctx context.Context, id string, limit, offset int) (content.LocationRuleHistoryPage, error) {
	page := content.LocationRuleHistoryPage{Items: []content.LocationRuleHistory{}}
	where := ` WHERE tenant_id=$1 AND ($2='' OR rule_id=$2)`
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.locationTable("rule_history")+where, rt.tenant, id).Scan(&page.Total); err != nil {
		return page, err
	}
	rows, err := rt.store.pool.Query(ctx, `SELECT id,rule_id,action,actor,at,snapshot FROM `+rt.locationTable("rule_history")+where+` ORDER BY id DESC LIMIT $3 OFFSET $4`, rt.tenant, id, limit, offset)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var h content.LocationRuleHistory
		if err := rows.Scan(&h.ID, &h.RuleID, &h.Action, &h.Actor, &h.At, &h.Snapshot); err != nil {
			return page, err
		}
		page.Items = append(page.Items, h)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	ids := make([]string, 0, len(page.Items))
	for _, h := range page.Items {
		ids = append(ids, h.Actor)
	}
	names := rt.locationActorNames(ctx, ids)
	for i := range page.Items {
		page.Items[i].ActorName = names[page.Items[i].Actor]
	}
	return page, nil
}
func (rt *Runtime) checkLocation(ctx context.Context, ip netip.Addr) (content.LocationCheck, error) {
	l := content.VisitorLocation{}
	if rt.locate != nil {
		l = rt.locate(ctx, ip)
	}
	out := content.LocationCheck{Location: l, Domains: map[string][]content.LocationRuleMatch{}}
	ctx = locationpolicy.WithLocation(ctx, l)
	names := []string{}
	for _, d := range rt.cfg.ContentFilters.Domains {
		names = append(names, d.Name)
	}
	if len(names) == 0 {
		names = []string{"main"}
	}
	for _, name := range names {
		b, err := locationpolicy.Resolve(ctx, rt.store.pool, rt.schema, rt.tenant, name, ip.String())
		if err != nil {
			return out, err
		}
		out.Domains[name] = b.Matches
	}
	return out, nil
}

func locationRoute(method, path, doc string, request any, reply httpapi.Reply, query []httpapi.Param, serve func(*Runtime, access.Actor, *http.Request) (any, error)) httpapi.Route[*Runtime] {
	return httpapi.Route[*Runtime]{Spec: httpapi.Spec{Method: method, Path: path, Resource: "locations", Auth: httpapi.Staff, Perm: string(access.LocationsWrite), Doc: doc, Request: request, Responses: []httpapi.Reply{reply}, Query: query, Errors: []string{httpapi.CodeNotFound}}, Serve: func(rt *Runtime) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			actor := rt.actor(req.Context())
			if err := rt.requirePerm(req.Context(), actor, access.LocationsWrite); err != nil {
				writeErr(w, err)
				return
			}
			out, err := serve(rt, actor, req)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, reply.Status, out)
		}
	}}
}

var locationRoutes = []httpapi.Route[*Runtime]{
	locationRoute(httpapi.GET, "/moderation/location-rules", "Lists location rules, newest first.", nil, httpapi.OK(content.LocationRulePage{}), httpapi.Page, func(rt *Runtime, _ access.Actor, r *http.Request) (any, error) {
		limit, offset := parsePage(r)
		return rt.locationRules(r.Context(), limit, offset)
	}),
	locationRoute(httpapi.POST, "/moderation/location-rules", "Creates a location rule and its history snapshot.", content.LocationRuleInput{}, httpapi.Created(content.LocationRule{}), nil, func(rt *Runtime, a access.Actor, r *http.Request) (any, error) {
		var in content.LocationRuleInput
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		return rt.saveLocationRule(r.Context(), a.ID, "", &in, nil, false)
	}),
	locationRoute(httpapi.PATCH, "/moderation/location-rules/{id}", "Updates a location rule, including its enabled state.", content.LocationRuleUpdate{}, httpapi.OK(content.LocationRule{}), nil, func(rt *Runtime, a access.Actor, r *http.Request) (any, error) {
		var in content.LocationRuleUpdate
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		return rt.saveLocationRule(r.Context(), a.ID, r.PathValue("id"), nil, &in, false)
	}),
	locationRoute(httpapi.DELETE, "/moderation/location-rules/{id}", "Deletes a location rule, retaining its history.", nil, httpapi.NoContent, nil, func(rt *Runtime, a access.Actor, r *http.Request) (any, error) {
		_, err := rt.saveLocationRule(r.Context(), a.ID, r.PathValue("id"), nil, nil, true)
		return nil, err
	}),
	locationRoute(httpapi.GET, "/moderation/location-rules/history", "Lists all location rule history, newest first.", nil, httpapi.OK(content.LocationRuleHistoryPage{}), httpapi.Page, func(rt *Runtime, _ access.Actor, r *http.Request) (any, error) {
		limit, offset := parsePage(r)
		return rt.locationHistory(r.Context(), "", limit, offset)
	}),
	locationRoute(httpapi.GET, "/moderation/location-rules/{id}/history", "Lists a rule's history, including after deletion.", nil, httpapi.OK(content.LocationRuleHistoryPage{}), httpapi.Page, func(rt *Runtime, _ access.Actor, r *http.Request) (any, error) {
		limit, offset := parsePage(r)
		return rt.locationHistory(r.Context(), r.PathValue("id"), limit, offset)
	}),
	locationRoute(httpapi.GET, "/moderation/location-rules/check", "Resolves an IP and reports matching enabled rules for each domain.", nil, httpapi.OK(content.LocationCheck{}), []httpapi.Param{httpapi.Text("ip", "IPv4 or IPv6 address")}, func(rt *Runtime, _ access.Actor, r *http.Request) (any, error) {
		ip, err := netip.ParseAddr(r.URL.Query().Get("ip"))
		if err != nil {
			return nil, badRequest("ip must be an IPv4 or IPv6 address")
		}
		return rt.checkLocation(r.Context(), ip)
	}),
}
