package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/codes"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/offers"
)

// ContentInput creates a content item, a draft until published.
type ContentInput struct {
	Kind string `json:"kind"`
	// Text is the text per language.
	Text    map[string]Text `json:"text,omitempty"`
	Listing string          `json:"listing,omitempty"` // default listed
	// Comments is open (the default), closed (existing ones show, new ones
	// are refused) or hidden.
	Comments string `json:"comments,omitempty"`
	// Access defaults to public.
	Access       *access.Rule   `json:"access,omitempty"`
	Restrictions []string       `json:"restrictions,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
	// OwnerGroupID is the home group (a channel); its editors edit the item.
	// It never changes.
	OwnerGroupID string `json:"owner_group_id,omitempty"`
	// Creators nil credits the creating person as its author.
	Creators *[]Creator `json:"creators,omitempty"`
	// Versions: at least one, except for a kind with hidden versions, which
	// gets its one version automatically.
	Versions []VersionInput `json:"versions,omitempty"`
}

// ContentPatch edits a content item; nil fields stay.
type ContentPatch struct {
	Listing      *string        `json:"listing,omitempty"`
	Comments     *string        `json:"comments,omitempty"`
	Access       *access.Rule   `json:"access,omitempty"`
	Restrictions *[]string      `json:"restrictions,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"` // replaces all
	Creators     *[]Creator     `json:"creators,omitempty"`
	// Cover is an inline image name of the content's folder; "" clears.
	Cover *string `json:"cover,omitempty"`
	// Revision, when set, refuses the edit (409) unless it is current.
	Revision *int64 `json:"revision,omitempty"`
}

// VersionInput creates or edits a version; nil fields stay (or default).
type VersionInput struct {
	Languages []string  `json:"languages,omitempty"`
	Traits    *[]string `json:"traits,omitempty"`
	Label     *string   `json:"label,omitempty"`
	// State is draft or published (the default for a new version): a
	// published version is live once its content is and PublishAt passed.
	State     string     `json:"state,omitempty"`
	PublishAt *time.Time `json:"publish_at,omitempty"`
	// Access overrides the content's rule; ClearAccess inherits it again.
	Access      *access.Rule           `json:"access,omitempty"`
	ClearAccess bool                   `json:"clear_access,omitempty"`
	Attributes  map[string]any         `json:"attributes,omitempty"` // replaces all
	Text        map[string]VersionText `json:"text,omitempty"`       // replaces all
	Segments    *[]Segment             `json:"segments,omitempty"`   // replaces all
	Ordinal     *int                   `json:"ordinal,omitempty"`
	// Fork fills a new version's media from a sibling (creation only).
	Fork *VersionFork `json:"fork,omitempty"`
}

// VersionFork copies a sibling version's media into a new version: its
// uploads and reader metadata, with Playback its audio, with Segments its
// segments.
type VersionFork struct {
	From     string `json:"from"`
	Playback bool   `json:"playback,omitempty"`
	Segments bool   `json:"segments,omitempty"`
}

// Update is one ContentPatch.
type Update struct {
	ID string
	ContentPatch
}

// TextPut sets (Text non-nil) or removes one language's text.
type TextPut struct {
	ContentID string
	Language  string
	Text      *Text
}

// Publish publishes a content item now, or at At when it is in the future
// (scheduled).
type Publish struct {
	ContentID string
	At        *time.Time
}

// VersionPut creates (VersionID "") or edits a version.
type VersionPut struct {
	ContentID string
	VersionID string
	VersionInput
}

// VersionKey names one version.
type VersionKey struct{ ContentID, VersionID string }

// effects is what a write changed, applied before it commits.
type effects struct {
	events []Event
	codes  bool // slugs may have changed
	expose bool // anonymous visibility may have changed
	// resync are offer targets whose product's name or required entitlements
	// may have changed.
	resync []offers.Target
	// republish are versions whose effective rule or segments changed.
	republish []versions.Target
	forks     []versions.Fork
}

// mutate runs fn over the authorized, locked items in one transaction, then
// bumps their revisions, refreshes facets and documents, re-codes, emits
// events and queues media exposure before committing.
func (s *Store) mutate(ctx context.Context, actor access.Actor, ids []string, fn func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error)) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	got, err := s.authorize(ctx, tx, actor, ids)
	if err != nil {
		return err
	}
	fx, err := fn(ctx, tx, got)
	if err != nil {
		return err
	}
	if err := s.apply(ctx, tx, ids, fx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) apply(ctx context.Context, tx pgx.Tx, ids []string, fx effects) error {
	if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET revision = revision + 1, updated_at = clock_timestamp() WHERE tenant_id = $1 AND id = ANY($2)`, s.tenant, ids); err != nil {
		return err
	}
	if fx.codes {
		if err := s.putCodes(ctx, tx, ids); err != nil {
			return err
		}
	}
	if err := s.RefreshFacets(ctx, tx, ids); err != nil {
		return err
	}
	if err := s.emit(ctx, tx, fx.events...); err != nil {
		return err
	}
	if s.media != nil && len(fx.republish) > 0 {
		if err := s.media.Versions.RepublishTx(ctx, tx, fx.republish); err != nil {
			return err
		}
	}
	if len(fx.forks) > 0 {
		if s.media == nil {
			return errMediaNotConfigured
		}
		if err := s.media.Versions.ForkTx(ctx, tx, fx.forks); err != nil {
			return err
		}
	}
	if len(fx.resync) > 0 && s.offers != nil {
		if err := s.offers.Resync(ctx, tx, fx.resync); err != nil {
			return err
		}
	}
	if fx.expose {
		return s.exposeTx(ctx, tx, ids)
	}
	return nil
}

// Create creates content items as drafts and returns their ids in order.
// actor needs root:content:write, or to edit each item's owner group.
func (s *Store) Create(ctx context.Context, actor access.Actor, in []ContentInput) ([]string, error) {
	type prepared struct {
		ContentInput
		id       string
		kind     kinds.Kind
		creators []Creator
		versions []VersionInput
	}
	preps := make([]prepared, len(in))
	var owners []string
	for i, c := range in {
		p := prepared{ContentInput: c, id: contentref.NewID()}
		var err error
		if p.kind, err = s.kind(c.Kind); err != nil {
			return nil, err
		}
		if p.Listing == "" {
			p.Listing = ListingListed
		}
		if err := s.checkListing(p.Listing); err != nil {
			return nil, err
		}
		if p.Comments == "" {
			p.Comments = content.CommentsOpen
		}
		if err := checkComments(p.Comments); err != nil {
			return nil, err
		}
		rule := access.Rule{Level: access.Public}
		if c.Access != nil {
			rule = *c.Access
		}
		if rule, err = s.checkRule(rule); err != nil {
			return nil, err
		}
		p.Access = &rule
		if p.Restrictions, err = s.checkRestrictions(c.Restrictions); err != nil {
			return nil, err
		}
		if p.Attributes, err = checkAttributes(p.kind.Attributes, c.Attributes); err != nil {
			return nil, err
		}
		texts := map[string]Text{}
		for l, t := range c.Text {
			if texts[l], err = s.cleanText(ctx, l, t); err != nil {
				return nil, err
			}
		}
		p.Text = texts
		switch {
		case c.Creators != nil:
			p.creators, err = s.checkCreators(*c.Creators)
		case actor.Person():
			p.creators = []Creator{{UserID: actor.ID, Role: "author"}}
		}
		if err != nil {
			return nil, err
		}
		if p.kind.Versions.Hidden {
			if len(c.Versions) > 0 {
				return nil, invalid("kind %s has one hidden version; give it no versions", c.Kind)
			}
			p.versions = []VersionInput{{Languages: s.hiddenLanguages(p.kind, texts)}}
		} else if p.versions = c.Versions; len(p.versions) == 0 {
			return nil, invalid("a content item needs at least one version")
		}
		if slices.ContainsFunc(p.versions, func(v VersionInput) bool { return v.Fork != nil }) {
			return nil, invalid("a new item's versions have no sibling to fork")
		}
		owners = append(owners, c.OwnerGroupID)
		preps[i] = p
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := s.authorizeCreate(ctx, tx, actor, owners); err != nil {
		return nil, err
	}
	ids := make([]string, len(preps))
	var events []Event
	for i, p := range preps {
		if err := s.checkRuleGroups(ctx, tx, p.Access); err != nil {
			return nil, err
		}
		if p.OwnerGroupID != "" {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.groups+` WHERE tenant_id = $1 AND id = $2 AND state = 'active')`, s.tenant, p.OwnerGroupID).Scan(&ok); err != nil {
				return nil, err
			}
			if !ok {
				return nil, invalid("owner group %s does not exist", p.OwnerGroupID)
			}
		}
		rule, _ := json.Marshal(p.Access)
		attrs, _ := json.Marshal(p.Attributes)
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.items+` (tenant_id, id, kind, listing, access, restrictions, attributes, owner_group_id, created_by, comments)
 VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), nullif($9, ''), $10)`,
			s.tenant, p.id, p.Kind, p.Listing, rule, p.Restrictions, attrs, p.OwnerGroupID, viewerID(actor), p.Comments); err != nil {
			return nil, err
		}
		if err := s.writeTexts(ctx, tx, p.id, p.Text, nil); err != nil {
			return nil, err
		}
		if err := s.writeCreators(ctx, tx, p.id, p.creators); err != nil {
			return nil, err
		}
		for _, v := range p.versions {
			vid, err := s.insertVersion(ctx, tx, p.id, p.Kind, p.kind, v)
			if err != nil {
				return nil, err
			}
			events = append(events, Event{Type: EventVersionCreated, Kind: p.Kind, ContentID: p.id, VersionID: vid})
		}
		ids[i] = p.id
		events = append([]Event{{Type: EventContentCreated, Kind: p.Kind, ContentID: p.id}}, events...)
	}
	if err := s.apply(ctx, tx, ids, effects{events: events, codes: true}); err != nil {
		return nil, err
	}
	return ids, tx.Commit(ctx)
}

// hiddenLanguages are the languages a hidden version serves: its content's
// text languages (the first in site order for a one-language kind), else the
// site's first language.
func (s *Store) hiddenLanguages(k kinds.Kind, texts map[string]Text) []string {
	var out []string
	for _, l := range s.cfg.Languages {
		if _, ok := texts[l]; ok {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		out = []string{s.defaultLanguage()}
	}
	if k.Versions.Languages == "one" {
		out = out[:1]
	}
	return out
}

// Update edits content items.
func (s *Store) Update(ctx context.Context, actor access.Actor, in []Update) error {
	ids := make([]string, len(in))
	for i, u := range in {
		ids[i] = u.ID
		if u.Listing != nil {
			if err := s.checkListing(*u.Listing); err != nil {
				return err
			}
		}
		if u.Comments != nil {
			if err := checkComments(*u.Comments); err != nil {
				return err
			}
		}
		if u.Access != nil {
			r, err := s.checkRule(*u.Access)
			if err != nil {
				return err
			}
			in[i].Access = &r
		}
		if u.Restrictions != nil {
			rs, err := s.checkRestrictions(*u.Restrictions)
			if err != nil {
				return err
			}
			in[i].Restrictions = &rs
		}
		if u.Cover != nil && *u.Cover != "" && !media.ValidNamed(*u.Cover) {
			return invalid("cover must be an inline image name (i-{uuid})")
		}
	}
	return s.mutate(ctx, actor, ids, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{expose: true}
		for _, u := range in {
			o := got[u.ID]
			if o.state == StateDeleted {
				return fx, ErrNotFound
			}
			if u.Revision != nil {
				var rev int64
				if err := tx.QueryRow(ctx, `SELECT revision FROM `+s.t.items+` WHERE tenant_id = $1 AND id = $2`, s.tenant, u.ID).Scan(&rev); err != nil {
					return fx, err
				}
				if rev != *u.Revision {
					return fx, errStale
				}
			}
			if err := s.checkRuleGroups(ctx, tx, u.Access); err != nil {
				return fx, err
			}
			var access, attrs, cover []byte
			if u.Access != nil {
				access, _ = json.Marshal(u.Access)
			}
			if u.Attributes != nil {
				a, err := checkAttributes(s.cfg.Kinds[o.kind].Attributes, u.Attributes)
				if err != nil {
					return fx, err
				}
				attrs, _ = json.Marshal(a)
			}
			if u.Cover != nil && *u.Cover != "" {
				if s.media == nil {
					return fx, errMediaNotConfigured
				}
				cover, _ = json.Marshal(Cover{Image: *u.Cover})
			}
			var restrictions []string
			if u.Restrictions != nil {
				restrictions = *u.Restrictions
			}
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET listing = coalesce($3, listing), access = coalesce($4, access),
 restrictions = coalesce($5, restrictions), attributes = coalesce($6, attributes),
 cover = CASE WHEN $7 THEN $8::jsonb ELSE cover END, comments = coalesce($9, comments) WHERE tenant_id = $1 AND id = $2`,
				s.tenant, u.ID, u.Listing, access, restrictions, attrs, u.Cover != nil, cover, u.Comments); err != nil {
				return fx, err
			}
			if u.Creators != nil {
				cs, err := s.checkCreators(*u.Creators)
				if err != nil {
					return fx, err
				}
				if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.creators+` WHERE tenant_id = $1 AND content_id = $2`, s.tenant, u.ID); err != nil {
					return fx, err
				}
				if err := s.writeCreators(ctx, tx, u.ID, cs); err != nil {
					return fx, err
				}
			}
			if u.Access != nil {
				inheriting, err := s.inheritingVersions(ctx, tx, u.ID)
				if err != nil {
					return fx, err
				}
				fx.republish = append(fx.republish, inheriting...)
				fx.resync = append(fx.resync, offers.Target{Kind: offers.ContentTarget, ID: u.ID})
				for _, v := range inheriting {
					fx.resync = append(fx.resync, offers.Target{Kind: offers.VersionTarget, ID: v.Version, Content: u.ID})
				}
			}
			fx.events = append(fx.events, Event{Type: EventContentUpdated, Kind: o.kind, ContentID: u.ID})
		}
		return fx, nil
	})
}

// inheritingVersions are the versions of content id that follow its rule.
func (s *Store) inheritingVersions(ctx context.Context, tx pgx.Tx, id string) ([]versions.Target, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $2 AND access IS NULL AND state <> 'deleted' ORDER BY id`, s.tenant, id)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]versions.Target, len(ids))
	for i, v := range ids {
		out[i] = versions.Target{Content: id, Version: v}
	}
	return out, nil
}

var errStale = errors.Join(ErrConflict, errors.New("the content changed since it was read"))

func (s *Store) writeCreators(ctx context.Context, tx pgx.Tx, id string, cs []Creator) error {
	for _, c := range cs {
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.creators+` (tenant_id, content_id, user_id, role, position) VALUES ($1, $2, $3, $4, $5)`,
			s.tenant, id, c.UserID, c.Role, c.Position); err != nil {
			return err
		}
	}
	return nil
}

// writeTexts upserts set and deletes the languages in drop.
func (s *Store) writeTexts(ctx context.Context, tx pgx.Tx, id string, set map[string]Text, drop []string) error {
	for l, t := range set {
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.text+` (tenant_id, content_id, language, title, slug, description, body, sort_key, aliases)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
 ON CONFLICT (tenant_id, content_id, language) DO UPDATE SET title = EXCLUDED.title, slug = EXCLUDED.slug, description = EXCLUDED.description,
 body = EXCLUDED.body, sort_key = EXCLUDED.sort_key, aliases = EXCLUDED.aliases, updated_at = clock_timestamp()`,
			s.tenant, id, l, t.Title, t.Slug, t.Description, t.Body, t.SortKey, orEmpty(t.Aliases)); err != nil {
			return err
		}
	}
	if len(drop) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.text+` WHERE tenant_id = $1 AND content_id = $2 AND language = ANY($3)`, s.tenant, id, drop); err != nil {
			return err
		}
	}
	return nil
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// putCodes gives each item its content code with a slug per language (the
// site's first language with text is the default).
func (s *Store) putCodes(ctx context.Context, tx pgx.Tx, ids []string) error {
	rows, err := tx.Query(ctx, `SELECT c.id, c.kind, t.language, t.slug FROM `+s.t.items+` c
 LEFT JOIN `+s.t.text+` t ON t.tenant_id = c.tenant_id AND t.content_id = c.id WHERE c.tenant_id = $1 AND c.id = ANY($2)`, s.tenant, ids)
	if err != nil {
		return err
	}
	entries := map[string]*codes.Entry{}
	var order []string
	for rows.Next() {
		var id, kind string
		var language, slug *string
		if err := rows.Scan(&id, &kind, &language, &slug); err != nil {
			rows.Close()
			return err
		}
		e, ok := entries[id]
		if !ok {
			e = &codes.Entry{Key: codes.Key{Kind: kind, ID: id}, Slugs: map[string]string{}}
			entries[id] = e
			order = append(order, id)
		}
		if language != nil {
			e.Slugs[*language] = *slug
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	out := make([]codes.Entry, 0, len(order))
	for _, id := range order {
		e := entries[id]
		for _, l := range s.cfg.Languages {
			if sl, ok := e.Slugs[l]; ok {
				e.Slug = sl
				break
			}
		}
		out = append(out, *e)
	}
	_, err = codes.Put(ctx, codes.Pgx(tx), s.qs, s.tenant, out)
	return err
}

// PutText sets or removes text per language. Text of a published item is
// screened first: a rejection writes nothing (content.RejectedError), a hold
// keeps the item's last approved text public until a reviewer decides.
func (s *Store) PutText(ctx context.Context, actor access.Actor, in []TextPut) error {
	byID := map[string][]TextPut{}
	var ids []string
	for i, p := range in {
		if err := s.checkLanguage(p.Language); err != nil {
			return err
		}
		if p.Text != nil {
			t, err := s.cleanText(ctx, p.Language, *p.Text)
			if err != nil {
				return err
			}
			in[i].Text = &t
		}
		if _, ok := byID[p.ContentID]; !ok {
			ids = append(ids, p.ContentID)
		}
		byID[p.ContentID] = append(byID[p.ContentID], in[i])
	}
	screens, err := s.screenTexts(ctx, actor, byID, false)
	if err != nil {
		return err
	}
	return s.mutate(ctx, actor, ids, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{codes: true, expose: true}
		for _, id := range ids {
			o := got[id]
			if o.state == StateDeleted {
				return fx, ErrNotFound
			}
			if err := s.checkScreened(ctx, tx, id, screens[id]); err != nil {
				return fx, err
			}
			// The snapshot of the approved text is taken before the write.
			if err := s.applyScreening(ctx, tx, id, screens[id]); err != nil {
				return fx, err
			}
			set := map[string]Text{}
			var drop []string
			for _, p := range byID[id] {
				if p.Text == nil {
					drop = append(drop, p.Language)
				} else {
					set[p.Language] = *p.Text
				}
			}
			retitled, err := s.retitled(ctx, tx, id, set, drop)
			if err != nil {
				return fx, err
			}
			fx.resync = append(fx.resync, retitled...)
			if err := s.writeTexts(ctx, tx, id, set, drop); err != nil {
				return fx, err
			}
			if err := s.syncHiddenVersion(ctx, tx, id, o.kind); err != nil {
				return fx, err
			}
			fx.events = append(fx.events, Event{Type: EventContentUpdated, Kind: o.kind, ContentID: id})
		}
		return fx, nil
	})
}

// retitled is the offer targets of content id and its versions when the
// write changes a title.
func (s *Store) retitled(ctx context.Context, tx pgx.Tx, id string, set map[string]Text, drop []string) ([]offers.Target, error) {
	rows, err := tx.Query(ctx, `SELECT language, title FROM `+s.t.text+` WHERE tenant_id = $1 AND content_id = $2`, s.tenant, id)
	if err != nil {
		return nil, err
	}
	titles := map[string]string{}
	for rows.Next() {
		var l, t string
		if err := rows.Scan(&l, &t); err != nil {
			rows.Close()
			return nil, err
		}
		titles[l] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	changed := slices.ContainsFunc(drop, func(l string) bool { _, ok := titles[l]; return ok })
	for l, t := range set {
		if old, ok := titles[l]; !ok || old != t.Title {
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	out := []offers.Target{{Kind: offers.ContentTarget, ID: id}}
	vrows, err := tx.Query(ctx, `SELECT id FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $2 ORDER BY id`, s.tenant, id)
	if err != nil {
		return nil, err
	}
	defer vrows.Close()
	for vrows.Next() {
		var vid string
		if err := vrows.Scan(&vid); err != nil {
			return nil, err
		}
		out = append(out, offers.Target{Kind: offers.VersionTarget, ID: vid, Content: id})
	}
	return out, vrows.Err()
}

// syncHiddenVersion keeps a hidden version serving its content's languages.
func (s *Store) syncHiddenVersion(ctx context.Context, tx pgx.Tx, id, kind string) error {
	k := s.cfg.Kinds[kind]
	if !k.Versions.Hidden {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT language, title FROM `+s.t.text+` WHERE tenant_id = $1 AND content_id = $2`, s.tenant, id)
	if err != nil {
		return err
	}
	texts := map[string]Text{}
	for rows.Next() {
		var l string
		var t Text
		if err := rows.Scan(&l, &t.Title); err != nil {
			rows.Close()
			return err
		}
		texts[l] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE `+s.t.versions+` SET languages = $3, updated_at = clock_timestamp(), revision = revision + 1
 WHERE tenant_id = $1 AND content_id = $2 AND languages <> $3`, s.tenant, id, s.hiddenLanguages(k, texts))
	return err
}

// Publish publishes content items now, or schedules those with a future At.
// A published item is screened (all its text); a hold leaves it unpublished
// until a reviewer approves it. Rescheduling a scheduled item moves its time;
// a live item is left as it is.
func (s *Store) Publish(ctx context.Context, actor access.Actor, in []Publish) error {
	ids := make([]string, len(in))
	byID := map[string][]TextPut{}
	for i, p := range in {
		ids[i] = p.ContentID
		byID[p.ContentID] = nil
	}
	screens, err := s.screenTexts(ctx, actor, byID, true)
	if err != nil {
		return err
	}
	now := s.now()
	return s.mutate(ctx, actor, ids, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{expose: true}
		for _, p := range in {
			o := got[p.ContentID]
			switch {
			case o.state == StateDeleted:
				return fx, ErrNotFound
			case o.state == StatePublished && o.publishedAt:
				continue
			}
			var titled, versioned bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.text+` WHERE tenant_id = $1 AND content_id = $2 AND title <> ''),
 EXISTS (SELECT 1 FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $2 AND state <> 'deleted')`, s.tenant, p.ContentID).Scan(&titled, &versioned); err != nil {
				return fx, err
			}
			if !titled || !versioned {
				return fx, invalid("content %s needs a title and a version to publish", p.ContentID)
			}
			if err := s.checkScreened(ctx, tx, p.ContentID, screens[p.ContentID]); err != nil {
				return fx, err
			}
			at := now
			if p.At != nil && p.At.After(now) {
				at = *p.At
			}
			if err := s.applyScreening(ctx, tx, p.ContentID, screens[p.ContentID]); err != nil {
				return fx, err
			}
			var live bool
			if err := tx.QueryRow(ctx, `UPDATE `+s.t.items+` SET state = 'published', publish_at = $3::timestamptz,
 published_at = CASE WHEN $3::timestamptz <= $4::timestamptz AND moderation = 'approved' THEN $4::timestamptz END
 WHERE tenant_id = $1 AND id = $2 RETURNING published_at IS NOT NULL`, s.tenant, p.ContentID, at, now).Scan(&live); err != nil {
				return fx, err
			}
			typ := EventContentScheduled
			if live {
				typ = EventContentPublished
			}
			fx.events = append(fx.events, Event{Type: typ, Kind: o.kind, ContentID: p.ContentID})
		}
		return fx, nil
	})
}

// Unpublish returns published or scheduled content items to drafts.
func (s *Store) Unpublish(ctx context.Context, actor access.Actor, ids []string) error {
	return s.mutate(ctx, actor, ids, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{expose: true}
		for _, id := range ids {
			o := got[id]
			if o.state != StatePublished {
				return fx, errors.Join(ErrConflict, errors.New("only a published item can be unpublished"))
			}
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET state = 'draft', publish_at = NULL, published_at = NULL WHERE tenant_id = $1 AND id = $2`, s.tenant, id); err != nil {
				return fx, err
			}
			fx.events = append(fx.events, Event{Type: EventContentUnpublished, Kind: o.kind, ContentID: id})
		}
		return fx, nil
	})
}

// Delete deletes content items. A deleted item stays readable to holders of
// its own key; one never sold is erased, media included, after the retention
// (PurgeDeleted), until when Restore brings it back.
func (s *Store) Delete(ctx context.Context, actor access.Actor, ids []string) error {
	return s.mutate(ctx, actor, ids, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{expose: true}
		for _, id := range ids {
			o := got[id]
			if o.state == StateDeleted {
				return fx, ErrNotFound
			}
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET state = 'deleted', deleted_at = clock_timestamp() WHERE tenant_id = $1 AND id = $2`, s.tenant, id); err != nil {
				return fx, err
			}
			fx.events = append(fx.events, Event{Type: EventContentDeleted, Kind: o.kind, ContentID: id})
		}
		return fx, nil
	})
}

// Restore brings deleted content items back as they were: published (or
// scheduled) when they had been, else drafts.
func (s *Store) Restore(ctx context.Context, actor access.Actor, ids []string) error {
	return s.mutate(ctx, actor, ids, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{expose: true}
		for _, id := range ids {
			o := got[id]
			if o.state != StateDeleted {
				return fx, ErrNotFound
			}
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET deleted_at = NULL,
 state = CASE WHEN published_at IS NOT NULL OR publish_at IS NOT NULL THEN 'published' ELSE 'draft' END WHERE tenant_id = $1 AND id = $2`, s.tenant, id); err != nil {
				return fx, err
			}
			fx.events = append(fx.events, Event{Type: EventContentRestored, Kind: o.kind, ContentID: id})
		}
		return fx, nil
	})
}

// PutVersions creates (VersionID "") or edits versions and returns their ids
// in order.
func (s *Store) PutVersions(ctx context.Context, actor access.Actor, in []VersionPut) ([]string, error) {
	var ids []string
	for _, p := range in {
		if !slices.Contains(ids, p.ContentID) {
			ids = append(ids, p.ContentID)
		}
	}
	out := make([]string, len(in))
	err := s.mutate(ctx, actor, ids, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{expose: true}
		for i, p := range in {
			o := got[p.ContentID]
			k := s.cfg.Kinds[o.kind]
			if o.state == StateDeleted {
				return fx, ErrNotFound
			}
			if k.Versions.Hidden {
				return fx, invalid("kind %s has one hidden version", o.kind)
			}
			if p.Fork != nil {
				if err := s.checkFork(ctx, tx, p); err != nil {
					return fx, err
				}
			}
			if p.VersionID == "" {
				vid, err := s.insertVersion(ctx, tx, p.ContentID, o.kind, k, p.VersionInput)
				if err != nil {
					return fx, err
				}
				out[i] = vid
				if f := p.Fork; f != nil {
					fx.forks = append(fx.forks, versions.Fork{Version: versions.Target{Content: p.ContentID, Version: vid}, From: f.From,
						Playback: f.Playback, Segments: f.Segments})
				}
				fx.events = append(fx.events, Event{Type: EventVersionCreated, Kind: o.kind, ContentID: p.ContentID, VersionID: vid})
				continue
			}
			if err := s.updateVersion(ctx, tx, p.ContentID, o.kind, k, p.VersionID, p.VersionInput); err != nil {
				return fx, err
			}
			if p.Label != nil || p.Access != nil || p.ClearAccess {
				fx.resync = append(fx.resync, offers.Target{Kind: offers.VersionTarget, ID: p.VersionID, Content: p.ContentID})
			}
			if p.Access != nil || p.ClearAccess || p.Segments != nil {
				fx.republish = append(fx.republish, versions.Target{Content: p.ContentID, Version: p.VersionID})
			}
			out[i] = p.VersionID
			fx.events = append(fx.events, Event{Type: EventVersionUpdated, Kind: o.kind, ContentID: p.ContentID, VersionID: p.VersionID})
		}
		return fx, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type versionRow struct {
	languages, traits []string
	label, state      string
	publishAt         *time.Time
	access            []byte
	attributes        []byte
	ordinal           *int
}

func (s *Store) versionRow(k kinds.Kind, kind string, v VersionInput, cur *versionRow) (versionRow, error) {
	var r versionRow
	if cur != nil {
		r = *cur
	} else {
		r = versionRow{traits: []string{}, state: StatePublished, attributes: []byte(`{}`)}
	}
	var err error
	if v.Languages != nil || cur == nil {
		if r.languages, err = s.checkVersionLanguages(k, v.Languages); err != nil {
			return r, err
		}
	}
	if v.Traits != nil {
		if r.traits, err = s.checkTraits(k, kind, *v.Traits); err != nil {
			return r, err
		}
	}
	if v.Label != nil {
		if r.label = strings.TrimSpace(*v.Label); len([]rune(r.label)) > maxLabel {
			return r, invalid("label too long")
		}
	}
	switch v.State {
	case "":
	case StateDraft, StatePublished:
		r.state = v.State
	default:
		return r, invalid("version state %q must be draft or published", v.State)
	}
	if v.PublishAt != nil {
		r.publishAt = v.PublishAt
	}
	switch {
	case v.ClearAccess:
		r.access = nil
	case v.Access != nil:
		rule, err := s.checkRule(*v.Access)
		if err != nil {
			return r, err
		}
		r.access, _ = json.Marshal(rule)
	}
	if v.Attributes != nil {
		a, err := checkAttributes(k.Attributes, v.Attributes)
		if err != nil {
			return r, err
		}
		r.attributes, _ = json.Marshal(a)
	}
	if v.Ordinal != nil {
		r.ordinal = v.Ordinal
	}
	if v.Segments != nil && !k.Segments {
		return r, invalid("kind %s has no segments", kind)
	}
	return r, nil
}

func (s *Store) insertVersion(ctx context.Context, tx pgx.Tx, contentID, kind string, k kinds.Kind, v VersionInput) (string, error) {
	r, err := s.versionRow(k, kind, v, nil)
	if err != nil {
		return "", err
	}
	if err := s.checkVersionRule(ctx, tx, r.access); err != nil {
		return "", err
	}
	id := contentref.NewID()
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.versions+` (tenant_id, id, content_id, position, languages, traits, label, state, publish_at, published_at, access, attributes, ordinal)
 SELECT $1, $2, $3, coalesce((SELECT max(position) + 1 FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $3), 0), $4, $5, $6, $7, $8,
  CASE WHEN $7 = 'published' AND ($8::timestamptz IS NULL OR $8 <= $12) THEN coalesce($8, $12) END, $9, $10, $11`,
		s.tenant, id, contentID, r.languages, r.traits, r.label, r.state, r.publishAt, r.access, r.attributes, r.ordinal, s.now()); err != nil {
		return "", err
	}
	if err := s.writeVersionExtras(ctx, tx, id, v); err != nil {
		return "", err
	}
	return id, nil
}

// checkFork accepts a fork into a new version from a live sibling.
func (s *Store) checkFork(ctx context.Context, tx pgx.Tx, p VersionPut) error {
	if p.VersionID != "" {
		return invalid("a fork fills a new version")
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $2 AND id = $3 AND state <> 'deleted')`,
		s.tenant, p.ContentID, p.Fork.From).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return invalid("fork source %s is not a version of %s", p.Fork.From, p.ContentID)
	}
	return nil
}

func (s *Store) checkVersionRule(ctx context.Context, tx pgx.Tx, raw []byte) error {
	if raw == nil {
		return nil
	}
	var r access.Rule
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	return s.checkRuleGroups(ctx, tx, &r)
}

func (s *Store) updateVersion(ctx context.Context, tx pgx.Tx, contentID, kind string, k kinds.Kind, id string, v VersionInput) error {
	var cur versionRow
	err := tx.QueryRow(ctx, `SELECT languages, traits, label, state, publish_at, access, attributes, ordinal FROM `+s.t.versions+`
 WHERE tenant_id = $1 AND content_id = $2 AND id = $3 FOR UPDATE`, s.tenant, contentID, id).
		Scan(&cur.languages, &cur.traits, &cur.label, &cur.state, &cur.publishAt, &cur.access, &cur.attributes, &cur.ordinal)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && cur.state == StateDeleted && v.State == "") {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	was := cur.state
	r, err := s.versionRow(k, kind, v, &cur)
	if err != nil {
		return err
	}
	if err := s.checkVersionRule(ctx, tx, r.access); err != nil {
		return err
	}
	// A state or schedule change restarts the publication clock.
	restart := r.state != was || v.PublishAt != nil
	if _, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` SET languages = $4, traits = $5, label = $6, state = $7, publish_at = $8,
 published_at = CASE WHEN NOT $12 THEN published_at WHEN $7 = 'published' AND ($8::timestamptz IS NULL OR $8 <= $13) THEN coalesce($8, $13) END,
 deleted_at = CASE WHEN $7 = 'deleted' THEN deleted_at END, access = $9, attributes = $10, ordinal = $11, updated_at = clock_timestamp(), revision = revision + 1
 WHERE tenant_id = $1 AND content_id = $2 AND id = $3`,
		s.tenant, contentID, id, r.languages, r.traits, r.label, r.state, r.publishAt, r.access, r.attributes, r.ordinal, restart, s.now()); err != nil {
		return err
	}
	return s.writeVersionExtras(ctx, tx, id, v)
}

// writeVersionExtras replaces the version's text and segments when given.
func (s *Store) writeVersionExtras(ctx context.Context, tx pgx.Tx, id string, v VersionInput) error {
	if v.Text != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.versionText+` WHERE tenant_id = $1 AND version_id = $2`, s.tenant, id); err != nil {
			return err
		}
		for l, t := range v.Text {
			if err := s.checkLanguage(l); err != nil {
				return err
			}
			if len([]rune(t.Title)) > maxTitle || len([]rune(t.Description)) > maxDescription {
				return invalid("version text %s exceeds its limits", l)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.versionText+` (tenant_id, version_id, language, title, description) VALUES ($1, $2, $3, $4, $5)`,
				s.tenant, id, l, strings.TrimSpace(t.Title), strings.TrimSpace(t.Description)); err != nil {
				return err
			}
		}
	}
	if v.Segments != nil {
		segs, err := checkSegments(*v.Segments)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.segments+` WHERE tenant_id = $1 AND version_id = $2`, s.tenant, id); err != nil {
			return err
		}
		for _, sg := range segs {
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.segments+` (tenant_id, version_id, position, title, start_index, end_index, release_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				s.tenant, id, sg.Position, strings.TrimSpace(sg.Title), sg.Start, sg.End, sg.ReleaseAt); err != nil {
				return err
			}
		}
	}
	return nil
}

// DeleteVersions deletes versions; an item keeps at least one.
func (s *Store) DeleteVersions(ctx context.Context, actor access.Actor, keys []VersionKey) error {
	var ids []string
	for _, k := range keys {
		if !slices.Contains(ids, k.ContentID) {
			ids = append(ids, k.ContentID)
		}
	}
	return s.mutate(ctx, actor, ids, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{expose: true}
		for _, k := range keys {
			o := got[k.ContentID]
			if s.cfg.Kinds[o.kind].Versions.Hidden {
				return fx, invalid("kind %s has one hidden version", o.kind)
			}
			tag, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` SET state = 'deleted', deleted_at = clock_timestamp(), updated_at = clock_timestamp(), revision = revision + 1
 WHERE tenant_id = $1 AND content_id = $2 AND id = $3 AND state <> 'deleted'`, s.tenant, k.ContentID, k.VersionID)
			if err != nil {
				return fx, err
			}
			if tag.RowsAffected() == 0 {
				return fx, ErrNotFound
			}
			fx.events = append(fx.events, Event{Type: EventVersionDeleted, Kind: o.kind, ContentID: k.ContentID, VersionID: k.VersionID})
		}
		var orphan bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.items+` c WHERE c.tenant_id = $1 AND c.id = ANY($2)
 AND NOT EXISTS (SELECT 1 FROM `+s.t.versions+` v WHERE v.tenant_id = c.tenant_id AND v.content_id = c.id AND v.state <> 'deleted'))`, s.tenant, ids).Scan(&orphan); err != nil {
			return fx, err
		}
		if orphan {
			return fx, errors.Join(ErrConflict, errors.New("a content item keeps at least one version"))
		}
		return fx, nil
	})
}

// OrderVersions sets the order of a content item's versions: ids lists every
// version not deleted, first first; deleted ones follow.
func (s *Store) OrderVersions(ctx context.Context, actor access.Actor, contentID string, ids []string) error {
	return s.mutate(ctx, actor, []string{contentID}, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{}
		var live []string
		rows, err := tx.Query(ctx, `SELECT id FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $2 AND state <> 'deleted' ORDER BY id`, s.tenant, contentID)
		if err != nil {
			return fx, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fx, err
			}
			live = append(live, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fx, err
		}
		if !slices.Equal(live, slices.Sorted(slices.Values(ids))) {
			return fx, invalid("the order must list each version that is not deleted once")
		}
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` v SET position = CASE WHEN o.n IS NULL THEN $4 + v.position ELSE o.n - 1 END, updated_at = clock_timestamp()
 FROM `+s.t.versions+` w LEFT JOIN unnest($3::text[]) WITH ORDINALITY AS o(id, n) ON o.id = w.id
 WHERE v.tenant_id = $1 AND v.content_id = $2 AND w.tenant_id = v.tenant_id AND w.id = v.id`, s.tenant, contentID, ids, len(ids)); err != nil {
			return fx, err
		}
		fx.events = append(fx.events, Event{Type: EventContentUpdated, Kind: got[contentID].kind, ContentID: contentID})
		return fx, nil
	})
}

// screening is one item's moderation outcome for a write.
type screening struct {
	revision int64
	result   content.Screening
	ran      bool
}

// screenTexts screens the text each item will publish, outside any
// transaction: published items always, drafts only when publishing.
// pending is the text about to be written; all screens every language.
func (s *Store) screenTexts(ctx context.Context, actor access.Actor, pending map[string][]TextPut, all bool) (map[string]screening, error) {
	out := map[string]screening{}
	if s.moderator == nil {
		return out, nil
	}
	for id, puts := range pending {
		var kind, state string
		var revision int64
		err := s.pool.QueryRow(ctx, `SELECT kind, state, moderation_revision FROM `+s.t.items+` WHERE tenant_id = $1 AND id = $2`, s.tenant, id).Scan(&kind, &state, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if !all && state != StatePublished {
			continue
		}
		texts, err := s.readTexts(ctx, s.pool, []string{id})
		if err != nil {
			return nil, err
		}
		merged := texts[id]
		if merged == nil {
			merged = map[string]Text{}
		}
		for _, p := range puts {
			if p.Text == nil {
				delete(merged, p.Language)
			} else {
				merged[p.Language] = *p.Text
			}
		}
		var title, body strings.Builder
		for _, l := range slices.Sorted(maps.Keys(merged)) {
			t := merged[l]
			title.WriteString(t.Title + "\n")
			body.WriteString(strings.TrimSpace(t.Description+"\n"+t.Body) + "\n")
		}
		res, err := s.moderator.Screen(ctx, content.ModerationInput{Actor: actor, Ref: s.Ref(kind, id), Kind: HeldKind, ItemID: id,
			Title: strings.TrimSpace(title.String()), Text: strings.TrimSpace(body.String())})
		if err != nil {
			return nil, err
		}
		out[id] = screening{revision: revision, result: res, ran: true}
	}
	return out, nil
}

// checkScreened refuses a write whose screened text changed meanwhile.
func (s *Store) checkScreened(ctx context.Context, tx pgx.Tx, id string, sc screening) error {
	if !sc.ran {
		return nil
	}
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT moderation_revision FROM `+s.t.items+` WHERE tenant_id = $1 AND id = $2`, s.tenant, id).Scan(&revision); err != nil {
		return err
	}
	if revision != sc.revision {
		return errors.Join(ErrConflict, errors.New("content changed while being screened; retry"))
	}
	return nil
}

// applyScreening stores a screening: approval clears any hold; a hold keeps
// the last approved text of a live item as its published snapshot.
func (s *Store) applyScreening(ctx context.Context, tx pgx.Tx, id string, sc screening) error {
	if !sc.ran {
		return nil
	}
	r := sc.result
	if r.State == content.ModerationApproved {
		_, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET moderation = 'approved', moderation_reason = NULL, moderation_verdict = $3, published_snapshot = NULL,
 moderated_by = NULL, moderated_at = NULL, moderation_revision = moderation_revision + 1 WHERE tenant_id = $1 AND id = $2`, s.tenant, id, r.Verdict)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE `+s.t.items+` c SET moderation = $3, moderation_reason = $4, moderation_verdict = $5,
 moderated_by = NULL, moderated_at = NULL, moderation_revision = moderation_revision + 1,
 published_snapshot = CASE WHEN c.published_snapshot IS NOT NULL OR c.moderation <> 'approved' OR c.published_at IS NULL THEN c.published_snapshot
  ELSE (SELECT coalesce(jsonb_object_agg(t.language, jsonb_build_object('title', t.title, 'slug', t.slug, 'description', t.description,
   'body', t.body, 'sort_key', t.sort_key, 'aliases', to_jsonb(t.aliases))), '{}') FROM `+s.t.text+` t WHERE t.tenant_id = c.tenant_id AND t.content_id = c.id) END
 WHERE c.tenant_id = $1 AND c.id = $2`, s.tenant, id, r.State, r.Reason, r.Verdict)
	return err
}
