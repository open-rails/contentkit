import type { BanInput, CommentBan } from "../generated/wire.js";
import type { Http } from "../http.js";
import { call, type PageQuery } from "../route.js";
import type { BanScope } from "./types.js";

/** Comment bans: "owner" on the caller's own content, "global" across the site (CommentBan). */
export class BansClient {
  constructor(private readonly http: Http) {}

  /** The scope's bans, newest first, expired ones included. */
  list(scope: BanScope, q: PageQuery = {}, signal?: AbortSignal): Promise<CommentBan[]> {
    return scope === "global"
      ? call(this.http, "GET", "/global-comment-bans", { query: q, signal })
      : call(this.http, "GET", "/comment-bans", { query: q, signal });
  }
  /** Bans user from commenting in the scope, replacing any ban there; no until lasts until lifted. */
  async ban(scope: BanScope, user: string, input: BanInput = {}): Promise<CommentBan> {
    const o = { params: { user }, body: input };
    const ban: CommentBan = await (scope === "global"
      ? call(this.http, "PUT", "/global-comment-bans/{user}", o)
      : call(this.http, "PUT", "/comment-bans/{user}", o));
    this.http.emit({ type: "ban.saved", scope, ban });
    return ban;
  }
  /** Lifts user's ban in the scope; idempotent. */
  async lift(scope: BanScope, user: string): Promise<void> {
    const o = { params: { user } };
    await (scope === "global" ? call(this.http, "DELETE", "/global-comment-bans/{user}", o) : call(this.http, "DELETE", "/comment-bans/{user}", o));
    this.http.emit({ type: "ban.lifted", scope, user });
  }
}
