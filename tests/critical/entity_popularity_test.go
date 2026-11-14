package critical_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

func TestEntityPopularity(t *testing.T) {
	configure := func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags", Popular: &kinds.EntityPopularity{ContentKind: "article"},
			Attributes: map[string]kinds.Attribute{"categorical": {Type: "bool"}}}
		cfg.Content.EntityKinds["label"] = kinds.EntityKind{Route: "labels"}
		article := cfg.Content.Kinds["article"]
		article.Entities = []string{"tag", "label"}
		cfg.Content.Kinds["article"] = article
	}
	for _, domains := range []bool{false, true} {
		name := "default"
		if domains {
			name = "domains"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, configure, func(cfg *ck.Config) {
				if domains {
					cfg.Content.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"article"}, Domains: []kinds.Domain{
						{Name: "main"},
						{Name: "restricted", Hosts: []string{"restricted.invalid"}, RequireEntities: map[string][]kinds.EntityRef{
							"article": {{Kind: "label", Slug: "allowed"}},
						}},
					}}
				}
			})
			a := w.apps[0]
			admin := a.engine.Client()
			tenant := a.cfg.Scope.AppID.String()
			table := func(name string) string { return pgx.Identifier{w.schema, name}.Sanitize() }
			tag := func(slug string) entities.EntityID {
				created, err := admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "tag", Slug: slug}})
				must(t, err)
				return created[0].ID
			}
			labels, err := admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "label", Slug: "allowed"}})
			must(t, err)
			// Seed live facets directly, as in the domain-read fixtures. No signal plane is configured.
			seed := func(id entities.EntityID) string {
				item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{
					{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Entity popularity fixture"}}},
				}})
				must(t, err)
				members := []string{string(id), string(labels[0].ID)}
				_, err = w.pool.Exec(t.Context(), `UPDATE `+table("content_facets")+`
 SET live_from=now()-interval '1 hour',entity_ids=$3::text[] WHERE tenant_id=$1 AND content_id=$2`, tenant, item.ID, members)
				must(t, err)
				return item.ID
			}
			// User IDs are fixture account identifiers; generate_series keeps the floor fixtures cheap.
			favorite := func(content string, accounts, days int, version *string) {
				_, err := w.pool.Exec(t.Context(), `INSERT INTO `+table("content_favorites")+`
 (tenant_id,content_kind,content_id,user_id,content_version_id,value,revision,created_at)
 SELECT $1,'article',$2,'account-'||n,$5,1,1,now()-make_interval(days=>$4) FROM generate_series(1,$3::int) n`,
					tenant, content, accounts, days, version)
				must(t, err)
			}
			pair, recent, old, backup := tag("pair"), tag("recent"), tag("old"), tag("backup")
			solo, repeated, floor := tag("solo"), tag("repeated"), tag("floor")
			categorical, internal, deleted := tag("categorical"), tag("internal"), tag("deleted")
			expired, inactive, anonymous, hidden := tag("expired"), tag("inactive"), tag("anonymous"), tag("hidden")
			quiet := tag("quiet")
			tag("unused")
			pairContent := seed(pair)
			favorite(pairContent, 32, 1, nil)
			favorite(seed(pair), 32, 1, nil)
			// A version row must not add support or replace the newer work favourite.
			version := "01900000-0000-7000-8000-000000000001"
			favorite(pairContent, 32, 61, &version)
			for range 2 {
				favorite(seed(recent), 28, 1, nil)
				favorite(seed(old), 48, 61, nil)
				favorite(seed(floor), 20, 1, nil)
				favorite(seed(categorical), 60, 1, nil)
				favorite(seed(internal), 60, 1, nil)
				favorite(seed(deleted), 60, 1, nil)
				favorite(seed(expired), 200, 181, nil)
				item := seed(inactive)
				favorite(item, 60, 1, nil)
				_, err = w.pool.Exec(t.Context(), `UPDATE `+table("content_favorites")+` SET value=0 WHERE tenant_id=$1 AND content_id=$2`, tenant, item)
				must(t, err)
				item = seed(anonymous)
				favorite(item, 60, 1, nil)
				_, err = w.pool.Exec(t.Context(), `UPDATE `+table("content_favorites")+` SET anonymous_actor_id=gen_random_uuid(),user_id=NULL WHERE tenant_id=$1 AND content_id=$2`, tenant, item)
				must(t, err)
				item = seed(hidden)
				favorite(item, 60, 1, nil)
				_, err = w.pool.Exec(t.Context(), `UPDATE `+table("content_facets")+` SET live_from=NULL WHERE tenant_id=$1 AND content_id=$2`, tenant, item)
				must(t, err)
			}
			favorite(seed(backup), 50, 1, nil)
			favorite(seed(backup), 50, 121, nil)
			favorite(seed(solo), 200, 1, nil)
			for range 3 {
				favorite(seed(repeated), 15, 1, nil)
			}
			for range 4 {
				seed(quiet)
			}
			_, err = w.pool.Exec(t.Context(), `UPDATE `+table("content_entities")+` SET attributes='{"categorical":true}' WHERE tenant_id=$1 AND id=$2`, tenant, categorical)
			must(t, err)
			_, err = w.pool.Exec(t.Context(), `UPDATE `+table("content_entities")+` SET internal=true WHERE tenant_id=$1 AND id=$2`, tenant, internal)
			must(t, err)
			_, err = w.pool.Exec(t.Context(), `UPDATE `+table("content_entities")+` SET state='deleted' WHERE tenant_id=$1 AND id=$2`, tenant, deleted)
			must(t, err)
			// Owner-only membership is included, and an out-of-domain content cannot supply the guard's backup.
			_, err = w.pool.Exec(t.Context(), `UPDATE `+table("content_facets")+` SET owner_entity_id=$2,entity_ids=array_remove(entity_ids,$2) WHERE tenant_id=$1 AND content_id=$3`, tenant, string(pair), pairContent)
			must(t, err)
			if domains {
				_, err = w.pool.Exec(t.Context(), `UPDATE `+table("content_facets")+` SET entity_ids=array_remove(entity_ids,$2) WHERE tenant_id=$1 AND content_id=$3`, tenant, string(labels[0].ID), pairContent)
				must(t, err)
			}
			countPage, err := admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", Sort: "count", Limit: 200})
			must(t, err)
			unbuilt, err := admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", Sort: "popular", Limit: 200})
			must(t, err)
			if !slices.Equal(entityIDs(unbuilt.Entities), entityIDs(countPage.Entities)) {
				t.Fatal("unbuilt popular listing differs from content count order")
			}

			must(t, a.engine.StartJobs(t.Context()))
			queue, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
			must(t, err)
			refresh := func(fail bool) {
				t.Helper()
				job, err := queue.Insert(t.Context(), scheduledContentJob{kind: "contentkit_entity_popular_refresh"}, &river.InsertOpts{Queue: "contentkit", MaxAttempts: 1})
				must(t, err)
				waitForWithin(t, time.Minute, func() bool {
					row, err := queue.JobGet(t.Context(), job.Job.ID)
					must(t, err)
					if row.State == rivertype.JobStateDiscarded {
						if !fail {
							t.Fatalf("entity refresh failed: %+v", row.Errors)
						}
						return true
					}
					if row.State == rivertype.JobStateCompleted {
						if fail {
							t.Fatal("entity refresh succeeded despite rejected inserts")
						}
						return true
					}
					return false
				})
			}
			// Startup schedules this refresh even without signals.
			waitForWithin(t, time.Minute, func() bool {
				var built bool
				must(t, w.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM `+table("content_entity_popular_rankings")+` WHERE tenant_id=$1)`, tenant).Scan(&built))
				return built
			})
			refresh(false)
			places := func(domain string) []entities.EntityID {
				t.Helper()
				rows, err := w.pool.Query(t.Context(), `SELECT entity_id FROM `+table("content_entity_popular_rankings")+` WHERE tenant_id=$1 AND entity_kind='tag' AND domain=$2 ORDER BY place`, tenant, domain)
				must(t, err)
				ids, err := pgx.CollectRows(rows, pgx.RowTo[entities.EntityID])
				must(t, err)
				return ids
			}
			domain := ""
			if domains {
				domain = "main"
			}
			want := []entities.EntityID{pair, recent, backup, old}
			if got := places(domain); !slices.Equal(got, want) {
				t.Fatalf("places = %v; want %v", got, want)
			}
			if domains {
				if got := places("restricted"); !slices.Equal(got, want[1:]) {
					t.Fatalf("restricted places = %v; want %v", got, want[1:])
				}
				public := ck.NewClient(a.server.URL+"/content", &http.Client{Transport: domainTransport{host: "restricted.invalid"}})
				page, err := public.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", Sort: "popular", Limit: 3})
				must(t, err)
				if got := entityIDs(page.Entities); !slices.Equal(got, want[1:]) {
					t.Fatalf("restricted listing = %v; want %v", got, want[1:])
				}
			}
			late := tag("late")
			for range 5 {
				seed(late)
			}
			counts, err := admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", Sort: "count", Limit: 200})
			must(t, err)
			for _, card := range counts.Entities {
				if !slices.Contains(want, card.ID) {
					want = append(want, card.ID)
				}
			}
			var got []entities.EntityID
			query := ck.GetEntitiesQuery{Kind: "tag", Sort: "popular", Limit: 6}
			for {
				page, err := admin.GetEntities(t.Context(), query)
				must(t, err)
				got = append(got, entityIDs(page.Entities)...)
				if page.NextCursor == "" {
					break
				}
				if len(got) > len(want) {
					t.Fatal("popular cursor repeated entities")
				}
				query.Cursor = page.NextCursor
			}
			if !slices.Equal(got, want) || got[4] != late || got[5] != quiet || got[6] != repeated {
				t.Fatalf("paged order = %v; want %v", got, want)
			}
			numbered, err := admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", Sort: "popular", Limit: 6, Page: 2})
			must(t, err)
			if !slices.Equal(entityIDs(numbered.Entities), want[6:12]) || numbered.Total == nil || *numbered.Total != len(want) {
				t.Fatalf("numbered page = %+v; want %v of %d", numbered, want[6:12], len(want))
			}
			// Reject replacement inserts after the delete; the transaction must retain the old places.
			_, err = w.pool.Exec(t.Context(), `ALTER TABLE `+table("content_entity_popular_rankings")+` ADD CONSTRAINT reject_entity_ranking CHECK (tenant_id <> '`+tenant+`') NOT VALID`)
			must(t, err)
			refresh(true)
			if got := places(domain); !slices.Equal(got, want[:4]) {
				t.Fatalf("failed refresh changed places: %v", got)
			}
			_, err = w.pool.Exec(t.Context(), `ALTER TABLE `+table("content_entity_popular_rankings")+` DROP CONSTRAINT reject_entity_ranking`)
			must(t, err)
			refresh(false)
			_, err = admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "label", Sort: "popular"})
			requireError(t, err, 400, "invalid_request")
			_, err = admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Sort: "popular"})
			requireError(t, err, 400, "invalid_request")
		})
	}
}

func entityIDs(cards []entities.EntityCard) []entities.EntityID {
	ids := make([]entities.EntityID, len(cards))
	for i, c := range cards {
		ids[i] = c.ID
	}
	return ids
}
