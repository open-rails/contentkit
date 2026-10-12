import type {
  ContentGroups,
  ContentMemberships,
  Edge,
  EdgeInput,
  Group,
  GroupDetail,
  GroupInput,
  GroupPage,
  GroupUpdate,
  MemberPage,
  MemberRef,
  Membership,
  MergeReport,
  MyGroupPage,
  Name,
  RefBody,
  Written,
} from "./generated/wire.js";
import type { Http } from "./http.js";
import type { MediaClient, NamedOptions } from "./media/client.js";
import { call, type RouteQuery } from "./route.js";

/** GET /groups filters, as the route names them. */
export type GroupQuery = RouteQuery<"GET", "/groups">;
/** GET /me/groups filters. */
export type MyGroupQuery = RouteQuery<"GET", "/me/groups">;
/** A group's image slots: every kind's cover, a user-owned kind's avatar. */
export type GroupImageSlot = "cover" | "avatar";
/** GET /groups/{id}/members paging. */
export type MemberQuery = RouteQuery<"GET", "/groups/{id}/members">;

/** Groups: public listings and pages; writes for staff (root:groups:write). */
export class GroupsClient {
  constructor(
    private readonly http: Http,
    private readonly media: MediaClient,
  ) {}

  list(q: GroupQuery = {}, signal?: AbortSignal): Promise<GroupPage> {
    return call(this.http, "GET", "/groups", { query: q, signal });
  }
  /** A merged group answers with its survivor. */
  get(id: string, language?: string, signal?: AbortSignal): Promise<GroupDetail> {
    return call(this.http, "GET", "/groups/{id}", { params: { id }, query: { language }, signal });
  }
  create(groups: GroupInput[]): Promise<Group[]> {
    return this.write(call(this.http, "POST", "/groups", { body: groups }));
  }
  update(id: string, patch: GroupUpdate): Promise<Group> {
    return this.write(call(this.http, "PATCH", "/groups/{id}", { params: { id }, body: patch }));
  }
  /** Marks the group deleted. */
  remove(id: string): Promise<Group> {
    return this.write(call(this.http, "DELETE", "/groups/{id}", { params: { id } }));
  }
  /** Replaces, adds or removes names and aliases; resolves with the group's names. */
  names(id: string, op: "set" | "add" | "remove", names: Name[]): Promise<Name[]> {
    const method = op === "set" ? "PUT" : op === "add" ? "POST" : "DELETE";
    return this.write(call(this.http, method, "/groups/{id}/names", { params: { id }, body: names }));
  }
  /** Adds or removes edges from the group; resolves with every edge touching it. */
  edges(id: string, op: "add" | "remove", edges: EdgeInput[]): Promise<Edge[]> {
    return this.write(call(this.http, op === "add" ? "POST" : "DELETE", "/groups/{id}/edges", { params: { id }, body: edges }));
  }
  /** The group's media folder: the media kind named after its group kind. */
  folder(group: { kind: string; id: string }): RefBody {
    return { kind: group.kind, id: group.id };
  }
  /**
   * Uploads an image to the folder's server-named `{slot}/{name}` and
   * selects it as the group's cover or avatar (a user-owned kind's).
   */
  async uploadImage(group: { kind: string; id: string }, slot: GroupImageSlot, file: Blob & { name?: string }, o: Omit<NamedOptions, "path"> = {}): Promise<Group> {
    const up = await this.media.uploadNamed(this.folder(group), file, { ...o, path: `${slot}/${file.name || slot}` });
    return this.update(group.id, { [slot]: { image: up.name } });
  }
  /** Groups the signed-in person owns, edits or belongs to. */
  mine(q: MyGroupQuery = {}, signal?: AbortSignal): Promise<MyGroupPage> {
    return call(this.http, "GET", "/me/groups", { query: q, signal });
  }
  merge(id: string, into: string): Promise<MergeReport> {
    return this.write(call(this.http, "POST", "/groups/{id}/merge", { params: { id }, body: { into } }));
  }
  /** The group's memberships in member order, a cursor page at a time. */
  members(id: string, q: MemberQuery = {}, signal?: AbortSignal): Promise<MemberPage> {
    return call(this.http, "GET", "/groups/{id}/members", { params: { id }, query: q, signal });
  }
  addMembers(id: string, members: Membership[]): Promise<Written> {
    return this.write(call(this.http, "POST", "/groups/{id}/members", { params: { id }, body: members }));
  }
  removeMembers(id: string, members: Membership[]): Promise<Written> {
    return this.write(call(this.http, "DELETE", "/groups/{id}/members", { params: { id }, body: members }));
  }
  /** Orders an ordered group: the listed members first, in list order. */
  reorder(id: string, members: MemberRef[]): Promise<Written> {
    return this.write(call(this.http, "PUT", "/groups/{id}/members/order", { params: { id }, body: { members } }));
  }
  /** A content's memberships, its versions' included, with their groups. */
  ofContent(contentId: string, language?: string, signal?: AbortSignal): Promise<ContentMemberships> {
    return call(this.http, "GET", "/contents/{id}/groups", { params: { id: contentId }, query: { language }, signal });
  }
  /** Replaces a content's memberships in the kinds named (every kind its kind joins by default). */
  setOfContent(contentId: string, set: ContentGroups, language?: string): Promise<ContentMemberships> {
    return this.write(call(this.http, "PUT", "/contents/{id}/groups", { params: { id: contentId }, query: { language }, body: set }));
  }

  private async write<T>(p: Promise<T>): Promise<T> {
    const emit = this.http.captureChanges();
    const out = await p;
    emit({ type: "groups.changed" });
    return out;
  }
}
