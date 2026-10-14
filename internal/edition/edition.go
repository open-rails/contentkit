// Package edition is the one default-version rule: the version a reader of a
// language opens by default is the first live version by position serving
// that language, else the first live version (contentkit-ui defaultVersion).
// Cards, detail, covers and sitemaps all pick it this way.
package edition

import "fmt"

// Order is the rule as ORDER BY terms over the content_facets row aliased f,
// language an SQL text expression: live first, then serving language, then
// position and id. A content without live versions falls to its first.
func Order(f, language string) string {
	return fmt.Sprintf(`(coalesce(%[1]s.live_from <= now(), false) AND NOT %[1]s.deleted) DESC, (%[2]s = ANY(%[1]s.languages)) DESC, %[1]s.position, %[1]s.version_id`,
		f, language)
}
