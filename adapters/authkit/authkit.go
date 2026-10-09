// Package authkit connects ContentKit to AuthKit accounts: who may upload to
// an account's media (Avatars) and content authors (Authors). It is a module
// of its own, so ContentKit's core never imports AuthKit.
//
// An account's media is an item of a kind whose ids are AuthKit user ids,
// such as the shared accounts/user kind. Its current avatar's public generation
// is owned by the media manifest; AuthKit stores no avatar metadata.
package authkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	ak "github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/helpers/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// Directory is what this package uses of *authkit.Client.
type Directory interface {
	CheckSession(ctx context.Context, cl verify.Claims) error
	Can(ctx context.Context, who auth.Identity, ref iam.GroupRef, perm iam.Perm) (bool, error)
	PublicUsers(ctx context.Context, ids []string) (map[string]iam.PublicUser, error)
}

var (
	_ Directory              = (*ak.Client)(nil)
	_ media.UploadAuthorizer = (*Avatars)(nil)
	_ content.UserEnricher   = (*Authors)(nil)
	_ Images                 = (*media.Manifests)(nil)
)

// DefaultKind is the account kind's name.
const DefaultKind = "user"

// Avatars authorizes uploads to account items: a signed-in user their own
// (Owner them and not Exempt, so the host's upload limiter applies), staff
// holding Staff anyone's (Exempt). It refuses every other kind: a host
// routes its other kinds before it. It reads the identity an AuthKit gate
// verified for the request (verify.IdentityFromContext; claims stored any
// other way grant nothing) and checks account and session state live.
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
	who, _ := verify.IdentityFromContext(ctx)
	state, verified := iam.StateOf(who)
	cl, _ := verify.ClaimsFromContext(ctx)
	if !verified {
		return media.UploadGrant{}, nil
	}
	if state.IsUser() && who.Subject == actor.ID && actor.ID == t.Ref.ContentID {
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
// account kind's currently published preset. A directory or media failure is
// logged and degrades to fallback names or an absent avatar; it never fails a
// listing.
type Authors struct {
	Directory Directory
	Media     Images
	Kind      string // default DefaultKind
	Preset    string // the avatar's public preset; default "avatar"
	// Width is the avatar's display width in CSS pixels: Avatar is the
	// narrowest width at least this wide; default 64.
	Width  int
	Logger *slog.Logger
}

// Images is the public lookup Authors uses of *media.Manifests.
type Images interface {
	Registry() *media.Registry
	PublicImages(context.Context, contentref.ContentRef) ([]media.PublicImage, error)
}

func (a *Authors) UsersByIDs(ctx context.Context, ids []string) (map[string]content.PublicUser, error) {
	out := make(map[string]content.PublicUser, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	log := a.Logger
	if log == nil {
		log = slog.Default()
	}
	users, err := a.Directory.PublicUsers(ctx, ids)
	if err != nil {
		log.WarnContext(ctx, "contentkit/authkit: public users failed; showing fallback names", "error", err)
		users = nil
	}
	for _, id := range ids {
		u := content.PublicUser{ID: id, Username: iam.PublicDisplayName(users, id)}
		if ref, err := a.Media.Registry().Ref(or(a.Kind, DefaultKind), id); err == nil {
			images, err := a.Media.PublicImages(ctx, ref)
			if err != nil {
				log.WarnContext(ctx, "contentkit/authkit: public images failed; showing no avatar", "user", id, "error", err)
			} else {
				for _, image := range images {
					if image.Preset == or(a.Preset, "avatar") {
						width := a.Width
						if width <= 0 {
							width = 64
						}
						u.Avatar, u.AvatarSrcSet = image.URL(width), image.SrcSet()
						break
					}
				}
			}
		}
		out[id] = u
	}
	return out, nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
