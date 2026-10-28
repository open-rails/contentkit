package contentstore

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	content "github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/kinds"
)

// shows reports whether the actor sees property p (kinds.Show*) of a visible
// target: all of content they open (for a version's, the version) or edit,
// of the rest what its kind's locked view shows; all of an entity.
func (rt *Runtime) shows(ref contentref.ContentRef, res resolve.Resolution, p string) bool {
	k, cnt := rt.cfg.Kinds[ref.ContentKind]
	return res.Visible && (!cnt || res.Accessible || res.Editor || k.LockedView().Shows(p))
}

// commentsLocked refuses comment writes where the actor doesn't see the
// target's comments.
func (rt *Runtime) commentsLocked(ref contentref.ContentRef, res resolve.Resolution) error {
	if !rt.shows(ref, res, kinds.ShowComments) {
		return ErrCommentsLocked
	}
	return nil
}

// commentsOpen refuses comment writes where comments are closed or hidden.
func commentsOpen(res resolve.Resolution) error {
	if res.Comments == content.CommentsClosed || res.Comments == content.CommentsHidden {
		return ErrCommentsClosed
	}
	return nil
}

// TargetTypes are the route families interactions are served under.
var TargetTypes = []content.TargetType{content.TargetContent, content.TargetEntity, content.TargetChannel}

// versionParam names one version of a content target.
var versionParam = httpapi.Text("version", "one version of the content item; the item itself by default")

// targetQuery is a target route's query parameters besides its own.
func targetQuery(typ content.TargetType, params ...httpapi.Param) []httpapi.Param {
	if typ == content.TargetContent {
		return append([]httpapi.Param{versionParam}, params...)
	}
	return params
}

// target reads a route's target: the content item or entity its id names,
// under its kind, and for a content item the version ?version names. An
// unknown id is ErrNotFound.
func (rt *Runtime) target(req *http.Request, typ content.TargetType) (contentref.ContentRef, error) {
	id, version := req.PathValue("id"), req.URL.Query().Get("version")
	if contentref.ValidateID(id) != nil {
		return contentref.ContentRef{}, ErrNotFound
	}
	if typ != content.TargetContent && version != "" {
		return contentref.ContentRef{}, badRequest("only content has versions")
	}
	table := rt.store.t.items
	if typ != content.TargetContent {
		table = rt.store.t.entities
	}
	var kind string
	err := rt.store.pool.QueryRow(req.Context(), `SELECT kind FROM `+table+` WHERE tenant_id = $1 AND id = $2`, rt.tenant, id).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return contentref.ContentRef{}, ErrNotFound
	}
	if err != nil {
		return contentref.ContentRef{}, err
	}
	if t, ok := rt.targetType(kind); !ok || t != typ {
		return contentref.ContentRef{}, ErrNotFound
	}
	return contentref.NewVersion(rt.tenant, kind, id, version), nil
}

// targetType is the type of targets of kind.
func (rt *Runtime) targetType(kind string) (content.TargetType, bool) {
	if _, ok := rt.cfg.Kinds[kind]; ok {
		return content.TargetContent, true
	}
	if kind == "channel" {
		return content.TargetChannel, rt.cfg.Channels != nil
	}
	if _, ok := rt.cfg.EntityKinds[kind]; ok {
		return content.TargetEntity, true
	}
	return "", false
}

// targetHandler adapts a handler of a resolved target to a route.
func (rt *Runtime) targetHandler(typ content.TargetType, h func(http.ResponseWriter, *http.Request, contentref.ContentRef)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ref, err := rt.target(req, typ)
		if err != nil {
			writeErr(w, err)
			return
		}
		h(w, req, ref)
	}
}
