import type {
  Assignment,
  AssignmentsWritten,
  ContentRef,
  Count,
  CountsRebuilt,
  Edge,
  EdgesWritten,
  EffectiveTagsOf,
  MergeReport,
  Name,
  Node,
  NodeDetail,
  NodeInput,
  NodePage,
  NodeUpdate,
} from "./generated/wire.js";
import type { Http } from "./http.js";
import { call, type RouteQuery } from "./route.js";

/** GET /taxonomy/nodes filters, as the route names them. */
export type NodeQuery = RouteQuery<"GET", "/nodes">;

/** The taxonomy admin API (Taxonomy permission). */
export class TaxonomyClient {
  constructor(private readonly http: Http) {}

  nodes(q: NodeQuery = {}, signal?: AbortSignal): Promise<NodePage> {
    return call(this.http, "GET", "/nodes", { query: q, signal });
  }
  node(id: string, signal?: AbortSignal): Promise<NodeDetail> {
    return call(this.http, "GET", "/nodes/{id}", { params: { id }, signal });
  }
  createNodes(nodes: NodeInput[]): Promise<Node[]> {
    return this.write(call(this.http, "POST", "/nodes", { body: nodes }));
  }
  updateNode(id: string, patch: NodeUpdate): Promise<Node> {
    return this.write(call(this.http, "PATCH", "/nodes/{id}", { params: { id }, body: patch }));
  }
  /** Marks the node deleted. */
  deleteNode(id: string): Promise<Node> {
    return this.write(call(this.http, "DELETE", "/nodes/{id}", { params: { id } }));
  }
  /** Replaces, adds or removes names. */
  names(id: string, op: "set" | "add" | "remove", names: Name[]): Promise<NodeDetail> {
    const method = op === "set" ? "PUT" : op === "add" ? "POST" : "DELETE";
    return this.write(call(this.http, method, "/nodes/{id}/names", { params: { id }, body: names }));
  }
  merge(id: string, into: string): Promise<MergeReport> {
    return this.write(call(this.http, "POST", "/nodes/{id}/merge", { params: { id }, body: { into_taxonomy_id: into } }));
  }
  edges(op: "add" | "remove", edges: Edge[]): Promise<EdgesWritten> {
    return this.write(call(this.http, op === "add" ? "POST" : "DELETE", "/edges", { body: edges }));
  }
  /** Assigns or unassigns nodes; suppressCounts skips count upkeep (rebuildCounts afterwards). */
  assignments(op: "add" | "remove", assignments: Assignment[], o: { suppressCounts?: boolean } = {}): Promise<AssignmentsWritten> {
    return this.write(call(this.http, op === "add" ? "POST" : "DELETE", "/assignments", { body: assignments, query: { suppress_counts: o.suppressCounts } }));
  }
  /** The effective tags (work and version) of each content. */
  effective(refs: ContentRef[], signal?: AbortSignal): Promise<EffectiveTagsOf[]> {
    return call(this.http, "POST", "/effective", { body: refs, signal });
  }
  /** Content counts of nodes, by taxonomy id. */
  counts(ids: readonly string[], signal?: AbortSignal): Promise<Record<string, Count[]>> {
    return call(this.http, "GET", "/counts", { query: { taxonomy_id: ids }, signal });
  }
  rebuildCounts(): Promise<CountsRebuilt> {
    return this.write(call(this.http, "POST", "/counts/rebuild"));
  }

  private async write<T>(p: Promise<T>): Promise<T> {
    const out = await p;
    this.http.emit({ type: "taxonomy.changed" });
    return out;
  }
}
