package critical_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	catalogstore "github.com/open-rails/contentkit/internal/catalogstore"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/helpers/eventlog"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// A host keeps its own links to entities and editions. It reads ContentKit
// only through the remote client and repairs links from reads and events.
func TestHostAPIContract(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{}
		cfg.Content.EntityKinds["series"] = kinds.EntityKind{}
		cfg.Content.EntityKinds["character"] = kinds.EntityKind{ScopedBy: "series"}
	})
	a := w.apps[0]
	persona, err := w.auth.Persona("app")
	must(t, err)
	key, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "host service", Role: persona.OwnerRole()})
	must(t, err)
	host := a.client(key.Secret)
	named := func(kind, slug string, internal bool, scope entities.EntityID, names ...entities.Name) entities.Entity {
		t.Helper()
		got, err := host.PostEntities(t.Context(), []entities.EntityInput{{Kind: kind, Slug: slug, Internal: internal, ScopeID: scope, Names: names}})
		must(t, err)
		return got[0]
	}
	en := func(kind entities.NameKind, name string) entities.Name {
		return entities.Name{Language: "en", Kind: kind, Name: name}
	}
	tag := func(slug, name string) entities.Entity { return named("tag", slug, false, "", en("name", name)) }

	t.Run("CK-J31-S01_resolve_answers_deleted_internal_and_merged_entities", func(t *testing.T) {
		kept, gone, first, second, survivor := tag("kept", "Kept"), tag("gone", "Gone"), tag("first", "First"), tag("second", "Second"), tag("survivor", "Survivor")
		internal := named("tag", "policy", true, "", en("name", "Policy"))
		_, err := host.DeleteEntity(t.Context(), string(gone.ID))
		must(t, err)
		_, err = host.PostEntityMerge(t.Context(), string(first.ID), entities.MergeInput{Into: second.ID})
		must(t, err)
		_, err = host.PostEntityMerge(t.Context(), string(second.ID), entities.MergeInput{Into: survivor.ID})
		must(t, err)
		unknown := uuid.Must(uuid.NewV7()).String()
		ids := []string{string(kept.ID), string(gone.ID), string(internal.ID), string(first.ID), string(second.ID), string(survivor.ID), unknown}
		page, err := host.GetManageEntities(t.Context(), ck.GetManageEntitiesQuery{Ids: ids})
		must(t, err)
		type resolved struct {
			State      entities.State
			MergedInto entities.EntityID
			Internal   bool
		}
		got := map[entities.EntityID]resolved{}
		for _, card := range page.Entities {
			got[card.ID] = resolved{card.State, card.MergedInto, card.Internal}
		}
		want := map[entities.EntityID]resolved{
			kept.ID:     {State: entities.StateActive},
			gone.ID:     {State: entities.StateDeleted},
			internal.ID: {State: entities.StateActive, Internal: true},
			first.ID:    {State: entities.StateMerged, MergedInto: survivor.ID},
			second.ID:   {State: entities.StateMerged, MergedInto: survivor.ID},
			survivor.ID: {State: entities.StateActive},
		}
		if !reflect.DeepEqual(got, want) || page.NextCursor != "" {
			t.Fatalf("resolve = %+v (next %q); want %+v", got, page.NextCursor, want)
		}
		_, err = host.GetManageEntities(t.Context(), ck.GetManageEntitiesQuery{Ids: ids, State: "deleted"})
		requireError(t, err, 400, "invalid_request")
		many := make([]string, entities.MaxListIDs+1)
		for i := range many {
			many[i] = uuid.Must(uuid.NewV7()).String()
		}
		_, err = host.GetManageEntities(t.Context(), ck.GetManageEntitiesQuery{Ids: many})
		requireError(t, err, 400, "invalid_request")
	})

	t.Run("CK-J31-S02_exact_name_match_is_literal_or_normalized_and_bounded", func(t *testing.T) {
		accented := tag("cafe-accented", "Café Noir")
		spaced := tag("cafe-spaced", "cafe  noir")
		upper := tag("cafe-upper", "CAFE NOIR")
		aliased := named("tag", "sword", false, "", en("name", "Sword"), en("alias", "Blade"))
		german := named("tag", "augen", false, "", entities.Name{Language: "de", Kind: "name", Name: "Große Augen"})
		named("tag", "hidden-name", true, "", en("name", "Hidden Name"))
		deleted := tag("deleted-name", "Deleted Name")
		_, err := host.DeleteEntity(t.Context(), string(deleted.ID))
		must(t, err)
		merged, into := tag("merged-name", "Merged Name"), tag("merged-into", "Merge Target")
		_, err = host.PostEntityMerge(t.Context(), string(merged.ID), entities.MergeInput{Into: into.ID})
		must(t, err)
		ids := func(gs ...entities.Entity) []entities.EntityID {
			out := []entities.EntityID{}
			for _, g := range gs {
				out = append(out, g.ID)
			}
			slices.Sort(out)
			return out
		}
		match := func(q ck.GetManageEntitiesMatchQuery) map[string][]entities.EntityID {
			t.Helper()
			got, err := host.GetManageEntitiesMatch(t.Context(), q)
			must(t, err)
			if len(got) != len(q.Name) {
				t.Fatalf("match answered %d of %d names", len(got), len(q.Name))
			}
			out := map[string][]entities.EntityID{}
			for i, m := range got {
				if m.Name != q.Name[i] || len(m.IDs) > 2 {
					t.Fatalf("match result %d = %+v for %q", i, m, q.Name[i])
				}
				out[m.Name] = m.IDs
			}
			return out
		}
		literal := match(ck.GetManageEntitiesMatchQuery{Kind: "tag", Name: []string{"Café Noir", "cafe noir", "Blade", "Hidden Name", "Deleted Name", "Merged Name", "Merge Target", "Unknown, With Comma"}})
		want := map[string][]entities.EntityID{"Café Noir": ids(accented), "cafe noir": {}, "Blade": ids(aliased), "Hidden Name": {}, "Deleted Name": {},
			"Merged Name": ids(into), "Merge Target": ids(into), "Unknown, With Comma": {}}
		if !reflect.DeepEqual(literal, want) {
			t.Fatalf("literal match = %v; want %v", literal, want)
		}
		normalized := true
		folded := match(ck.GetManageEntitiesMatchQuery{Kind: "tag", Normalized: &normalized, Name: []string{"CAFÉ  NOIR", "blade"}})
		all := ids(accented, spaced, upper)
		if got := folded["CAFÉ  NOIR"]; len(got) != 2 || !slices.IsSorted(got) || !slices.Contains(all, got[0]) || !slices.Contains(all, got[1]) {
			t.Fatalf("ambiguous normalized match = %v; want two of %v", got, all)
		}
		if !reflect.DeepEqual(folded["blade"], ids(aliased)) {
			t.Fatalf("normalized alias match = %v", folded["blade"])
		}
		if got := match(ck.GetManageEntitiesMatchQuery{Kind: "tag", Language: "en", Name: []string{"Große Augen"}}); len(got["Große Augen"]) != 0 {
			t.Fatalf("language filter matched another language: %v", got)
		}
		if got := match(ck.GetManageEntitiesMatchQuery{Kind: "tag", Language: "de", Name: []string{"Große Augen"}}); !reflect.DeepEqual(got["Große Augen"], ids(german)) {
			t.Fatalf("language match = %v", got)
		}
		first, second := named("series", "first-series", false, ""), named("series", "second-series", false, "")
		alice := named("character", "alice", false, first.ID, en("name", "Alice"))
		named("character", "alice", false, second.ID, en("name", "Alice"))
		if got := match(ck.GetManageEntitiesMatchQuery{Kind: "character", Scope: string(first.ID), Name: []string{"Alice"}}); !reflect.DeepEqual(got["Alice"], ids(alice)) {
			t.Fatalf("scoped match = %v", got)
		}
		if got := match(ck.GetManageEntitiesMatchQuery{Kind: "character", Name: []string{"Alice"}}); len(got["Alice"]) != 0 {
			t.Fatalf("unscoped match reached scoped entities: %v", got)
		}
		_, err = host.GetManageEntitiesMatch(t.Context(), ck.GetManageEntitiesMatchQuery{Name: []string{"Alice"}})
		requireError(t, err, 400, "invalid_request")
		_, err = host.GetManageEntitiesMatch(t.Context(), ck.GetManageEntitiesMatchQuery{Kind: "tag"})
		requireError(t, err, 400, "invalid_request")
	})

	t.Run("CK-J31-S03_version_status_follows_draft_live_scheduled_deleted_and_purged", func(t *testing.T) {
		admin := a.engine.Client()
		live := publishedArticle(t, a, "Live edition")
		removed := publishedArticle(t, a, "Deleted edition")
		must(t, admin.DeleteContentVersion(t.Context(), removed.ID, removed.Versions[0].ID))
		draft, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Versions: []catalog.VersionInput{{Languages: []string{"de"}, Text: map[string]catalog.VersionText{"de": {Title: "Entwurf"}}}}})
		must(t, err)
		scheduled, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Scheduled edition", Body: "Later"}}}}})
		must(t, err)
		due := w.clock.now().Add(time.Hour)
		_, err = admin.PostContentPublish(t.Context(), scheduled.ID, catalog.PublishInput{PublishAt: &due})
		must(t, err)
		unknown := uuid.Must(uuid.NewV7()).String()
		order := []string{removed.Versions[0].ID, draft.Versions[0].ID, unknown, live.Versions[0].ID, scheduled.Versions[0].ID}
		got, err := host.GetVersions(t.Context(), ck.GetVersionsQuery{Ids: order})
		must(t, err)
		if len(got) != 4 {
			t.Fatalf("status answered %d editions, want 4 without the unknown one: %+v", len(got), got)
		}
		check := func(s catalog.VersionStatus, item catalog.Content, kind string, languages []string, deleted, isLive, released bool) {
			t.Helper()
			if s.VersionID != item.Versions[0].ID || s.ContentID != item.ID || s.Kind != kind || !reflect.DeepEqual(s.Languages, languages) ||
				s.Deleted != deleted || s.Live != isLive || (s.LiveFrom != nil) != released {
				t.Fatalf("status %+v for %s", s, item.ID)
			}
		}
		check(got[0], removed, "article", []string{"en"}, true, false, true)
		check(got[1], draft, "gallery", []string{"de"}, false, false, false)
		check(got[2], live, "article", []string{"en"}, false, true, true)
		check(got[3], scheduled, "article", []string{"en"}, false, false, false)
		if got[2].LiveFrom.After(w.clock.now()) {
			t.Fatalf("live edition starts in the future: %v", got[2].LiveFrom)
		}
		_, err = host.GetVersions(t.Context(), ck.GetVersionsQuery{})
		requireError(t, err, 400, "invalid_request")
		_, err = host.GetVersions(t.Context(), ck.GetVersionsQuery{Ids: []string{"not-a-version"}})
		requireError(t, err, 400, "invalid_request")

		w.clock.set(w.clock.now().Add(31 * 24 * time.Hour))
		producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
		must(t, err)
		job, err := producer.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_purge_deleted"}, &river.InsertOpts{Queue: catalogstore.QueueFor(a.cfg.Scope.AppID.String()), MaxAttempts: 1})
		must(t, err)
		waitForWithin(t, 45*time.Second, func() bool {
			current, err := producer.JobGet(t.Context(), job.Job.ID)
			must(t, err)
			if current.State == rivertype.JobStateDiscarded {
				t.Fatalf("purge failed: %+v", current.Errors)
			}
			return current.State == rivertype.JobStateCompleted
		})
		got, err = host.GetVersions(t.Context(), ck.GetVersionsQuery{Ids: order})
		must(t, err)
		if len(got) != 3 || got[0].VersionID != draft.Versions[0].ID || got[1].VersionID != live.Versions[0].ID || got[2].VersionID != scheduled.Versions[0].ID {
			t.Fatalf("purged edition is still answered: %+v", got)
		}
	})

	t.Run("CK-J31-S04_entity_events_tell_hosts_to_repair_links", func(t *testing.T) {
		types := []string{entities.EventCreated, entities.EventUpdated, entities.EventDeleted, entities.EventRestored, entities.EventMerged}
		reader := host.EventReader()
		start, err := reader.ListEvents(t.Context(), eventlog.Query{Types: types})
		must(t, err)
		for start.HasMore {
			start, err = reader.ListEvents(t.Context(), eventlog.Query{After: start.NextCursor, Types: types})
			must(t, err)
		}
		made, err := host.PostEntities(t.Context(), []entities.EntityInput{
			{Kind: "tag", Slug: "event-p", Names: []entities.Name{en("name", "Event P")}},
			{Kind: "tag", Slug: "event-q", Names: []entities.Name{en("name", "Event Q")}},
			{Kind: "tag", Slug: "event-r", Names: []entities.Name{en("name", "Event R")}},
		})
		must(t, err)
		p, q, r := made[0], made[1], made[2]
		_, err = host.PostEntityNames(t.Context(), string(p.ID), []entities.Name{en("alias", "Event P alias")})
		must(t, err)
		_, err = host.DeleteEntity(t.Context(), string(q.ID))
		must(t, err)
		active := entities.StateActive
		_, err = host.PatchEntity(t.Context(), string(q.ID), entities.EntityUpdate{State: &active})
		must(t, err)
		_, err = host.PostEntityMerge(t.Context(), string(r.ID), entities.MergeInput{Into: p.ID})
		must(t, err)
		_, err = host.PostEntityMerge(t.Context(), string(p.ID), entities.MergeInput{Into: q.ID})
		must(t, err)

		type fact struct {
			Type, ID, MergedInto string
		}
		var got []fact
		cursor := start.NextCursor
		for {
			page, err := reader.ListEvents(t.Context(), eventlog.Query{After: cursor, Types: types})
			must(t, err)
			for _, e := range page.Events {
				var data struct {
					Kind       string `json:"kind"`
					EntityID   string `json:"entity_id"`
					MergedInto string `json:"merged_into"`
				}
				must(t, json.Unmarshal(e.Data, &data))
				if e.Subject.Type != "tag" || data.Kind != "tag" || e.Subject.ID != data.EntityID || !slices.Contains(types, e.Type) {
					t.Fatalf("entity event envelope %+v data %s", e, e.Data)
				}
				got = append(got, fact{e.Type, data.EntityID, data.MergedInto})
			}
			cursor = page.NextCursor
			if !page.HasMore {
				break
			}
		}
		P, Q, R := string(p.ID), string(q.ID), string(r.ID)
		want := []fact{
			{entities.EventCreated, P, ""}, {entities.EventCreated, Q, ""}, {entities.EventCreated, R, ""},
			{entities.EventUpdated, P, ""},
			{entities.EventDeleted, Q, ""},
			{entities.EventRestored, Q, ""},
			{entities.EventMerged, R, P}, {entities.EventUpdated, P, ""},
			{entities.EventMerged, P, Q}, {entities.EventMerged, R, Q}, {entities.EventUpdated, Q, ""},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("entity events = %+v; want %+v", got, want)
		}
		records, err := host.ListEvents(t.Context(), catalog.EventQuery{After: start.NextCursor, Types: types})
		must(t, err)
		var merged []catalog.EventRecord
		for _, e := range records.Events {
			if e.Type == entities.EventMerged {
				merged = append(merged, e)
			}
		}
		if len(merged) != 3 || merged[1].EntityID != P || merged[1].MergedInto != Q || merged[1].ContentID != "" || merged[1].Kind != "tag" {
			t.Fatalf("merged catalog records = %+v", merged)
		}
	})
}
