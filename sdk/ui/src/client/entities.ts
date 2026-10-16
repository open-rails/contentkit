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

/** GET /entities filters, as the route names them. */
export type EntityQuery = RouteQuery<"GET", "/entities">;
/** Staff entity listing, including internal entities. */
export type ManageEntityQuery = RouteQuery<"GET", "/manage/entities">;
/** GET /me/entities filters. */
export type MyEntityQuery = RouteQuery<"GET", "/me/entities">;
/** An entity's image slots: every kind's cover, a user-owned kind's avatar. */
export type EntityImageSlot = "cover" | "avatar";
/** GET /entities/{id}/members paging. */
export type MemberQuery = RouteQuery<"GET", "/entities/{id}/members">;

/** Entities: public listings and pages; writes for staff (root:groups:write). */
export class EntitiesClient {
  constructor(
    private readonly http: Http,
    private readonly media: MediaClient,
  ) {}

  list(q: EntityQuery = {}, signal?: AbortSignal): Promise<GroupPage> {
    return call(this.http, "GET", "/entities", { query: q, signal });
  }
  /** Staff listing, including internal entities hidden from public discovery. */
  manage(q: ManageEntityQuery = {}, signal?: AbortSignal): Promise<GroupPage> {
    return call(this.http, "GET", "/manage/entities", { query: q, signal });
  }
  /** Staff detail, including internal entities. */
  getForManagement(id: string, language?: string, signal?: AbortSignal): Promise<GroupDetail> {
    return call(this.http, "GET", "/manage/entities/{id}", { params: { id }, query: { language }, signal });
  }
  /** A merged group answers with its survivor. */
  get(id: string, language?: string, signal?: AbortSignal): Promise<GroupDetail> {
    return call(this.http, "GET", "/entities/{id}", { params: { id }, query: { language }, signal });
  }
  create(groups: GroupInput[]): Promise<Group[]> {
    return this.write(call(this.http, "POST", "/entities", { body: groups }));
  }
  update(id: string, patch: GroupUpdate): Promise<Group> {
    return this.write(call(this.http, "PATCH", "/entities/{id}", { params: { id }, body: patch }));
  }
  /** Marks the group deleted. */
  remove(id: string): Promise<Group> {
    return this.write(call(this.http, "DELETE", "/entities/{id}", { params: { id } }));
  }
  /** Replaces, adds or removes names and aliases; resolves with the group's names. */
  names(id: string, op: "set" | "add" | "remove", names: Name[]): Promise<Name[]> {
    const method = op === "set" ? "PUT" : op === "add" ? "POST" : "DELETE";
    return this.write(call(this.http, method, "/entities/{id}/names", { params: { id }, body: names }));
  }
  /** Adds or removes edges from the group; resolves with every edge touching it. */
  edges(id: string, op: "add" | "remove", edges: EdgeInput[]): Promise<Edge[]> {
    return this.write(call(this.http, op === "add" ? "POST" : "DELETE", "/entities/{id}/edges", { params: { id }, body: edges }));
  }
  /** The group's media folder: the media kind named after its group kind. */
  folder(group: { kind: string; id: string }): RefBody {
    return { kind: group.kind, id: group.id };
  }
  /**
   * Uploads an image to the folder's server-named `{slot}/{name}` and
   * selects it as the group's cover or avatar (a user-owned kind's).
   */
  async uploadImage(group: { kind: string; id: string }, slot: EntityImageSlot, file: Blob & { name?: string }, o: Omit<NamedOptions, "path"> = {}): Promise<Group> {
    const up = await this.media.uploadNamed(this.folder(group), file, { ...o, path: `${slot}/${file.name || slot}` });
    return this.update(group.id, { [slot]: { image: up.name } });
  }
  /** Entities the signed-in person owns, edits or belongs to. */
  mine(q: MyEntityQuery = {}, signal?: AbortSignal): Promise<MyGroupPage> {
    return call(this.http, "GET", "/me/entities", { query: q, signal });
  }
  merge(id: string, into: string): Promise<MergeReport> {
    return this.write(call(this.http, "POST", "/entities/{id}/merge", { params: { id }, body: { into } }));
  }
  /** The group's memberships in member order, a cursor page at a time. */
  members(id: string, q: MemberQuery = {}, signal?: AbortSignal): Promise<MemberPage> {
    return call(this.http, "GET", "/entities/{id}/members", { params: { id }, query: q, signal });
  }
  addMembers(id: string, members: Membership[]): Promise<Written> {
    return this.write(call(this.http, "POST", "/entities/{id}/members", { params: { id }, body: members }));
  }
  removeMembers(id: string, members: Membership[]): Promise<Written> {
    return this.write(call(this.http, "DELETE", "/entities/{id}/members", { params: { id }, body: members }));
  }
  /** Orders an ordered group: the listed members first, in list order. */
  reorder(id: string, members: MemberRef[]): Promise<Written> {
    return this.write(call(this.http, "PUT", "/entities/{id}/members/order", { params: { id }, body: { members } }));
  }
  /** A content's memberships, its versions' included, with their groups. */
  ofContent(contentId: string, language?: string, signal?: AbortSignal): Promise<ContentMemberships> {
    return call(this.http, "GET", "/contents/{id}/entities", { params: { id: contentId }, query: { language }, signal });
  }
  /** Replaces a content's memberships in the kinds named (every kind its kind joins by default). */
  setOfContent(contentId: string, set: ContentGroups, language?: string): Promise<ContentMemberships> {
    return this.write(call(this.http, "PUT", "/contents/{id}/entities", { params: { id: contentId }, query: { language }, body: set }));
  }

  private async write<T>(p: Promise<T>): Promise<T> {
    const emit = this.http.captureChanges();
    const out = await p;
    emit({ type: "entities.changed" });
    return out;
  }
}
