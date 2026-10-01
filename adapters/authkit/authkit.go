// Package authkit connects ContentKit to AuthKit accounts: account avatars on
// ContentKit's slots (Avatars) and content authors (Authors). It is a module
// of its own, so ContentKit's core never imports AuthKit.
//
// An account's avatar is its user folder's avatar slot (media.UserKind,
// media.AvatarSlotName), served from fixed public names. The account's AuthKit
// public metadata names its link (media.Reader.SlotLink, ?v= its version)
// under Key ("avatar"): hosts and auth-ui read public_metadata.avatar, and
// show their default when it is unset. Hosts sharing one account store share
// the key, so the site where the user last set an avatar is the one shown
// everywhere.
package authkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	ak "github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// DefaultKey is the public-metadata key naming an account's avatar link.
const DefaultKey = "avatar"

// Directory is what this package uses of *authkit.Client.
type Directory interface {
	Can(ctx context.Context, actor iam.Actor, ref iam.GroupRef, perm iam.Perm) (bool, error)
	PatchPublicMetadata(ctx context.Context, actor iam.Actor, userID string, patch map[string]any, opts ...ak.Option) error
	PublicUsers(ctx context.Context, ids []string) (map[string]iam.PublicUser, error)
}

// SlotLinker builds a public slot's URL: *media.Reader.
type SlotLinker interface {
	SlotLink(ref contentref.ContentRef, slot string, width int, version string) string
}

var (
	_ Directory              = (*ak.Client)(nil)
	_ SlotLinker             = (*media.Reader)(nil)
	_ media.UploadAuthorizer = (*Avatars)(nil)
	_ content.UserEnricher   = (*Authors)(nil)
)

// Avatars are account avatars: CanUpload decides who may change one and
// SlotChanged names it in the account's public metadata.
type Avatars struct {
	Directory Directory
	Links     SlotLinker
	// Staff may change any account's avatar, checked live in the root group;
	// zero: nobody but the account's user.
	Staff iam.Perm
	// Key is the public-metadata key; default DefaultKey.
	Key string
	// Slot is the avatar slot; default media.AvatarSlotName.
	Slot string
	// Width is the rung public_metadata names (clients pick others with
	// media.Slot.LinkAt); default 256.
	Width int
}

func (a *Avatars) key() string { return or(a.Key, DefaultKey) }

func (a *Avatars) slot() string { return or(a.Slot, media.AvatarSlotName) }

// CanUpload authorizes the avatar slot of user folders: a signed-in user
// their own (Owner them and not Exempt, so the host's upload limiter
// applies), staff holding Staff anyone's (Exempt). It refuses every other
// target: a host routes its other kinds before it. It reads the verified
// claims AuthKit's middleware put in ctx; only the staff check reads the
// database.
func (a *Avatars) CanUpload(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	if t.Ref.ContentKind != media.UserKind || t.Ref.Version() != "" || t.Slot != a.slot() || actor.Anonymous || actor.ID == "" {
		return media.UploadGrant{}, nil
	}
	cl, ok := verify.ClaimsFromContext(ctx)
	if !ok {
		return media.UploadGrant{}, nil
	}
	if cl.IsUser() && cl.UserID == actor.ID && actor.ID == t.Ref.ContentID {
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

// SlotChanged is the avatar's media.Hooks.SlotChanged: when a user's avatar
// is set or replaced, their public metadata's Key becomes its link at Width
// with ?v= the new version, one merge patch outside the slot index
// transaction (AuthKit may live in another database). A removal clears Key
// when it names this site's avatar (another site's stays), so clients show
// their default. An erased account is done; other slots pass.
func (a *Avatars) SlotChanged(ctx context.Context, _ pgx.Tx, c media.SlotChange) error {
	if c.Ref.ContentKind != media.UserKind || c.Slot != a.slot() {
		return nil
	}
	width := a.Width
	if width <= 0 {
		width = 256
	}
	link := a.Links.SlotLink(c.Ref, c.Slot, width, c.Version)
	if link == "" {
		return fmt.Errorf("contentkit/authkit: kind %q has no slot %q", c.Ref.ContentKind, c.Slot)
	}
	var value any = link
	if c.Version == "" {
		users, err := a.Directory.PublicUsers(ctx, []string{c.Ref.ContentID})
		if err != nil {
			return err
		}
		current, _, _ := strings.Cut(AvatarLink(users[c.Ref.ContentID], a.key()), "?")
		if current != link {
			return nil // unset, or another site's avatar
		}
		value = nil
	}
	err := a.Directory.PatchPublicMetadata(ctx, iam.SystemActor(), c.Ref.ContentID, map[string]any{a.key(): value})
	if errors.Is(err, iam.ErrUserNotFound) {
		return nil
	}
	return err
}

// Authors is content's UserEnricher over AuthKit: each id's display name
// (tombstones and unknown ids get AuthKit's fallback) and the avatar its
// public metadata names. A directory failure is logged and degrades to
// fallback names without avatars; it never fails a listing.
type Authors struct {
	Directory Directory
	// Key is the public-metadata key; default DefaultKey.
	Key string
	// Width is the avatar's display width in CSS pixels: Avatar is the link
	// at the rung for it (media.Slot.LinkAt); default 64.
	Width int
	// Slot gives AvatarSrcSet's widths; default media.AvatarSlot.
	Slot   *media.Slot
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
	width := a.Width
	if width <= 0 {
		width = 64
	}
	slot := media.AvatarSlot
	if a.Slot != nil {
		slot = *a.Slot
	}
	for _, id := range ids {
		u := content.PublicUser{ID: id, Username: iam.PublicDisplayName(users, id)}
		if link := AvatarLink(users[id], or(a.Key, DefaultKey)); link != "" {
			if u.Avatar = slot.LinkAt(link, width); u.Avatar != "" {
				u.AvatarSrcSet = slot.LinkSrcSet(link)
			}
		}
		out[id] = u
	}
	return out, nil
}

// AvatarLink is the avatar link an account's public metadata names under key:
// an absolute http(s) URL whose only query is v, else "".
func AvatarLink(u iam.PublicUser, key string) string {
	s, _ := u.PublicMetadata[key].(string)
	p, err := url.Parse(s)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" || p.Fragment != "" || p.User != nil {
		return ""
	}
	if q := p.Query(); len(q) > 1 || len(q) == 1 && len(q["v"]) != 1 {
		return ""
	}
	return s
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
