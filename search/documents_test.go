package search

import (
	"strings"
	"testing"

	"github.com/open-rails/contentkit/contentref"
)

func TestKeywordDocumentValidate(t *testing.T) {
	key := DocumentKey{ContentRef: contentref.New("doujins", "gallery", cid(1)), Language: "en"}
	for _, tc := range []struct {
		name    string
		doc     KeywordDocument
		wantErr bool
	}{
		{"valid", KeywordDocument{DocumentKey: key, Title: "title"}, false},
		{"blank title deletes", KeywordDocument{DocumentKey: key, Title: "  ", Keywords: make([]string, 257)}, false},
		{"long title", KeywordDocument{DocumentKey: key, Title: strings.Repeat("a", 513)}, true},
		{"too many aliases", KeywordDocument{DocumentKey: key, Title: "title", Aliases: make([]string, 65)}, true},
		{"too many keywords", KeywordDocument{DocumentKey: key, Title: "title", Keywords: make([]string, 257)}, true},
		{"long term", KeywordDocument{DocumentKey: key, Title: "title", Keywords: []string{strings.Repeat("a", 513)}}, true},
		{"invalid ref", KeywordDocument{DocumentKey: DocumentKey{ContentRef: contentref.New("doujins", "gallery", "not-a-uuid"), Language: "en"}, Title: "title"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.doc.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}
