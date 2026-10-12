package search

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/contentkit/contentref"
)

// Executor lets a pool or a transaction own document and queue writes.
type Executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// DocumentKey identifies one keyword document: a content reference in one
// language. The language is metadata of the document, never of the content.
type DocumentKey struct {
	contentref.ContentRef
	Language string
}

func (k DocumentKey) validate() error {
	if err := k.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(k.Language) == "" {
		return fmt.Errorf("search: Language is required for %s", k.ContentRef)
	}
	return nil
}

// KeywordDocument is the search input for one document, built by ContentKit
// from its own records.
// Derived search text belongs to ContentKit; callers never write it. An empty
// Title deletes the document. Aliases are alternate names; Keywords are
// contextual terms (for example trusted tags), not descriptions.
type KeywordDocument struct {
	DocumentKey
	Title    string
	Aliases  []string
	Keywords []string
}

// Validate checks a document before it crosses into the keyword index.
// An empty title is a deletion, so only its key needs validation.
func (d KeywordDocument) Validate() error {
	if err := d.DocumentKey.validate(); err != nil {
		return err
	}
	title := strings.TrimSpace(d.Title)
	if title == "" {
		return nil
	}
	if utf8.RuneCountInString(title) > 512 || len(d.Aliases) > 64 || len(d.Keywords) > 256 {
		return fmt.Errorf("search: document %s exceeds keyword input limits", d.ContentRef)
	}
	for _, values := range [][]string{d.Aliases, d.Keywords} {
		for _, value := range values {
			if utf8.RuneCountInString(value) > 512 {
				return fmt.Errorf("search: document %s term exceeds 512 characters", d.ContentRef)
			}
		}
	}
	return nil
}

type documentRow struct {
	TenantID         string   `json:"tenant_id"`
	ContentKind      string   `json:"content_kind"`
	ContentID        string   `json:"content_id"`
	ContentVersionID string   `json:"content_version_id"`
	Language         string   `json:"language"`
	Title            string   `json:"title,omitempty"`
	Aliases          []string `json:"aliases"`
	Keywords         []string `json:"keywords"`
	Raw              string   `json:"raw,omitempty"`
}

func keyRow(k DocumentKey) documentRow {
	return documentRow{TenantID: k.TenantID, ContentKind: k.ContentKind, ContentID: k.ContentID, ContentVersionID: k.Version(), Language: k.Language}
}

const rowColumns = `(tenant_id text,content_kind text,content_id text,content_version_id text,language text,title text,aliases text[],keywords text[],raw text)`

// UpsertKeywordDocuments derives every lexical projection from structured
// input for a batch of documents of any tenant, kind or language.
func UpsertKeywordDocuments(ctx context.Context, db Executor, schema string, docs []KeywordDocument) error {
	if db == nil {
		return fmt.Errorf("search: database is required")
	}
	qs, err := quoteIdent(schema)
	if err != nil {
		return err
	}
	rows := make([]documentRow, 0, len(docs))
	var removed []DocumentKey
	for _, doc := range docs {
		if err := doc.Validate(); err != nil {
			return err
		}
		doc.Title = strings.TrimSpace(doc.Title)
		if doc.Title == "" {
			removed = append(removed, doc.DocumentKey)
			continue
		}
		row := keyRow(doc.DocumentKey)
		row.Title, row.Aliases, row.Keywords = doc.Title, append([]string{}, doc.Aliases...), append([]string{}, doc.Keywords...)
		row.Raw = strings.Join(append(append([]string{doc.Title}, row.Aliases...), row.Keywords...), "\n")
		rows = append(rows, row)
	}
	if len(rows) > 0 {
		data, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		query := fmt.Sprintf(`INSERT INTO %s.content_search_documents(tenant_id,content_kind,content_id,content_version_id,language,title,aliases,keywords,raw_document,created_at,updated_at)
   SELECT r.tenant_id,r.content_kind,r.content_id,r.content_version_id,r.language,r.title,r.aliases,r.keywords,r.raw,now(),now()
   FROM jsonb_to_recordset($1::jsonb) AS r%s
   ON CONFLICT(tenant_id,content_kind,content_id,content_version_id,language) DO UPDATE SET title=EXCLUDED.title,aliases=EXCLUDED.aliases,keywords=EXCLUDED.keywords,raw_document=EXCLUDED.raw_document,updated_at=now()`, qs, rowColumns)
		if _, err := db.Exec(ctx, query, data); err != nil {
			return err
		}
	}
	return DeleteKeywordDocuments(ctx, db, schema, removed)
}

// DeleteKeywordDocuments removes the documents; unknown keys are ignored.
func DeleteKeywordDocuments(ctx context.Context, db Executor, schema string, keys []DocumentKey) error {
	if db == nil {
		return fmt.Errorf("search: database is required")
	}
	if len(keys) == 0 {
		return nil
	}
	qs, err := quoteIdent(schema)
	if err != nil {
		return err
	}
	rows := make([]documentRow, 0, len(keys))
	for _, k := range keys {
		if err := k.validate(); err != nil {
			return err
		}
		rows = append(rows, keyRow(k))
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.content_search_documents sd USING jsonb_to_recordset($1::jsonb) AS r%s
   WHERE sd.tenant_id=r.tenant_id AND sd.content_kind=r.content_kind AND sd.content_id=r.content_id AND sd.content_version_id=r.content_version_id AND sd.language=r.language`, qs, rowColumns), data)
	return err
}

// DirtyMark asks the worker to rebuild (or delete) one document.
type DirtyMark struct {
	DocumentKey
	Deleted bool
	Reason  string
}

// MarkDirty queues documents for the worker, in the transaction that changes
// their records; the queue trigger assigns each row a new revision.
func MarkDirty(ctx context.Context, db Executor, schema string, marks []DirtyMark) error {
	if db == nil {
		return fmt.Errorf("search: database is required")
	}
	if len(marks) == 0 {
		return nil
	}
	qs, err := quoteIdent(schema)
	if err != nil {
		return err
	}
	type dirtyRow struct {
		documentRow
		Deleted bool   `json:"deleted"`
		Reason  string `json:"reason"`
	}
	rows := make([]dirtyRow, 0, len(marks))
	for _, m := range marks {
		if err := m.validate(); err != nil {
			return err
		}
		reason := strings.TrimSpace(m.Reason)
		if reason == "" {
			reason = "host"
		}
		rows = append(rows, dirtyRow{keyRow(m.DocumentKey), m.Deleted, reason})
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.content_search_dirty(tenant_id,content_kind,content_id,content_version_id,language,is_deleted,reason)
   SELECT r.tenant_id,r.content_kind,r.content_id,r.content_version_id,r.language,r.deleted,r.reason
   FROM jsonb_to_recordset($1::jsonb) AS r(tenant_id text,content_kind text,content_id text,content_version_id text,language text,deleted boolean,reason text)
   ON CONFLICT(tenant_id,content_kind,content_id,content_version_id,language) DO UPDATE SET is_deleted=EXCLUDED.is_deleted,reason=EXCLUDED.reason,updated_at=now()`, qs), data)
	return err
}
