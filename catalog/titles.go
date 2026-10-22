package catalog

import (
	"context"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
	"golang.org/x/text/language"
)

func (s *Store) seedVersionTitles(ctx context.Context, tx pgx.Tx, contentID string, languages []string, supplied map[string]VersionText) (map[string]VersionText, error) {
	out := maps.Clone(supplied)
	if out == nil {
		out = map[string]VersionText{}
	}
	rows, err := tx.Query(ctx, `SELECT language,title FROM `+s.t.text+` WHERE tenant_id=$1 AND content_id=$2 AND language=ANY($3)`, s.tenant, contentID, languages)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var language, title string
		if err := rows.Scan(&language, &title); err != nil {
			return nil, err
		}
		text := out[language]
		if strings.TrimSpace(text.Title) == "" {
			text.Title = title
			out[language] = text
		}
	}
	return out, rows.Err()
}

func cleanOriginalTitle(title, sourceLanguage string) (string, string, error) {
	title, sourceLanguage = strings.TrimSpace(title), strings.TrimSpace(sourceLanguage)
	if utf8.RuneCountInString(title) > maxTitle {
		return "", "", invalid("original title exceeds %d characters", maxTitle)
	}
	if len(sourceLanguage) > 128 {
		return "", "", invalid("original language is too long")
	}
	if sourceLanguage != "" {
		tag, err := language.Parse(sourceLanguage)
		if err != nil {
			return "", "", invalid("original language %q is not a language tag", sourceLanguage)
		}
		sourceLanguage = tag.String()
	}
	return title, sourceLanguage, nil
}

// BackfillVersionTitles is an explicit, repeatable adoption step for a kind
// switching to original_and_version titles. It only copies titles from the
// exact same content language into a version serving that language. Existing
// edition titles/descriptions and all content text remain unchanged. It never
// invents an original title or infers the source language.
func BackfillVersionTitles(ctx context.Context, pool *pgxpool.Pool, schema, tenant, kind string) (int64, error) {
	if pool == nil || schema == "" || tenant == "" || kind == "" {
		return 0, invalid("title backfill needs schema, tenant and kind")
	}
	q := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	var count int64
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `INSERT INTO `+q("content_version_text")+` AS existing (tenant_id,version_id,language,title,description)
 SELECT v.tenant_id,v.id,t.language,t.title,''
 FROM `+q("content_versions")+` v
 JOIN `+q("content_items")+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 JOIN `+q("content_text")+` t ON t.tenant_id=c.tenant_id AND t.content_id=c.id AND t.language=ANY(v.languages)
 WHERE c.tenant_id=$1 AND c.kind=$2 AND trim(t.title)<>'' ORDER BY v.id,t.language
 ON CONFLICT (tenant_id,version_id,language) DO UPDATE SET title=EXCLUDED.title WHERE trim(existing.title)=''
 RETURNING version_id,language`, tenant, kind)
		if err != nil {
			return err
		}
		type changedTitle struct{ version, language string }
		changed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (changedTitle, error) {
			var r changedTitle
			err := row.Scan(&r.version, &r.language)
			return r, err
		})
		if err != nil {
			return err
		}
		count = int64(len(changed))
		if count == 0 {
			return nil
		}
		versions := []string{}
		for _, row := range changed {
			if !slices.Contains(versions, row.version) {
				versions = append(versions, row.version)
			}
		}
		rows, err = tx.Query(ctx, `UPDATE `+q("content_versions")+` SET revision=revision+1,updated_at=clock_timestamp()
 WHERE tenant_id=$1 AND id=ANY($2) RETURNING id,content_id`, tenant, versions)
		if err != nil {
			return err
		}
		parents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
			var p [2]string
			err := row.Scan(&p[0], &p[1])
			return p, err
		})
		if err != nil {
			return err
		}
		ids := []string{}
		var marks []search.DirtyMark
		for _, parent := range parents {
			if !slices.Contains(ids, parent[1]) {
				ids = append(ids, parent[1])
			}
			for _, row := range changed {
				if row.version == parent[0] {
					marks = append(marks, search.DirtyMark{DocumentKey: search.DocumentKey{ContentRef: contentref.New(tenant, kind, parent[1]), Language: row.language}, Reason: "version_titles"})
				}
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE `+q("content_items")+` SET revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=ANY($2)`, tenant, ids); err != nil {
			return err
		}
		return search.MarkDirty(ctx, tx, schema, marks)
	})
	return count, err
}

// editionTitle never treats the shared original as a translation. An edition
// uses its requested language when it serves it, otherwise its own language.
func editionTitle(version *Version, requested, original, legacy string) string {
	if version != nil {
		languages := version.Languages
		if slices.Contains(languages, requested) {
			languages = append([]string{requested}, languages...)
		}
		for _, lang := range languages {
			if text, ok := version.Text[lang]; ok && strings.TrimSpace(text.Title) != "" {
				return text.Title
			}
		}
	}
	if original != "" {
		return original
	}
	return legacy
}
