package content

import "context"

// avatarWidth is the CSS width authors are shown at: Avatar is the narrowest
// rendition at least this wide; AvatarSrcSet has every one.
const avatarWidth = 64

// authors is how each of ids is shown: its name and username from
// Options.UserInfo (one Get) and its avatar (Options.Avatars, built from the
// id). An id the directory does not hold, such as a deleted account, is
// absent. A failure is logged and leaves bare ids; it never fails a response.
func (rt *Runtime) authors(ctx context.Context, ids []string) map[string]PublicUser {
	out := map[string]PublicUser{}
	ids = dedup(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := rt.store.pool.Query(ctx, `SELECT actor_id FROM `+rt.accountLifecycleTable()+` WHERE tenant_id=$1 AND actor_id=ANY($2)`, rt.tenant, ids)
	if err != nil {
		rt.log.WarnContext(ctx, "content: author lifecycle lookup failed", "err", err.Error())
		return out
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return map[string]PublicUser{}
		}
		out[id] = PublicUser{ID: id, Deleted: true}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return map[string]PublicUser{}
	}
	if rt.userInfo == nil {
		return out
	}
	users, err := rt.userInfo.Get(ctx, ids)
	if err != nil {
		rt.log.WarnContext(ctx, "content: authors lookup failed; showing ids", "err", err.Error())
		return out
	}
	var held []string
	for _, id := range ids {
		if out[id].Deleted {
			continue
		}
		if u, ok := users[id]; ok {
			out[id] = PublicUser{ID: id, Name: u.Name, Username: u.Username}
			held = append(held, id)
		}
	}
	if len(held) == 0 || rt.avatars == nil {
		return out
	}
	images := rt.avatars.Avatars(held...)
	for i, id := range held {
		if i < len(images) && len(images[i].Renditions) > 0 {
			u := out[id]
			u.Avatar, u.AvatarSrcSet = images[i].URL(avatarWidth), images[i].SrcSet()
			out[id] = u
		}
	}
	return out
}
