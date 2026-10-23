package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
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
	Text             map[string]Text `json:"text,omitempty"`
	OriginalTitle    string          `json:"original_title,omitempty"`
	OriginalLanguage string          `json:"original_language,omitempty"`
	// Comments is open (the default when permitted), closed (existing ones
	// show, new ones are refused) or hidden. A kind excluding open defaults to
	// its first configured comment mode.
	Comments string `json:"comments,omitempty"`
	// Access defaults to public when permitted, otherwise the kind's first
	// allowed level (and the first configured tier for tier access).
	Access       *access.Rule   `json:"access,omitempty"`
	Restrictions []string       `json:"restrictions,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
	// OwnerEntityID is the home entity (a channel); its editors edit the item.
	// It never changes.
	OwnerEntityID string `json:"owner_group_id,omitempty"`
	// Creators nil credits the creating person as its author.
	Creators *[]Creator `json:"creators,omitempty"`
	// Versions: none gives the item one serving its text's languages (a
	// kind with hidden versions takes none).
	Versions []VersionInput `json:"versions,omitempty"`
}

// ContentPatch edits a content item; nil fields stay.
type ContentPatch struct {
	OriginalTitle    *string        `json:"original_title,omitempty"`
	OriginalLanguage *string        `json:"original_language,omitempty"`
	Comments         *string        `json:"comments,omitempty"`
	Access           *access.Rule   `json:"access,omitempty"`
	Restrictions     *[]string      `json:"restrictions,omitempty"`
	Attributes       map[string]any `json:"attributes,omitempty"` // replaces all
	Creators         *[]Creator     `json:"creators,omitempty"`
	// Revision, when set, refuses the edit (409) unless it is current.
	Revision *int64 `json:"revision,omitempty"`
}

// VersionInput creates or edits a version; nil fields stay (or default).
type VersionInput struct {
	Languages []string  `json:"languages,omitempty"`
	Traits    *[]string `json:"traits,omitempty"`
	Label     *string   `json:"label,omitempty"`
	// State is draft or published: a published version is live once its
	// content is and PublishAt passed. A version created with its item
	// defaults to published, one added later to draft.
	State     string     `json:"state,omitempty"`
	PublishAt *time.Time `json:"publish_at,omitempty"`
	// Access overrides the content's rule; ClearAccess inherits it again.
	Access      *access.Rule           `json:"access,omitempty"`
	ClearAccess bool                   `json:"clear_access,omitempty"`
	Attributes  map[string]any         `json:"attributes,omitempty"` // replaces all
	Text        map[string]VersionText `json:"text,omitempty"`       // replaces all
	Segments    *[]SegmentInput        `json:"segments,omitempty"`   // replaces all
	Ordinal     *int                   `json:"ordinal,omitempty"`
	// Cover selects one of the version's cover uploads by name (i-{uuid});
	// "" returns to its first.
	Cover *string `json:"cover,omitempty"`
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
	bodies    []versions.BodyCopy
}

// mutate runs fn over the authorized, locked items in one transaction, then
// bumps their revisions, refreshes facets and documents, re-codes, emits
// events and queues media exposure before committing.
func (s *Store) mutate(ctx context.Context, actor access.Actor, ids []string, fn func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error)) error {
	g, err := s.grantsFor(ctx, actor, ids)
	if err != nil {
		return err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	got, err := s.authorize(ctx, tx, actor, ids, g)
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
	if err := s.retireOffers(ctx, tx, fx.events); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET revision = revision + 1, updated_at = clock_timestamp() WHERE tenant_id = $1 AND id = ANY($2)`, s.tenant, ids); err != nil {
		return err
	}
	if fx.codes {
		if err := s.putCodes(ctx, tx, ids); err != nil {
			return err
		}
	}
	if err := s.emit(ctx, tx, fx.events...); err != nil {
		return err
	}
	if err := s.RefreshFacets(ctx, tx, ids); err != nil {
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
	if s.media != nil && len(fx.bodies) > 0 {
		if err := s.media.Versions.CopyBodiesTx(ctx, tx, fx.bodies); err != nil {
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
// actor needs root:content:write, or to edit each item's owner entity.
func (s *Store) Create(ctx context.Context, actor access.Actor, in []ContentInput) ([]string, error) {
	prepared, err := s.PrepareCreate(ctx, actor, in)
	if err != nil {
		return nil, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	ids, err := prepared.CreateTx(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	return ids, tx.Commit(ctx)
}

type preparedCreateContent struct {
	ContentInput
	id       string
	kind     kinds.Kind
	creators []Creator
	versions []VersionInput
}

// CreateAuthorization is one fully validated, in-process creation plan.
// PrepareCreate performs permission queries and body processing before a
// transaction reserves a pool connection. The plan is not serializable and
// must be used immediately with exactly the inputs that were prepared.
type CreateAuthorization struct {
	store *Store
	actor access.Actor
	input []byte
	preps []preparedCreateContent
}

func (s *Store) PrepareCreate(ctx context.Context, actor access.Actor, in []ContentInput) (*CreateAuthorization, error) {
	input, err := json.Marshal(in)
	if err != nil {
		return nil, ErrInvalid
	}
	preps := make([]preparedCreateContent, len(in))
	var owners []string
	for i, c := range in {
		p := preparedCreateContent{ContentInput: c, id: contentref.NewID()}
		var err error
		if p.kind, err = s.kind(c.Kind); err != nil {
			return nil, err
		}
		if p.OriginalTitle, p.OriginalLanguage, err = cleanOriginalTitle(p.OriginalTitle, p.OriginalLanguage); err != nil {
			return nil, err
		}
		if p.Comments == "" {
			p.Comments = s.defaultComments(c.Kind)
		}
		if err := checkComments(p.Comments); err != nil {
			return nil, err
		}
		if err := s.checkKindComments(c.Kind, p.Comments); err != nil {
			return nil, err
		}
		rule := s.defaultRule(c.Kind)
		if c.Access != nil {
			rule = *c.Access
		}
		if rule, err = s.checkRule(rule); err != nil {
			return nil, err
		}
		if err := s.checkKindRule(c.Kind, rule); err != nil {
			return nil, err
		}
		p.Access = &rule
		if p.Restrictions, err = s.checkRestrictions(c.Restrictions); err != nil {
			return nil, err
		}
		if p.Attributes, err = checkAttributes(p.kind.Attributes, c.Attributes); err != nil {
			return nil, err
		}
		if c.Attributes != nil {
			if err := kinds.ValidateRequiredAttributes(p.kind.Attributes, p.Attributes); err != nil {
				return nil, invalid("%v", err)
			}
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
		case actor.Member():
			p.creators = []Creator{{UserID: actor.AccountID(), Role: "author"}}
		}
		if err != nil {
			return nil, err
		}
		switch {
		case p.kind.Versions.Hidden && len(c.Versions) > 0:
			return nil, invalid("kind %s has one hidden version; give it no versions", c.Kind)
		case len(c.Versions) == 0:
			p.versions = []VersionInput{{Languages: s.textLanguages(p.kind, texts)}}
		default:
			p.versions = c.Versions
		}
		if slices.ContainsFunc(p.versions, func(v VersionInput) bool { return v.Fork != nil }) {
			return nil, invalid("a new item's versions have no sibling to fork")
		}
		owners = append(owners, c.OwnerEntityID)
		preps[i] = p
	}
	if err := s.authorizeCreate(ctx, s.pool, actor, owners); err != nil {
		return nil, err
	}
	return &CreateAuthorization{store: s, actor: actor, input: input, preps: preps}, nil
}

// CreateTx writes this prepared creation in the caller-owned transaction.
// It never invokes a body processor or the authorization provider.
func (a *CreateAuthorization) CreateTx(ctx context.Context, tx pgx.Tx, in []ContentInput) ([]string, error) {
	if a == nil || a.store == nil || tx == nil {
		return nil, ErrForbidden
	}
	input, err := json.Marshal(in)
	if err != nil || !bytes.Equal(input, a.input) {
		return nil, ErrForbidden
	}
	return a.store.createPreparedTx(ctx, tx, a.actor, a.preps)
}

func (s *Store) createPreparedTx(ctx context.Context, tx pgx.Tx, actor access.Actor, preps []preparedCreateContent) ([]string, error) {
	ids := make([]string, len(preps))
	var events []Event
	for i, p := range preps {
		if err := s.checkRuleEntities(ctx, tx, p.Access); err != nil {
			return nil, err
		}
		if p.OwnerEntityID != "" {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.entities+` WHERE tenant_id = $1 AND id = $2 AND state = 'active')`, s.tenant, p.OwnerEntityID).Scan(&ok); err != nil {
				return nil, err
			}
			if !ok {
				return nil, invalid("owner group %s does not exist", p.OwnerEntityID)
			}
		}
		rule, _ := json.Marshal(p.Access)
		attrs, _ := json.Marshal(p.Attributes)
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.items+` (tenant_id, id, kind, original_title, original_language, access, restrictions, attributes, owner_entity_id, created_by, comments)
 VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6, $7, $8, nullif($9, ''), nullif($10, ''), $11)`,
			s.tenant, p.id, p.Kind, p.OriginalTitle, p.OriginalLanguage, rule, p.Restrictions, attrs, p.OwnerEntityID, viewerID(actor), p.Comments); err != nil {
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
	return ids, nil
}

// textLanguages are the languages a default or hidden version serves: its
// content's text languages (the first in site order for a one-language kind),
// else the site's first language.
func (s *Store) textLanguages(k kinds.Kind, texts map[string]Text) []string {
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
		if u.OriginalTitle != nil {
			title, _, err := cleanOriginalTitle(*u.OriginalTitle, "")
			if err != nil {
				return err
			}
			in[i].OriginalTitle = &title
		}
		if u.OriginalLanguage != nil {
			_, language, err := cleanOriginalTitle("", *u.OriginalLanguage)
			if err != nil {
				return err
			}
			in[i].OriginalLanguage = &language
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
	}
	return s.mutate(ctx, actor, ids, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{expose: true}
		for _, u := range in {
			o := got[u.ID]
			if u.Comments != nil {
				if err := s.checkKindComments(o.kind, *u.Comments); err != nil {
					return fx, err
				}
			}
			if u.Access != nil {
				if err := s.checkKindRule(o.kind, *u.Access); err != nil {
					return fx, err
				}
			}
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
			if err := s.checkRuleEntities(ctx, tx, u.Access); err != nil {
				return fx, err
			}
			var access, attrs []byte
			if u.Access != nil {
				access, _ = json.Marshal(u.Access)
			}
			if u.Attributes != nil {
				a, err := checkAttributes(s.cfg.Kinds[o.kind].Attributes, u.Attributes)
				if err != nil {
					return fx, err
				}
				if err := kinds.ValidateRequiredAttributes(s.cfg.Kinds[o.kind].Attributes, a); err != nil {
					return fx, invalid("%v", err)
				}
				attrs, _ = json.Marshal(a)
			}
			var restrictions []string
			if u.Restrictions != nil {
				restrictions = *u.Restrictions
			}
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET original_title = coalesce($3, original_title), original_language = coalesce($4, original_language), access = coalesce($5, access),
 restrictions = coalesce($6, restrictions), attributes = coalesce($7, attributes), comments = coalesce($8, comments) WHERE tenant_id = $1 AND id = $2`,
				s.tenant, u.ID, u.OriginalTitle, u.OriginalLanguage, access, restrictions, attrs, u.Comments); err != nil {
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

// putCodes gives each item its content code and decorative path per language:
// its kind's path entity slug, if any, then its title slug (the site's first
// language with text is the default).
func (s *Store) putCodes(ctx context.Context, q entities.Querier, ids []string) error {
	rows, err := q.Query(ctx, `SELECT c.id, c.kind, t.language, t.slug FROM `+s.t.items+` c
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
	paths, err := s.pathEntities(ctx, q, order)
	if err != nil {
		return err
	}
	out := make([]codes.Entry, 0, len(order))
	for _, id := range order {
		e := entries[id]
		entity := paths[id]
		def := ""
		for _, l := range s.cfg.Languages {
			if _, ok := e.Slugs[l]; ok {
				def = l
				break
			}
		}
		e.Slug = decorativePath(entity.slugFor(def), e.Slugs[def])
		for l, title := range e.Slugs {
			e.Slugs[l] = decorativePath(entity.slugFor(l), title)
		}
		out = append(out, *e)
	}
	_, err = codes.Put(ctx, codes.Pgx(q), s.qs, s.tenant, out)
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
 WHERE tenant_id = $1 AND content_id = $2 AND languages <> $3`, s.tenant, id, s.textLanguages(k, texts))
	return err
}

// Publish publishes content items now, or schedules those with a future At.
// A published item is screened (all its text); a hold leaves it unpublished
// until a reviewer approves it. Rescheduling a scheduled item moves its time;
// a live item is left as it is.
func (s *Store) Publish(ctx context.Context, actor access.Actor, in []Publish) error {
	return s.PublishChecked(ctx, actor, in, nil)
}

// PublishChecked is Publish with a transaction-local precondition. The check
// runs after ordinary authorization and content locks, before any publication
// mutation. It must use tx for database reads and must not open transactions.
func (s *Store) PublishChecked(ctx context.Context, actor access.Actor, in []Publish, check func(context.Context, pgx.Tx) error) error {
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
		if check != nil {
			if err := check(ctx, tx); err != nil {
				return fx, err
			}
		}
		if err := s.checkStoredPolicies(ctx, tx, ids); err != nil {
			return fx, err
		}
		// A scheduled item goes live at its time without another check, so
		// scheduling needs ready media too.
		var going []string
		for _, p := range in {
			if o := got[p.ContentID]; o.state != StateDeleted && !(o.state == StatePublished && o.publishedAt) {
				going = append(going, p.ContentID)
			}
		}
		targets, err := s.publishedVersions(ctx, tx, going)
		if err != nil {
			return fx, err
		}
		if err := s.readyTx(ctx, tx, targets); err != nil {
			return fx, err
		}
		for _, p := range in {
			o := got[p.ContentID]
			switch {
			case o.state == StateDeleted:
				return fx, ErrNotFound
			case o.state == StatePublished && o.publishedAt:
				continue
			}
			var titled, versioned bool
			originalAndVersion := s.cfg.Kinds[o.kind].TitlePolicy == kinds.TitleOriginalAndVersion
			if err := tx.QueryRow(ctx, `SELECT
 EXISTS (SELECT 1 FROM `+s.t.text+` WHERE tenant_id=$1 AND content_id=$2 AND title<>'') OR
 ($3 AND (EXISTS (SELECT 1 FROM `+s.t.items+` WHERE tenant_id=$1 AND id=$2 AND coalesce(original_title,'')<>'') OR
 EXISTS (SELECT 1 FROM `+s.t.versions+` v JOIN `+s.t.versionText+` t ON t.tenant_id=v.tenant_id AND t.version_id=v.id
 WHERE v.tenant_id=$1 AND v.content_id=$2 AND v.state<>'deleted' AND t.language=ANY(v.languages) AND t.title<>''))),
 EXISTS (SELECT 1 FROM `+s.t.versions+` WHERE tenant_id=$1 AND content_id=$2 AND state<>'deleted')`, s.tenant, p.ContentID, originalAndVersion).Scan(&titled, &versioned); err != nil {
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

// Delete deletes a hidden-version kind or an already empty content item.
// Visible-version kinds are deleted by deleting their versions instead.
func (s *Store) Delete(ctx context.Context, actor access.Actor, ids []string) error {
	return s.deleteContent(ctx, actor, ids, false)
}

// DeleteForModeration hides content without changing its editions. This
// embedded operation has no public delete route. Existing purchaser access
// and retention protections still apply.
func (s *Store) DeleteForModeration(ctx context.Context, actor access.Actor, ids []string) error {
	return s.deleteContent(ctx, actor, ids, true)
}

func (s *Store) deleteContent(ctx context.Context, actor access.Actor, ids []string, moderation bool) error {
	return s.mutate(ctx, actor, ids, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{expose: true}
		for _, id := range ids {
			o := got[id]
			if o.state == StateDeleted {
				if moderation {
					changed, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET deleted_without_versions=false WHERE tenant_id=$1 AND id=$2 AND deleted_without_versions`, s.tenant, id)
					if err != nil {
						return fx, err
					}
					if changed.RowsAffected() > 0 {
						fx.events = append(fx.events, Event{Type: EventContentDeleted, Kind: o.kind, ContentID: id})
						continue
					}
				}
				return fx, ErrNotFound
			}
			auto := !moderation && !s.cfg.Kinds[o.kind].Versions.Hidden
			if auto {
				var nonempty bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.versions+` WHERE tenant_id=$1 AND content_id=$2 AND state<>'deleted')`, s.tenant, id).Scan(&nonempty); err != nil {
					return fx, err
				}
				if nonempty {
					return fx, errors.Join(ErrConflict, errors.New("delete the content's versions instead"))
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET state = 'deleted', deleted_at = clock_timestamp(),deleted_without_versions=$3 WHERE tenant_id = $1 AND id = $2`, s.tenant, id, auto); err != nil {
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
			var emptyCause bool
			if err := tx.QueryRow(ctx, `SELECT deleted_without_versions FROM `+s.t.items+` WHERE tenant_id=$1 AND id=$2`, s.tenant, id).Scan(&emptyCause); err != nil {
				return fx, err
			}
			if emptyCause {
				return fx, errors.Join(ErrConflict, errors.New("restore a version to restore this content"))
			}
			if err := s.restoreRows(ctx, tx, []string{id}); err != nil {
				return fx, err
			}
			fx.events = append(fx.events, Event{Type: EventContentRestored, Kind: o.kind, ContentID: id})
		}
		return fx, nil
	})
}

// restoreRows brings deleted items back as they were: published (or
// scheduled) when they had been, else drafts.
func (s *Store) restoreRows(ctx context.Context, tx pgx.Tx, ids []string) error {
	// A fixed comment policy is applied only when explicitly restoring a
	// deleted item. Retained tombstones are not rewritten on configuration load.
	for kind, spec := range s.cfg.Kinds {
		if len(spec.CommentModes) == 1 {
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET comments = $3 WHERE tenant_id = $1 AND id = ANY($2) AND kind = $4`, s.tenant, ids, spec.CommentModes[0], kind); err != nil {
				return err
			}
		}
	}
	if err := s.checkStoredPolicies(ctx, tx, ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET deleted_at = NULL, deleted_with_entity = NULL,deleted_without_versions=false,
 state = CASE WHEN published_at IS NOT NULL OR publish_at IS NOT NULL THEN 'published' ELSE 'draft' END WHERE tenant_id = $1 AND id = ANY($2)`, s.tenant, ids)
	return err
}

// ownedBatch bounds the content one owned-content transaction locks.
const ownedBatch = 100

// DeleteOwned deletes the live content of the deleted user-owned entities
// among entities (every one with nil), recording each entity as its content's
// cause (entities.OwnedContent): holders of a deleted item's key still read
// it until the content is erased after the deletion retention.
func (s *Store) DeleteOwned(ctx context.Context, entities []string) error {
	return s.followOwned(ctx, `SELECT c.id, c.kind, g.id FROM `+s.t.items+` c JOIN `+s.t.entities+` g ON g.tenant_id = c.tenant_id AND g.id = c.owner_entity_id
 WHERE c.tenant_id = $1 AND g.state = 'deleted' AND g.kind = ANY($2) AND ($3::text[] IS NULL OR g.id = ANY($3)) AND c.state <> 'deleted'
 ORDER BY c.id LIMIT `+strconv.Itoa(ownedBatch)+` FOR UPDATE OF c FOR SHARE OF g`, entities, EventContentDeleted,
		func(ctx context.Context, tx pgx.Tx, ids, causes []string) error {
			_, err := tx.Exec(ctx, `UPDATE `+s.t.items+` c SET state = 'deleted', deleted_at = clock_timestamp(), deleted_with_entity = r.cause,deleted_without_versions=false
 FROM unnest($2::text[], $3::text[]) AS r(id, cause) WHERE c.tenant_id = $1 AND c.id = r.id`, s.tenant, ids, causes)
			return err
		})
}

// RestoreOwned restores the content the active entities among entities (every
// one with nil) deleted when they were deleted (entities.OwnedContent), as
// Restore does; content deleted for another reason stays deleted, and
// content purged since is gone.
func (s *Store) RestoreOwned(ctx context.Context, entities []string) error {
	return s.followOwned(ctx, `SELECT c.id, c.kind, g.id FROM `+s.t.items+` c JOIN `+s.t.entities+` g ON g.tenant_id = c.tenant_id AND g.id = c.deleted_with_entity
 WHERE c.tenant_id = $1 AND g.state = 'active' AND g.kind = ANY($2) AND ($3::text[] IS NULL OR g.id = ANY($3)) AND c.state = 'deleted'
 ORDER BY c.id LIMIT `+strconv.Itoa(ownedBatch)+` FOR UPDATE OF c FOR SHARE OF g`, entities, EventContentRestored,
		func(ctx context.Context, tx pgx.Tx, ids, _ []string) error { return s.restoreRows(ctx, tx, ids) })
}

// followOwned writes, a batch at a time, the content items query selects
// (id, kind, entity) with write, each batch in one transaction with the
// write's effects.
func (s *Store) followOwned(ctx context.Context, query string, entities []string, typ string,
	write func(ctx context.Context, tx pgx.Tx, ids, causes []string) error) error {
	var userKinds []string
	for name, gk := range s.cfg.EntityKinds {
		if gk.Owner == "user" {
			userKinds = append(userKinds, name)
		}
	}
	if len(userKinds) == 0 {
		return nil
	}
	for {
		tx, err := s.begin(ctx)
		if err != nil {
			return err
		}
		ids, causes, events, err := func() ([]string, []string, []Event, error) {
			rows, err := tx.Query(ctx, query, s.tenant, userKinds, entities)
			if err != nil {
				return nil, nil, nil, err
			}
			defer rows.Close()
			var ids, causes []string
			var events []Event
			for rows.Next() {
				var id, kind, entity string
				if err := rows.Scan(&id, &kind, &entity); err != nil {
					return nil, nil, nil, err
				}
				ids, causes = append(ids, id), append(causes, entity)
				events = append(events, Event{Type: typ, Kind: kind, ContentID: id})
			}
			return ids, causes, events, rows.Err()
		}()
		if err == nil && len(ids) > 0 {
			if err = write(ctx, tx, ids, causes); err == nil {
				if err = s.apply(ctx, tx, ids, effects{events: events, expose: true}); err == nil {
					err = tx.Commit(ctx)
				}
			}
		}
		_ = tx.Rollback(ctx)
		if err != nil || len(ids) == 0 {
			return err
		}
	}
}

// PutVersions creates (VersionID "") or edits versions and returns their ids
// in order.
func (s *Store) PutVersions(ctx context.Context, actor access.Actor, in []VersionPut) ([]string, error) {
	var segmentVersions []string
	for _, p := range in {
		if p.VersionID != "" && p.Segments != nil {
			segmentVersions = append(segmentVersions, p.VersionID)
		}
	}
	unlock, err := versions.LockSources(ctx, s.pool, s.tenant, segmentVersions)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var ids []string
	for _, p := range in {
		if !slices.Contains(ids, p.ContentID) {
			ids = append(ids, p.ContentID)
		}
	}
	out := make([]string, len(in))
	err = s.mutate(ctx, actor, ids, func(ctx context.Context, tx pgx.Tx, got map[string]owned) (effects, error) {
		fx := effects{expose: true}
		for i, p := range in {
			o := got[p.ContentID]
			k := s.cfg.Kinds[o.kind]
			if o.state == StateDeleted {
				var automatic bool
				if err := tx.QueryRow(ctx, `SELECT c.deleted_without_versions AND (c.owner_entity_id IS NULL OR EXISTS
 (SELECT 1 FROM `+s.t.entities+` g WHERE g.tenant_id=c.tenant_id AND g.id=c.owner_entity_id AND g.state='active'))
 FROM `+s.t.items+` c WHERE c.tenant_id=$1 AND c.id=$2`, s.tenant, p.ContentID).Scan(&automatic); err != nil {
					return fx, err
				}
				if !automatic {
					return fx, ErrNotFound
				}
			}
			if k.Versions.Hidden && (p.VersionID == "" || !coverOnly(p.VersionInput)) {
				return fx, invalid("kind %s has one hidden version: only its cover is edited", o.kind)
			}
			if p.Fork != nil {
				if err := s.checkFork(ctx, tx, p); err != nil {
					return fx, err
				}
			}
			if p.VersionID == "" {
				// An added version stays a draft until its editors publish it.
				in := p.VersionInput
				if in.State == "" {
					in.State = StateDraft
				}
				vid, err := s.insertVersion(ctx, tx, p.ContentID, o.kind, k, in)
				if err != nil {
					return fx, err
				}
				out[i] = vid
				from := ""
				if f := p.Fork; f != nil {
					fx.forks = append(fx.forks, versions.Fork{Version: versions.Target{Content: p.ContentID, Version: vid}, From: f.From,
						Playback: f.Playback, Segments: f.Segments})
					from = f.From
				}
				// A new version starts with its own copy of the body's images
				// and, forked, of its source's cover selection.
				if from, err = s.sibling(ctx, tx, p.ContentID, vid, from, in.Cover == nil); err != nil {
					return fx, err
				}
				if from != "" {
					fx.bodies = append(fx.bodies, versions.BodyCopy{Content: p.ContentID, From: from, To: []string{vid}})
				}
				fx.events = append(fx.events, Event{Type: EventVersionCreated, Kind: o.kind, ContentID: p.ContentID, VersionID: vid})
				continue
			}
			if (o.state == StatePublished || o.state == StateDeleted && (o.publishedAt || o.publishAt)) && (p.State == StatePublished || p.State == "" && p.PublishAt != nil) {
				var was string
				if err := tx.QueryRow(ctx, `SELECT state FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $2 AND id = $3`,
					s.tenant, p.ContentID, p.VersionID).Scan(&was); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return fx, err
				}
				if (p.State == StatePublished || was == StatePublished) && (was != StatePublished || p.PublishAt != nil) {
					if err := s.readyTx(ctx, tx, []versions.Target{{Content: p.ContentID, Version: p.VersionID}}); err != nil {
						return fx, err
					}
				}
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
		restored, err := s.restoreVersionParents(ctx, tx, ids)
		if err != nil {
			return fx, err
		}
		fx.events = append(fx.events, restored...)
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
	cover             []byte
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
		if err := s.checkKindRule(kind, rule); err != nil {
			return r, err
		}
		r.access, _ = json.Marshal(rule)
	}
	if r.access != nil {
		var rule access.Rule
		if err := json.Unmarshal(r.access, &rule); err != nil {
			return r, err
		}
		if err := s.checkKindRule(kind, rule); err != nil {
			return r, err
		}
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
	switch {
	case v.Cover == nil:
	case *v.Cover == "":
		r.cover = nil
	case !media.ValidNamed(*v.Cover):
		return r, invalid("cover must be an image name (i-{uuid})")
	default:
		r.cover, _ = json.Marshal(Cover{Image: *v.Cover})
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
	if k.TitlePolicy == kinds.TitleOriginalAndVersion {
		v.Text, err = s.seedVersionTitles(ctx, tx, contentID, r.languages, v.Text)
		if err != nil {
			return "", err
		}
	}
	id := contentref.NewID()
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.versions+` (tenant_id, id, content_id, position, languages, traits, label, state, publish_at, published_at, access, attributes,
 ordinal, cover, playable)
 SELECT $1, $2, $3, coalesce((SELECT max(position) + 1 FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $3), 0), $4, $5, $6, $7, $8,
  CASE WHEN $7 = 'published' AND ($8::timestamptz IS NULL OR $8 <= $12) THEN coalesce($8, $12) END, $9, $10, $11, $13, $14`,
		s.tenant, id, contentID, r.languages, r.traits, r.label, r.state, r.publishAt, r.access, r.attributes, r.ordinal, s.now(), r.cover,
		s.startsUnplayable(kind)); err != nil {
		return "", err
	}
	if err := s.writeVersionExtras(ctx, tx, id, v); err != nil {
		return "", err
	}
	return id, nil
}

// coverOnly reports an edit of nothing but the cover.
func coverOnly(v VersionInput) bool {
	v.Cover = nil
	return reflect.DeepEqual(v, VersionInput{})
}

// sibling is the version new version vid copies its body images from: fork,
// else its content's first other version ("" when it has none). With cover
// a fork's source's cover selection is copied too.
func (s *Store) sibling(ctx context.Context, tx pgx.Tx, contentID, vid, fork string, cover bool) (string, error) {
	if fork != "" && cover {
		_, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` v SET cover = f.cover FROM `+s.t.versions+` f
 WHERE v.tenant_id = $1 AND v.id = $2 AND f.tenant_id = $1 AND f.id = $3`, s.tenant, vid, fork)
		return fork, err
	}
	if fork != "" {
		return fork, nil
	}
	var from string
	err := tx.QueryRow(ctx, `SELECT id FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $2 AND id <> $3 AND state <> 'deleted'
 ORDER BY position, id LIMIT 1`, s.tenant, contentID, vid).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return from, err
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

// checkVersionRule checks a version's override (stored JSON, nil for none).
func (s *Store) checkVersionRule(ctx context.Context, tx pgx.Tx, raw []byte) error {
	if raw == nil {
		return nil
	}
	var r access.Rule
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	return s.checkRuleEntities(ctx, tx, &r)
}

func (s *Store) updateVersion(ctx context.Context, tx pgx.Tx, contentID, kind string, k kinds.Kind, id string, v VersionInput) error {
	var cur versionRow
	err := tx.QueryRow(ctx, `SELECT languages, traits, label, state, publish_at, access, attributes, ordinal, cover FROM `+s.t.versions+`
 WHERE tenant_id = $1 AND content_id = $2 AND id = $3 FOR UPDATE`, s.tenant, contentID, id).
		Scan(&cur.languages, &cur.traits, &cur.label, &cur.state, &cur.publishAt, &cur.access, &cur.attributes, &cur.ordinal, &cur.cover)
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
 deleted_at = CASE WHEN $7 = 'deleted' THEN deleted_at END, access = $9, attributes = $10, ordinal = $11, cover = $14, updated_at = clock_timestamp(),
 revision = revision + 1
 WHERE tenant_id = $1 AND content_id = $2 AND id = $3`,
		s.tenant, contentID, id, r.languages, r.traits, r.label, r.state, r.publishAt, r.access, r.attributes, r.ordinal, restart, s.now(), r.cover); err != nil {
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
		segs, err := checkSegmentInputs(*v.Segments)
		if err != nil {
			return err
		}
		if len(segs) > 0 && segs[0].StartPage != "" {
			if err := s.validateChapterStarts(ctx, tx, id, segs); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.segments+` WHERE tenant_id = $1 AND version_id = $2`, s.tenant, id); err != nil {
			return err
		}
		for _, sg := range segs {
			var start, end any = sg.Start, sg.End
			var startPage any
			if sg.StartPage != "" {
				start, end, startPage = nil, nil, sg.StartPage
			}
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.segments+` (tenant_id, version_id, position, title, start_index, end_index, release_at, start_page) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				s.tenant, id, sg.Position, strings.TrimSpace(sg.Title), start, end, sg.ReleaseAt, startPage); err != nil {
				return err
			}
		}
	}
	return nil
}

// DeleteVersions soft-deletes editions. Removing the final undeleted edition
// also soft-deletes its parent in the same transaction.
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
		deleted, err := s.deleteEmptyParents(ctx, tx, ids)
		if err != nil {
			return fx, err
		}
		fx.events = append(fx.events, deleted...)
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
