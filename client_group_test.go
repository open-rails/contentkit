package searchkit

import (
	"reflect"
	"testing"
)

func TestGroupByParentOrdersItemsWithoutEnglishPreference(t *testing.T) {
	t.Parallel()
	docs := []groupedDoc{
		{EntityType: "v", EntityID: "a-en", ParentID: "a", Language: "en", Score: .9},
		{EntityType: "v", EntityID: "b-es", ParentID: "b", Language: "es", Score: .9, requested: true},
		{EntityType: "v", EntityID: "a-es", ParentID: "a", Language: "es", Score: .5, requested: true, Priority: 1},
		{EntityType: "v", EntityID: "a-es2", ParentID: "a", Language: "es", Score: .5, requested: true},
		{EntityType: "v", EntityID: "c-en", ParentID: "c", Language: "en", Score: 1},
		{EntityType: "w", EntityID: "c", Language: "es", Score: .9, requested: true},
	}
	var got []string
	for _, g := range groupByParent(docs) {
		got = append(got, g.best.ParentID+":"+g.representative.EntityID+"/"+g.representative.Language)
	}
	// c ranks first on score; a and b tie at .9 and the requested language
	// breaks the tie; a is represented by its requested-language default even
	// though its English document ranked it.
	want := []string{"c:c-en/en", "b:b-es/es", "c:c/es", "a:a-es2/es"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groups=%v want %v", got, want)
	}
	groups := groupByParent(docs)
	if p, more := page(groups, 1, 2); len(p) != 2 || !more || p[0].best.ParentID != "b" {
		t.Fatalf("page=%+v more=%v", p, more)
	}
	if p, more := page(groups, 3, 5); len(p) != 1 || more {
		t.Fatalf("last page=%+v more=%v", p, more)
	}
	if p, more := page(groups, 4, 5); p != nil || more {
		t.Fatalf("empty page=%+v more=%v", p, more)
	}
}
