package content

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A list endpoint with nothing to return must answer [] and never null. A nil
// slice encodes as null, which a caller reads as "no field" rather than "no
// items", so client code that measures the response breaks on empty results.
func TestListResponseIsEmptyArrayNotNull(t *testing.T) {
	var empty []Comment

	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, orEmpty(empty))

	body := rec.Body.String()
	if body != "[]\n" {
		t.Fatalf("empty list encoded as %q, want \"[]\\n\"", body)
	}

	// The shape must survive a decode as a list, which is what a client does.
	var decoded []Comment
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("client could not decode the empty list: %v", err)
	}
	if decoded == nil {
		t.Error("decoded value is still nil; a client walking it would panic")
	}
	if len(decoded) != 0 {
		t.Errorf("decoded %d items from an empty list", len(decoded))
	}
}

// Whatever orEmpty does to the empty case, it must not disturb a real one.
func TestOrEmptyLeavesPopulatedListsAlone(t *testing.T) {
	items := []Comment{{ID: "a"}, {ID: "b"}}

	got := orEmpty(items)

	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("order or contents changed: %v", got)
	}
}
