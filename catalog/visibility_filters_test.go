package catalog

import (
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/domainpolicy"
	"github.com/open-rails/contentkit/kinds"
)

func TestViewerOptionalFilterCopies(t *testing.T) {
	v := &Viewer{actor: access.Actor{ID: "reader"}, domain: kinds.Domain{Name: "main"}, language: "en", mode: access.Hide, now: time.Now(),
		hidden: map[string][]string{"gallery": {"b", "a"}}, policies: domainpolicy.Policies{"gallery": {Required: []string{"required"}, Blocked: []string{"blocked"}}},
		asked: map[string]bool{"channel": true}}
	got := v.HiddenContentFilters()
	if !reflect.DeepEqual(got, map[string][]string{"gallery": {"a", "b"}}) {
		t.Fatal(got)
	}
	got["gallery"][0] = "changed"
	delete(got, "gallery")
	if !reflect.DeepEqual(v.hidden["gallery"], []string{"b", "a"}) {
		t.Fatal("caller mutated original preferences")
	}
	relaxed := v.WithoutContentFilters()
	if len(relaxed.HiddenContentFilters()) != 0 || len(v.HiddenContentFilters()) != 1 {
		t.Fatal("relaxation changed original preferences")
	}
	want := *v
	want.hidden = nil
	if !reflect.DeepEqual(relaxed, &want) {
		t.Fatal("relaxation changed identity, hard policy, language, access or clock")
	}
	relaxed.asked["channel"] = false
	if !v.asked["channel"] {
		t.Fatal("clone changed original request cache")
	}
}
