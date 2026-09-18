package contentkit

import (
	"reflect"
	"testing"

	"github.com/open-rails/contentkit/contentref"
)

func TestGroupByContentOrdersItemsWithoutEnglishPreference(t *testing.T) {
	t.Parallel()
	v := func(item, version string) ContentRef { return contentref.NewVersion("t", "v", item, version) }
	docs := []groupedDoc{
		{ref: v("a", "a-en"), language: "en", score: .9},
		{ref: v("b", "b-es"), language: "es", score: .9, requested: true},
		{ref: v("a", "a-es"), language: "es", score: .5, requested: true, priority: 1},
		{ref: v("a", "a-es2"), language: "es", score: .5, requested: true},
		{ref: v("c", "c-en"), language: "en", score: 1},
		{ref: contentref.New("t", "w", "c"), language: "es", score: .9, requested: true},
	}
	var got []string
	for _, g := range groupByContent(docs) {
		got = append(got, g.best.ref.ContentID+":"+g.representative.ref.Version()+"/"+g.representative.language)
	}
	// c ranks first on score; a and b tie at .9 and the requested language
	// breaks the tie; a is represented by its requested-language default even
	// though its English document ranked it; the kind w item is its own group.
	want := []string{"c:c-en/en", "b:b-es/es", "c:/es", "a:a-es2/es"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groups=%v want %v", got, want)
	}
	groups := groupByContent(docs)
	if p, more := page(groups, 1, 2); len(p) != 2 || !more || p[0].best.ref.ContentID != "b" {
		t.Fatalf("page=%+v more=%v", p, more)
	}
	if p, more := page(groups, 3, 5); len(p) != 1 || more {
		t.Fatalf("last page=%+v more=%v", p, more)
	}
	if p, more := page(groups, 4, 5); p != nil || more {
		t.Fatalf("empty page=%+v more=%v", p, more)
	}
}
