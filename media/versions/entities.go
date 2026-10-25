package versions

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/edition"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
)

// A entity kind's items, {kind, entity}, hold the entity's named covers (and a
// user-owned entity's avatars): staff holding root:entities:write edit them,
// and for a user-owned entity its owner and its AuthKit scope's holders of
// {kind}:media:write or {kind}:settings:manage.

// groupRight is what one actor may do with one entity's item.
type entityRight struct {
	active, editor, staff bool
	internal              bool
	owner                 string
}

// groupRights loads, per entity id of kind, the entity's state and actor's
// right to edit its media.
func (s *Service) entityRights(ctx context.Context, actor access.Actor, kind string, ids []string) (map[string]entityRight, error) {
	out := map[string]entityRight{}
	rows, err := s.o.Pool.Query(ctx, `SELECT id, state = 'active', internal, coalesce(owner_user_id, ''), coalesce(auth_scope, '')
FROM `+s.t.entities+` WHERE tenant_id = $1 AND kind = $2 AND id = ANY($3)`, s.o.Tenant, kind, ids)
	if err != nil {
		return nil, err
	}
	type row struct {
		id, owner, scope string
		active           bool
		internal         bool
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.active, &r.internal, &r.owner, &r.scope); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil || len(found) == 0 {
		return out, err
	}
	staff := false
	if actor.Person() || actor.Kind == auth.SubjectApplication {
		if staff, err = gate.Can(ctx, s.o.Auth, actor, string(access.EntitiesWrite)); err != nil {
			return nil, err
		}
	}
	scoped := s.o.Kinds.EntityKinds[kind].Owner == "user" || s.o.Kinds.EntityKinds[kind].AuthScope
	for _, r := range found {
		right := entityRight{active: r.active, internal: r.internal, staff: staff, owner: r.owner}
		switch {
		case staff:
			right.editor = true
		case scoped && !r.internal && (!s.o.Kinds.EntityKinds[kind].AuthScope || r.scope != ""):
			if right.editor, err = gate.EntityEditor(ctx, s.o.Auth, actor, r.owner, r.scope, access.EntityMediaWrite(kind), access.EntitySettingsManage(kind)); err != nil {
				return nil, err
			}
		}
		out[r.id] = right
	}
	return out, nil
}

func (s *Service) isEntityKind(kind string) bool {
	_, ok := s.o.Kinds.EntityKinds[kind]
	return ok
}

// resolveEntities shows an active entity's item to everyone, its public files
// only, and its private files to its editors.
func (s *Service) resolveEntities(ctx context.Context, refs []contentref.ContentRef, actor access.Actor, out map[contentref.ContentKey]resolve.Resolution) error {
	byKind := map[string][]string{}
	for _, ref := range refs {
		byKind[ref.ContentKind] = append(byKind[ref.ContentKind], ref.ContentID)
	}
	rights := map[string]map[string]entityRight{}
	for kind, ids := range byKind {
		var err error
		if rights[kind], err = s.entityRights(ctx, actor, kind, ids); err != nil {
			return err
		}
	}
	for _, ref := range refs {
		if r, ok := rights[ref.ContentKind][ref.ContentID]; ok && r.active && (!r.internal || r.staff && resolve.EditorRead(ctx)) {
			out[ref.Key()] = resolve.Resolution{Visible: true, Editor: r.editor}
		}
	}
	return nil
}

func (s *Service) canEditEntity(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	rights, err := s.entityRights(ctx, actor, t.Ref.ContentKind, []string{t.Ref.ContentID})
	if err != nil {
		return media.UploadGrant{}, err
	}
	r, ok := rights[t.Ref.ContentID]
	if !ok || !r.active || !r.editor {
		return media.UploadGrant{}, nil
	}
	return media.UploadGrant{Allowed: true, Exempt: r.staff, Owner: EntityOwner(t.Ref.ContentKind, t.Ref.ContentID)}, nil
}

// EntityOwner is the quota owner of an entity's uploads: "{kind}:{id}".
func EntityOwner(kind, id string) string { return kind + ":" + id }

// ParseEntityOwner is EntityOwner's kind and id; ok is false for any other
// owner.
func ParseEntityOwner(owner string) (kind, id string, ok bool) {
	kind, id, ok = strings.Cut(owner, ":")
	return kind, id, ok && kind != "" && id != ""
}

// CoverOf names a content's or an entity's cover: Kind and ID are its owner;
// Image an image uploaded to the owner's item ("i-{uuid}"), else ContentID a
// content whose cover it shows; with neither, a content owner's own. A
// content's cover is its Version's, else its default edition's in Language
// (internal/edition). With Preset media.AvatarPreset it names an entity's
// avatar: Image, else the item's first, else the kind's default.
type CoverOf struct {
	Kind      string
	ID        string
	Image     string
	ContentID string
	Version   string
	Language  string
	Preset    string
}

// Covers resolves covers to their public renditions, per cover in order,
// in two queries for the page. A content's cover is the image its version's
// cover selects, else the version's first cover image, else its kind's
// default.
func (s *Service) Covers(ctx context.Context, covers []CoverOf) ([][]media.PublicRendition, error) {
	out := make([][]media.PublicRendition, len(covers))
	type ask struct{ content, version, language string }
	var asks []ask
	at := make([]int, len(covers))
	for i, c := range covers {
		at[i] = -1
		if c.Image != "" || c.Preset == media.AvatarPreset {
			continue
		}
		id := c.ContentID
		if _, isContent := s.o.Kinds.Kinds[c.Kind]; id == "" && isContent {
			id = c.ID
		}
		if id != "" {
			at[i] = len(asks)
			asks = append(asks, ask{id, c.Version, c.Language})
		}
	}
	type chosen struct{ kind, version, image string }
	found := map[int]chosen{}
	if len(asks) > 0 {
		contents, versions, languages := make([]string, len(asks)), make([]string, len(asks)), make([]string, len(asks))
		for i, a := range asks {
			contents[i], versions[i], languages[i] = a.content, a.version, a.language
		}
		rows, err := s.o.Pool.Query(ctx, `SELECT t.n, f.kind, f.version_id, coalesce(v.cover->>'image', '')
FROM unnest($2::text[], $3::text[], $4::text[]) WITH ORDINALITY AS t(content, version, language, n)
CROSS JOIN LATERAL (SELECT f.kind, f.version_id FROM `+s.t.facets+` f WHERE f.tenant_id = $1 AND f.content_id = t.content
 AND (t.version = '' OR f.version_id = t.version) ORDER BY `+edition.Order("f", "t.language")+` LIMIT 1) f
JOIN `+s.t.versions+` v ON v.tenant_id = $1 AND v.id = f.version_id`, s.o.Tenant, contents, versions, languages)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var n int
			var e chosen
			if err := rows.Scan(&n, &e.kind, &e.version, &e.image); err != nil {
				rows.Close()
				return nil, err
			}
			found[n-1] = e
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	var qs []media.ImageQuery
	var put []int
	for i, c := range covers {
		q := media.ImageQuery{Preset: media.CoverPreset, Name: c.Image}
		kind := c.Kind
		var err error
		if c.Preset == media.AvatarPreset {
			q.Preset = media.AvatarPreset
			q.Ref, err = s.reg.Ref(c.Kind, c.ID)
		} else if c.Image != "" {
			q.Ref, err = s.reg.Ref(c.Kind, c.ID)
		} else if e, ok := found[at[i]]; ok && at[i] >= 0 {
			kind = e.kind
			q.Ref, err = s.reg.VersionRef(e.kind, asks[at[i]].content, e.version)
			q.Name = e.image
		} else {
			continue
		}
		// A kind without the preset (no covers, no avatars) shows none.
		if k, kerr := s.reg.Kind(kind); err != nil || kerr != nil || !slices.ContainsFunc(k.Public, func(p media.Public) bool { return p.Name == q.Preset }) {
			continue
		}
		qs, put = append(qs, q), append(put, i)
	}
	images, err := s.m.Manifests.Images(ctx, qs...)
	if err != nil {
		return nil, err
	}
	for j, i := range put {
		image, ok := media.PublicImage{}, len(images[j]) > 0
		if ok {
			image = images[j][0]
		} else {
			image, ok = s.reg.DefaultImage(qs[j].Ref, qs[j].Preset)
		}
		if ok {
			out[i] = image.Renditions
		}
	}
	return out, nil
}

// Ladder is the folder of the kind's first HLS ladder (a read's hls entry),
// "" when it streams none or one per upload.
func (s *Service) Ladder(kind string) string {
	k, err := s.reg.Kind(kind)
	if err != nil {
		return ""
	}
	for _, p := range k.Private {
		if p.HLS != nil {
			if strings.Contains(p.To, "{") {
				return ""
			}
			return p.To
		}
	}
	return ""
}

// EntityCovers is Covers for the entities API's cards and pages
// (entities.Options.Covers).
func (s *Service) EntityCovers() entities.CoverResolver { return entityCovers{s} }

type entityCovers struct{ s *Service }

func (g entityCovers) Covers(ctx context.Context, cs []entities.CoverOf) ([][]entities.Rendition, error) {
	asked := make([]CoverOf, len(cs))
	for i, c := range cs {
		asked[i] = CoverOf{Kind: c.Kind, ID: string(c.ID), Image: c.Cover.Image, ContentID: c.Cover.ContentID, Language: c.Language, Preset: c.Preset}
	}
	got, err := g.s.Covers(ctx, asked)
	if err != nil {
		return nil, err
	}
	out := make([][]entities.Rendition, len(cs))
	for i, rs := range got {
		for _, r := range rs {
			out[i] = append(out[i], entities.Rendition{URL: r.URL, W: r.W, H: r.H})
		}
	}
	return out, nil
}

// EntityFolders hides, shows and deletes entities' media folders for the entities
// API (entities.Options.Folders).
func (s *Service) EntityFolders() entities.Folders { return entityFolders{s} }

type entityFolders struct{ s *Service }

// RetireTx commits each entity media item's retirement intent in tx.
func (g entityFolders) RetireTx(ctx context.Context, tx pgx.Tx, refs []entities.Ref) error {
	del := make([]media.Deletion, 0, len(refs))
	for _, r := range refs {
		ref, err := g.s.reg.Ref(r.Kind, string(r.ID))
		if err != nil {
			return err
		}
		del = append(del, media.Deletion{Ref: ref, Owner: EntityOwner(r.Kind, string(r.ID))})
	}
	if len(del) == 0 || g.s.m.Jobs == nil {
		return nil
	}
	return g.s.m.Jobs.RetireItemsTx(ctx, tx, del...)
}

func (g entityFolders) RetirementStartedTx(ctx context.Context, tx pgx.Tx, r entities.Ref) (bool, error) {
	ref, err := g.s.reg.Ref(r.Kind, string(r.ID))
	if err != nil {
		return false, err
	}
	return g.s.m.Jobs.RetirementStartedTx(ctx, tx, ref)
}
func (g entityFolders) RetirementComplete(ctx context.Context, r entities.Ref) (bool, error) {
	ref, err := g.s.reg.Ref(r.Kind, string(r.ID))
	if err != nil {
		return false, err
	}
	return g.s.m.Jobs.RetirementComplete(ctx, ref)
}
func (g entityFolders) PurgeRetired(ctx context.Context, limit int) (int, error) {
	return g.s.m.Jobs.PurgeRetired(ctx, limit)
}
