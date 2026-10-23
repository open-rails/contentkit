package discovery

import (
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
)

func TestTimedEntityFiltersAndAccessWithoutWorkers(t *testing.T) {
	billing := &dateBilling{releaseBilling: releaseBilling{held: map[string]bool{}}, complete: true}
	f := newFixtureBilling(t, nil, "", siteConfig(), billing)
	f.group("rail", "featured")
	f.seed(item{n: 1, titles: map[string]string{"en": "Featured work"}, age: time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}})
	id := f.ids["featured"]
	future := time.Now().Add(time.Hour)
	if err := f.groups.AddMemberships(f.ctx, []groups.Membership{{GroupID: groups.GroupID(id), ContentID: f.cid(1), ActiveFrom: &future}}); err != nil {
		t.Fatal(err)
	}
	billing.held[tenant+":group:"+id] = true
	actor := access.Actor{ID: "reader"}
	check := func(want int, allowed bool) {
		t.Helper()
		page, err := f.svc.List(f.ctx, ListOptions{Reader: Reader{Actor: actor, Host: "main.test", Mode: access.Hide}, Filter: Filter{Kind: "gallery", Groups: []string{id}}, Sort: "position"})
		if err != nil || len(page.Contents) != want {
			t.Fatalf("featured membership/access: %+v %v want=%d", page, err, want)
		}
		ref := contentref.New(tenant, "gallery", f.cid(1))
		resolved, err := f.cat.Resolver().Resolve(f.ctx, []contentref.ContentRef{ref}, actor)
		if err != nil || resolved[ref.Key()].Accessible != allowed {
			t.Fatalf("direct access: %+v %v want=%v", resolved, err, allowed)
		}
	}
	check(0, false)
	f.exec(`UPDATE {s}.content_memberships SET active_from=now()-interval '3 days' WHERE group_id=$1`, id)
	check(1, true)
	f.exec(`UPDATE {s}.content_memberships SET active_until=now()-interval '2 days' WHERE group_id=$1`, id)
	check(0, false)
}
