// Shared by the integration tests: the harness, named accounts, clients
// against the real ContentKit, and a recorder of what a client sends.
import { Blob as NodeBlob, File as NodeFile } from "node:buffer";
import { readFileSync } from "node:fs";
import path from "node:path";
import { inject } from "vitest";
import { createContentKitClient, fetchTransport, type CommitBody, type ContentKitClient, type MediaOptions, type Op, type PresignBody, type RefBody, type Transport } from "../../src/client/index.js";
import { png as pngBytes } from "../support/bytes.js";
import { Harness, type Config, type Role, type TestUser } from "../support/harness.js";

export const harness = () => new Harness(inject("origin"));

// The package root is vitest's root; jsdom files have no file URL to resolve from.
const fixtures = path.resolve(process.cwd(), "e2e", "fixtures");

/** Waits for the worker: generous on loaded shared runners. */
export const wait = { interval: 100, timeout: 60_000 };

/**
 * Accounts by name, created on first load: "staff", "moderator" and "editor"
 * hold their role, every other name none. A name is one account per file.
 */
export class Accounts {
  private readonly users = new Map<string, TestUser>();
  constructor(private readonly h: Harness) {}

  async load(...names: string[]): Promise<void> {
    await Promise.all(
      names.filter((n) => !this.users.has(n)).map(async (n) => {
        const role = (["staff", "moderator", "editor"] as const).find((r) => r === n) as Role | undefined;
        this.users.set(n, await this.h.user({ role }));
      }),
    );
  }

  get(name: string): TestUser {
    const u = this.users.get(name);
    if (!u) throw new Error(`account ${name} not loaded`);
    return u;
  }

  id(name: string): string {
    return this.get(name).id;
  }

  username(name: string): string {
    return this.get(name).username;
  }
}

/** What a client sent: API calls (upload paths, "/read" for reads), storage PUTs, presign and commit bodies. */
export interface Recorder {
  calls: string[];
  puts: string[];
  presigns: PresignBody[];
  commitRequests: CommitBody[];
  commits: Op[][];
  frames: string[];
  fetch: typeof fetch;
  transport: Transport;
}

/**
 * fetchTransport for bodies made in jsdom too: Node's fetch sends only Node's
 * Blobs, and components build files (a frame grab, a crop) with jsdom's.
 */
export const transport: Transport = async (req, body, o) => {
  const sendable = body instanceof NodeBlob ? body : (new NodeBlob([await body.arrayBuffer()], { type: body.type }) as unknown as Blob);
  await fetchTransport(req, sendable, o);
};

export function recorder(): Recorder {
  const r: Recorder = {
    calls: [],
    puts: [],
    presigns: [],
    commitRequests: [],
    commits: [],
    frames: [],
    fetch: async (input, init) => {
      const url = new URL(String(input instanceof Request ? input.url : input));
      const upload = url.pathname.match(/\/media\/upload(\/.*)$/);
      const path = upload ? upload[1]! : /\/media\/[^/]+\/[^/]+/.test(url.pathname) && !url.pathname.includes("/hls/") ? "/read" : url.pathname;
      r.calls.push(path);
      const body = typeof init?.body === "string" ? JSON.parse(init.body) : undefined;
      if (path === "/presign") r.presigns.push(body);
      if (path === "/commit") {
        r.commitRequests.push(body);
        r.commits.push(body.ops);
      }
      if (path === "/frame") r.frames.push(`${url.searchParams.get("t")}@${url.searchParams.get("w")}`);
      return fetch(input, init);
    },
    transport: async (req, body, o) => {
      r.puts.push(req.url);
      await transport(req, body, o);
    },
  };
  return r;
}

export interface ClientOptions {
  /** Records the client's requests. */
  record?: Recorder;
  /** A signed-out visitor's address (X-Forwarded-For). */
  ip?: string;
  /** The second upload mount, whose uploads process as they land. */
  onUpload?: boolean;
  media?: MediaOptions;
}

/** A client signed in as user (null: signed out) on the harness's mount. */
export function client(h: Harness, cfg: Config, user: TestUser | null, o: ClientOptions = {}): ContentKitClient {
  return createContentKitClient({
    baseUrl: `${h.origin}${cfg.api}`,
    mounts: o.onUpload ? { upload: `${h.origin}${cfg.on_upload_api}/media/upload` } : undefined,
    token: () => user?.access_token,
    headers: (): Record<string, string> => (o.ip ? { "X-Forwarded-For": o.ip } : {}),
    fetch: o.record?.fetch,
    media: { transport: o.record?.transport ?? transport, retryDelay: () => 100, ...o.media },
  });
}

/** An item the host knows, owned by owner. */
export async function item(h: Harness, kind: string, owner: TestUser | null, o: { access?: "full" | "none"; hidden?: boolean; title?: string } = {}): Promise<RefBody> {
  const it = await h.item({ kind, owner: owner?.id, ...o });
  return { kind: it.kind, id: it.id };
}

/** A real PNG, each seed distinct, as Node's File (jsdom's Blob is no body Node's fetch can send). */
export function png(name: string, seed = 1, w = 48, h = 32): File {
  return new NodeFile([pngBytes(w, h, seed)], name, { type: "image/png" }) as unknown as File;
}

/** A fixture under e2e/fixtures as Node's File. */
export function fixture(name: string, type: string, as = name): File {
  return new NodeFile([readFileSync(path.resolve(fixtures, name))], as, { type }) as unknown as File;
}
