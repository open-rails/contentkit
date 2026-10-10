// The harness's test-control surface (/__test, e2e/server/control.go).

export interface TestUser {
  id: string;
  username: string;
  email: string;
  password: string;
  role?: Role;
  access_token: string;
}

/** Root-group roles: staff holds every permission; moderator moderates comments; editor writes posts and polls. */
export type Role = "staff" | "moderator" | "editor";

export interface Item {
  kind: string;
  id: string;
  owner?: string;
  access: "full" | "none";
  hidden?: boolean;
  /** Its content code and canonical path, when created with a title. */
  code?: string;
  path?: string;
}

export interface Fault {
  /** The item id the request's path names; any when absent. */
  item?: string;
  /** Sec-Fetch-Dest: "empty" (fetch, XHR, hls.js) or "image". */
  dest?: string;
  /**
   * no-cors, rate-limit, expired, missing: at the proxy in front of media-gateway;
   * drop: a bucket PUT; hold: the worker's bucket requests wait until cleared;
   * api: ContentKit's API answers status with an error reply.
   */
  fault: "no-cors" | "rate-limit" | "expired" | "missing" | "drop" | "hold" | "api";
  /** api: a substring of the request path, and its method. */
  path?: string;
  method?: string;
  status?: number;
  code?: string;
  error?: string;
  retry_after?: number;
  /** drop: only this multipart part. */
  part?: number;
  /** drop: body bytes forwarded before the connection drops. */
  after?: number;
  /** Matching requests let through first. */
  skip?: number;
  /** hold: "browser" holds the browser's PUTs instead of the media worker's requests. */
  from?: "worker" | "browser";
  /** Matching requests to fault; every one when absent. */
  times?: number;
}

export interface Config {
  origin: string;
  media: string;
  namespace: string;
  bucket: string;
  /** ContentKit's mount, AuthKit's JSON API, the ProcessOnUpload upload mount and the mount taking nothing anonymous, as paths. */
  api: string;
  auth_api: string;
  on_upload_api: string;
  members_api: string;
}

export class Harness {
  readonly origin: string;

  constructor(origin: string) {
    this.origin = origin;
  }

  private async call<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await fetch(`${this.origin}/__test${path}`, {
      method,
      headers: body === undefined ? {} : { "Content-Type": "application/json" },
      body: body === undefined ? null : JSON.stringify(body),
    });
    if (!res.ok) throw new Error(`${method} /__test${path}: ${res.status} ${await res.text()}`);
    return (res.status === 204 ? undefined : await res.json()) as T;
  }

  config(): Promise<Config> {
    return this.call("GET", "/config");
  }

  /** A new verified, signed-in account, holding role when given. */
  user(o: { role?: Role } = {}): Promise<TestUser> {
    return this.call("POST", "/users", o);
  }

  /** Registers an item with the host's resolver: owned by owner, locked to viewers when access is "none". */
  item(o: { kind: string; id?: string; owner?: string; access?: "full" | "none"; hidden?: boolean; title?: string }): Promise<Item> {
    return this.call("POST", "/items", o);
  }

  updateItem(ref: { kind: string; id: string }, o: { access?: "full" | "none"; hidden?: boolean }): Promise<Item> {
    return this.call("PATCH", `/items/${ref.kind}/${ref.id}`, o);
  }

  /** The stored bytes of an item's file (by path or upload stem) or public name; null when absent. */
  async object(ref: { kind: string; id: string }, name: string, at: "path" | "public" = "path"): Promise<{ size: number; sha256: string } | null> {
    const res = await fetch(`${this.origin}/__test/object?${new URLSearchParams({ ...ref, [at]: name })}`);
    return res.ok ? res.json() : null;
  }

  faults(rules: Fault[]): Promise<(Fault & { id: number; hits: number })[]> {
    return this.call("POST", "/faults", rules);
  }

  listFaults(): Promise<(Fault & { id: number; hits: number })[]> {
    return this.call("GET", "/faults");
  }

  clearFaults(item?: string): Promise<void> {
    return this.call("DELETE", `/faults${item ? `?item=${item}` : ""}`);
  }

  reset(): Promise<void> {
    return this.call("POST", "/reset");
  }
}
