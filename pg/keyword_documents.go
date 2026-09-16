package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/open-rails/searchkit/internal/textnormalize"
)

// KeywordDocument is the host's canonical search input. Derived search text and
// vectors belong to Searchkit; callers should not write them independently.
// Empty Title deletes the document. Aliases are alternate names, while Keywords
// are contextual terms (for example trusted tags), not descriptions.
type KeywordDocument struct {
	Title    string   `json:"title"`
	Aliases  []string `json:"aliases"`
	Keywords []string `json:"keywords"`
}

// UpsertKeywordDocuments derives every lexical projection from structured input.
func UpsertKeywordDocuments(ctx context.Context, db DocumentExecutor, schema, entityType, language string, docs map[string]KeywordDocument) error {
	if db == nil || strings.TrimSpace(entityType) == "" || strings.TrimSpace(language) == "" {
		return fmt.Errorf("database, entity type and language are required")
	}
	qs, err := QuoteSchema(schema)
	if err != nil {
		return err
	}
	type row struct {
		KeywordDocument
		ID       string `json:"id"`
		Raw      string `json:"raw"`
		Document string `json:"document"`
	}
	ids := make([]string, 0, len(docs))
	for id := range docs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("document ID is required")
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := make([]row, 0, len(ids))
	var removed []string
	for _, id := range ids {
		doc := docs[id]
		doc.Title = strings.TrimSpace(doc.Title)
		if doc.Title == "" {
			removed = append(removed, id)
			continue
		}
		if utf8.RuneCountInString(doc.Title) > 512 || len(doc.Aliases) > 64 || len(doc.Keywords) > 256 {
			return fmt.Errorf("document %s exceeds keyword input limits", id)
		}
		for _, values := range [][]string{doc.Aliases, doc.Keywords} {
			for _, value := range values {
				if utf8.RuneCountInString(value) > 512 {
					return fmt.Errorf("document %s term exceeds 512 characters", id)
				}
			}
		}
		if doc.Aliases == nil {
			doc.Aliases = []string{}
		}
		if doc.Keywords == nil {
			doc.Keywords = []string{}
		}
		fields := append([]string{doc.Title}, doc.Aliases...)
		fields = append(fields, doc.Keywords...)
		raw := strings.Join(fields, "\n")
		rows = append(rows, row{doc, id, raw, textnormalize.Heavy(raw)})
	}
	if len(rows) > 0 {
		data, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		query := fmt.Sprintf(`INSERT INTO %s.search_documents(entity_type,entity_id,language,title,aliases,keywords,raw_document,document,tsv,created_at,updated_at)
   SELECT $1,r.id,$2,r.title,r.aliases,r.keywords,r.raw,r.document,to_tsvector(%s.searchkit_regconfig_for_language($2),r.raw),now(),now()
   FROM jsonb_to_recordset($3::jsonb) AS r(id text,title text,aliases text[],keywords text[],raw text,document text)
   ON CONFLICT(entity_type,entity_id,language) DO UPDATE SET title=EXCLUDED.title,aliases=EXCLUDED.aliases,keywords=EXCLUDED.keywords,raw_document=EXCLUDED.raw_document,document=EXCLUDED.document,tsv=EXCLUDED.tsv,updated_at=now()`, qs, qs)
		if _, err := db.Exec(ctx, query, entityType, language, data); err != nil {
			return err
		}
	}
	return DeleteSearchDocumentsMany(ctx, db, schema, entityType, removed, language)
}
