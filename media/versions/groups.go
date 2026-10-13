package versions

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/media"
)

// A group kind's items, {kind, group}, hold the group's named covers (and a
// user-owned group's avatars): staff holding root:groups:write edit them,
// and for a user-owned group its owner and its AuthKit scope's holders of
// {kind}:media:write or {kind}:settings:manage.

// groupRight is what one actor may do with one group's item.
type groupRight struct {
	active, editor, staff bool
	owner                 string
}

// groupRights loads, per group id of kind, the group's state and actor's
// right to edit its media.
func (s *Service) groupRights(ctx context.Context, actor access.Actor, kind string, ids []string) (map[string]groupRight, error) {
	out := map[string]groupRight{}
	rows, err := s.o.Pool.Query(ctx, `SELECT id, state = 'active', coalesce(owner_user_id, ''), coalesce(auth_scope, '')
FROM `+s.t.groups+` WHERE tenant_id = $1 AND kind = $2 AND id = ANY($3)`, s.o.Tenant, kind, ids)
	if err != nil {
		return nil, err
	}
	type row struct {
		id, owner, scope string
		active           bool
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.active, &r.owner, &r.scope); err != nil {
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
		if staff, err = gate.Can(ctx, s.o.Auth, actor, string(access.GroupsWrite)); err != nil {
			return nil, err
		}
	}
	userOwned := s.o.Kinds.GroupKinds[kind].Owner == "user"
	for _, r := range found {
		right := groupRight{active: r.active, staff: staff, owner: r.owner}
		switch {
		case staff:
			right.editor = true
		case userOwned:
			if right.editor, err = gate.GroupEditor(ctx, s.o.Auth, actor, r.owner, r.scope, access.GroupMediaWrite(kind), access.GroupSettingsManage(kind)); err != nil {
				return nil, err
			}
		}
		out[r.id] = right
	}
	return out, nil
}

func (s *Service) isGroupKind(kind string) bool {
	_, ok := s.o.Kinds.GroupKinds[kind]
	return ok
}

// resolveGroups shows an active group's item to everyone, its public files
// only, and its private files to its editors.
func (s *Service) resolveGroups(ctx context.Context, refs []contentref.ContentRef, actor access.Actor, out map[contentref.ContentKey]access.Resolution) error {
	byKind := map[string][]string{}
	for _, ref := range refs {
		byKind[ref.ContentKind] = append(byKind[ref.ContentKind], ref.ContentID)
	}
	rights := map[string]map[string]groupRight{}
	for kind, ids := range byKind {
		var err error
		if rights[kind], err = s.groupRights(ctx, actor, kind, ids); err != nil {
			return err
		}
	}
	for _, ref := range refs {
		if r, ok := rights[ref.ContentKind][ref.ContentID]; ok && r.active {
			out[ref.Key()] = access.Resolution{Visible: true, Editor: r.editor}
		}
	}
	return nil
}

func (s *Service) canEditGroup(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	rights, err := s.groupRights(ctx, actor, t.Ref.ContentKind, []string{t.Ref.ContentID})
	if err != nil {
		return media.UploadGrant{}, err
	}
	r, ok := rights[t.Ref.ContentID]
	if !ok || !r.active || !r.editor {
		return media.UploadGrant{}, nil
	}
	return media.UploadGrant{Allowed: true, Exempt: r.staff, Owner: GroupOwner(t.Ref.ContentKind, t.Ref.ContentID)}, nil
}

// GroupOwner is the quota owner of a group's uploads: "{kind}:{id}".
func GroupOwner(kind, id string) string { return kind + ":" + id }

// ParseGroupOwner is GroupOwner's kind and id; ok is false for any other
// owner.
func ParseGroupOwner(owner string) (kind, id string, ok bool) {
	kind, id, ok = strings.Cut(owner, ":")
	return kind, id, ok && kind != "" && id != ""
}

// CoverOf names a content's or a group's cover: Kind and ID are its owner;
// Image an image uploaded to the owner's item ("i-{uuid}"), else ContentID a
// content whose cover it shows; with neither, a content owner's own. With
// Preset media.AvatarPreset it names a group's avatar: Image, else the
// item's first, else the kind's default.
type CoverOf struct {
	Kind      string
	ID        string
	Image     string
	ContentID string
	Preset    string
}

// Covers resolves covers to their public renditions, per cover in order,
// in two queries for the page. A content's cover is the image its cover
// selects, else its item's first cover image, else its kind's default.
func (s *Service) Covers(ctx context.Context, covers []CoverOf) ([][]media.PublicRendition, error) {
	out := make([][]media.PublicRendition, len(covers))
	targets := make([]string, len(covers))
	var contents []string
	for i, c := range covers {
		if c.Image != "" || c.Preset == media.AvatarPreset {
			continue
		}
		targets[i] = c.ContentID
		if _, isContent := s.o.Kinds.Kinds[c.Kind]; targets[i] == "" && isContent {
			targets[i] = c.ID
		}
		if targets[i] != "" {
			contents = append(contents, targets[i])
		}
	}
	type content struct{ kind, image string }
	found := map[string]content{}
	if len(contents) > 0 {
		rows, err := s.o.Pool.Query(ctx, `SELECT id, kind, coalesce(cover, '{}'::jsonb) FROM `+s.t.items+` WHERE tenant_id = $1 AND id = ANY($2)`,
			s.o.Tenant, contents)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, kind string
			var raw []byte
			if err := rows.Scan(&id, &kind, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			var cover struct {
				Image string `json:"image"`
			}
			_ = json.Unmarshal(raw, &cover)
			found[id] = content{kind, cover.Image}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	var qs []media.ImageQuery
	var at []int
	for i, c := range covers {
		q := media.ImageQuery{Preset: media.CoverPreset, Name: c.Image}
		kind := c.Kind
		var err error
		if c.Preset == media.AvatarPreset {
			q.Preset = media.AvatarPreset
			q.Ref, err = s.reg.Ref(c.Kind, c.ID)
		} else if c.Image != "" {
			q.Ref, err = s.reg.Ref(c.Kind, c.ID)
		} else if f, ok := found[targets[i]]; ok {
			kind = f.kind
			q.Ref, err = s.reg.Ref(f.kind, targets[i])
			q.Name = f.image
		} else {
			continue
		}
		// A kind without the preset (no covers, no avatars) shows none.
		if k, kerr := s.reg.Kind(kind); err != nil || kerr != nil || !slices.ContainsFunc(k.Public, func(p media.Public) bool { return p.Name == q.Preset }) {
			continue
		}
		qs, at = append(qs, q), append(at, i)
	}
	images, err := s.m.Manifests.Images(ctx, qs...)
	if err != nil {
		return nil, err
	}
	for j, i := range at {
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

// GroupCovers is Covers for the groups API's cards and pages
// (groups.Options.Covers).
func (s *Service) GroupCovers() groups.CoverResolver { return groupCovers{s} }

type groupCovers struct{ s *Service }

func (g groupCovers) Covers(ctx context.Context, cs []groups.CoverOf) ([][]groups.Rendition, error) {
	asked := make([]CoverOf, len(cs))
	for i, c := range cs {
		asked[i] = CoverOf{Kind: c.Kind, ID: string(c.ID), Image: c.Cover.Image, ContentID: c.Cover.ContentID, Preset: c.Preset}
	}
	got, err := g.s.Covers(ctx, asked)
	if err != nil {
		return nil, err
	}
	out := make([][]groups.Rendition, len(cs))
	for i, rs := range got {
		for _, r := range rs {
			out[i] = append(out[i], groups.Rendition{URL: r.URL, W: r.W, H: r.H})
		}
	}
	return out, nil
}

// GroupFolders deletes groups' media folders for the groups API
// (groups.Options.Folders).
func (s *Service) GroupFolders() groups.Folders { return groupFolders{s} }

type groupFolders struct{ s *Service }

// DeleteTx queues the deletion of each group's media item in tx.
func (g groupFolders) DeleteTx(ctx context.Context, tx pgx.Tx, refs []groups.Ref) error {
	del := make([]media.Deletion, 0, len(refs))
	for _, r := range refs {
		ref, err := g.s.reg.Ref(r.Kind, string(r.ID))
		if err != nil {
			return err
		}
		del = append(del, media.Deletion{Ref: ref, Owner: GroupOwner(r.Kind, string(r.ID))})
	}
	if len(del) == 0 || g.s.m.Jobs == nil {
		return nil
	}
	return g.s.m.Jobs.DeleteItemsTx(ctx, tx, del...)
}
