// Package authkit connects ContentKit to AuthKit accounts: who may upload to
// an account's media (Avatars) and content authors (Authors). It is a module
// of its own, so ContentKit's core never imports AuthKit.
//
// An account's media is an item of a kind whose ids are AuthKit user ids,
// such as the shared accounts/user kind. Its avatar is a public preset at a
// fixed name, so its URL is a pure function of the user id: AuthKit stores
// no avatar metadata.
package authkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	ak "github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// Directory is what this package uses of *authkit.Client.
type Directory interface {
	CheckSession(ctx context.Context, cl verify.Claims) error
	Can(ctx context.Context, actor iam.Actor, ref iam.GroupRef, perm iam.Perm) (bool, error)
	PublicUsers(ctx context.Context, ids []string) (map[string]iam.PublicUser, error)
}

var (
	_ Directory              = (*ak.Client)(nil)
	_ media.UploadAuthorizer = (*Avatars)(nil)
	_ content.UserEnricher   = (*Authors)(nil)
)

// DefaultKind is the account kind's name.
const DefaultKind = "user"

// Avatars authorizes uploads to account items: a signed-in user their own
// (Owner them and not Exempt, so the host's upload limiter applies), staff
// holding Staff anyone's (Exempt). It refuses every other kind: a host
// routes its other kinds before it. It reads the verified claims AuthKit's
// middleware put in ctx and checks account and session state live.
type Avatars struct {
	Directory Directory
	// Staff may change any account's media, checked live in the root
	// group; zero: nobody but the account's user.
	Staff iam.Perm
	Kind  string // default DefaultKind
}

func (a *Avatars) CanUpload(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	if t.Ref.ContentKind != or(a.Kind, DefaultKind) || actor.Anonymous || actor.ID == "" {
		return media.UploadGrant{}, nil
	}
	cl, ok := verify.ClaimsFromContext(ctx)
	if !ok {
		return media.UploadGrant{}, nil
	}
	if cl.IsUser() && cl.UserID == actor.ID && actor.ID == t.Ref.ContentID {
		if err := a.Directory.CheckSession(ctx, cl); errors.Is(err, iam.ErrSessionRevoked) {
			return media.UploadGrant{}, nil
		} else if err != nil {
			return media.UploadGrant{}, fmt.Errorf("contentkit/authkit: session check: %w", err)
		}
		return media.UploadGrant{Allowed: true, Owner: actor.ID}, nil
	}
	if a.Staff.IsZero() {
		return media.UploadGrant{}, nil
	}
	who, ok := verify.ActorFromClaims(cl)
	if !ok {
		return media.UploadGrant{}, nil
	}
	allowed, err := a.Directory.Can(ctx, who, iam.RootGroup(), a.Staff)
	if errors.Is(err, iam.ErrSessionRevoked) {
		return media.UploadGrant{}, nil
	} else if err != nil {
		return media.UploadGrant{}, fmt.Errorf("contentkit/authkit: staff check: %w", err)
	}
	return media.UploadGrant{Allowed: allowed, Exempt: allowed}, nil
}

// Authors is content's UserEnricher over AuthKit: each id's display name
// (tombstones and unknown ids get AuthKit's fallback) and its avatar, the
// account kind's public preset at the site's media host (the access agent
// serves the default until one is set). A directory failure is logged and
// degrades to fallback names; it never fails a listing.
type Authors struct {
	Directory Directory
	Media     *media.Registry // the site's registry, importing the account kind
	Kind      string          // default DefaultKind
	Preset    string          // the avatar's public preset; default "avatar"
	// Width is the avatar's display width in CSS pixels: Avatar is the
	// narrowest width at least this wide; default 64.
	Width  int
	Logger *slog.Logger
}

func (a *Authors) UsersByIDs(ctx context.Context, ids []string) (map[string]content.PublicUser, error) {
	out := make(map[string]content.PublicUser, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	users, err := a.Directory.PublicUsers(ctx, ids)
	if err != nil {
		log := a.Logger
		if log == nil {
			log = slog.Default()
		}
		log.WarnContext(ctx, "contentkit/authkit: public users failed; showing fallback names", "error", err)
		users = nil
	}
	for _, id := range ids {
		u := content.PublicUser{ID: id, Username: iam.PublicDisplayName(users, id)}
		if ref, err := a.Media.Ref(or(a.Kind, DefaultKind), id); err == nil {
			u.Avatar, u.AvatarSrcSet = a.avatar(ref)
		}
		out[id] = u
	}
	return out, nil
}

// avatar is the account's avatar URL at Width and its srcset.
func (a *Authors) avatar(ref contentref.ContentRef) (string, string) {
	k, err := a.Media.Kind(ref.ContentKind)
	if err != nil {
		return "", ""
	}
	preset := or(a.Preset, "avatar")
	for i := range k.Public {
		p := &k.Public[i]
		if p.Name != preset {
			continue
		}
		names := k.PublicNames(p, "")
		width := a.Width
		if width <= 0 {
			width = 64
		}
		pick := names[len(names)-1]
		for j, w := range p.Widths {
			if w >= width {
				pick = names[j]
				break
			}
		}
		return a.Media.PublicURL(ref, pick), a.Media.SrcSet(ref, preset)
	}
	return "", ""
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
