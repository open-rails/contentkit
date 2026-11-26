package rankings

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
)

// A ranked list pages in three parts: the ranking's entries with a zero or
// positive score, in order; then everything the ranking leaves out (no
// activity in the window), in the list's own order; then the negative
// entries. Each part reads Postgres in chunks with the viewer's filters, so a
// list drops only what they block.
const (
	partRanked = iota
	partRest
	partNegative
)

// Cursor continues a ranked list. Version pins the ranking paged while it is
// cached, so pages don't shift when it is replaced.
type Cursor struct {
	Version string `json:"v,omitempty"`
	Part    int    `json:"ph,omitempty"`
	// Pos is the next ranked position (the first and last parts).
	Pos int `json:"p,omitempty"`
	// After is the rest's key of the last row read.
	After string `json:"a,omitempty"`
}

// Encode is the cursor's wire form.
func (c Cursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor reads Encode's form.
func DecodeCursor(raw string) (Cursor, bool) {
	var c Cursor
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) > 4096 || json.Unmarshal(b, &c) != nil || c.Part < partRanked || c.Part > partNegative || c.Pos < 0 {
		return Cursor{}, false
	}
	return c, true
}

// Row is one row of a list's rest: its id and the key the rest continues
// after.
type Row struct{ ID, Key string }

// Lister reads one viewer's list.
type Lister interface {
	// Admit keeps the ids the viewer's list shows, in their order.
	Admit(ctx context.Context, ids []string) ([]string, error)
	// Rest is up to n rows the viewer's list shows after key after ("" from
	// the start), in the list's order of entries without activity.
	Rest(ctx context.Context, after string, n int) ([]Row, error)
}

// restScans bounds the rest's reads per page: a ranking covering nearly
// everything leaves a short page and a cursor further on.
const restScans = 8

// Page is the next limit ids of r's list after cur, past the first skip
// (a numbered page's offset), and the cursor after them; nil at the end.
func Page(ctx context.Context, r *Ranking, l Lister, cur Cursor, limit, skip int) ([]string, *Cursor, error) {
	if cur.Version != r.Version {
		cur = Cursor{}
	}
	negative := sort.Search(len(r.Stats), func(i int) bool { return r.Stats[i].Score < 0 })
	type item struct {
		id    string
		after Cursor
	}
	var items []item
	want := skip + limit + 1
	at := Cursor{Version: r.Version, Part: cur.Part, Pos: cur.Pos, After: cur.After}
	ranked := func(end int) error {
		size := max(2*limit, 50)
		for at.Pos < end && len(items) < want {
			chunk := r.IDs[at.Pos:min(at.Pos+size, end)]
			kept, err := l.Admit(ctx, chunk)
			if err != nil {
				return err
			}
			for _, id := range kept {
				if p, ok := r.Position(id); ok {
					items = append(items, item{id, Cursor{Version: r.Version, Part: at.Part, Pos: p + 1}})
				}
			}
			at.Pos += len(chunk)
			size = min(2*size, 2000)
		}
		return nil
	}
	if at.Part == partRanked {
		if err := ranked(negative); err != nil {
			return nil, nil, err
		}
		if at.Pos >= negative {
			at = Cursor{Version: r.Version, Part: partRest}
		}
	}
	short := false
	if at.Part == partRest && len(items) < want {
		size := max(2*limit, 50)
		for scans := 0; len(items) < want; scans++ {
			if scans == restScans {
				short = true
				break
			}
			rows, err := l.Rest(ctx, at.After, size)
			if err != nil {
				return nil, nil, err
			}
			for _, row := range rows {
				at.After = row.Key
				if _, ok := r.Position(row.ID); !ok {
					items = append(items, item{row.ID, Cursor{Version: r.Version, Part: partRest, After: row.Key}})
				}
			}
			if len(rows) < size {
				at = Cursor{Version: r.Version, Part: partNegative, Pos: negative}
				break
			}
			size = min(2*size, 2000)
		}
	}
	if at.Part == partNegative && len(items) < want {
		if err := ranked(len(r.IDs)); err != nil {
			return nil, nil, err
		}
	}
	items = items[min(skip, len(items)):]
	ids := make([]string, 0, min(len(items), limit))
	for i := 0; i < len(items) && i < limit; i++ {
		ids = append(ids, items[i].id)
	}
	switch {
	case len(items) > limit:
		next := items[limit-1].after
		return ids, &next, nil
	case short:
		return ids, &at, nil
	}
	return ids, nil, nil
}
