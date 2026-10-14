// The real ContentKit the integration and browser suites run against: the
// compose services (e2e/compose.yaml) under a fresh project name, and the
// harness server (e2e/server) on the host. startStack publishes it as
// CKUI_E2E_* environment variables; a stack already published there is
// reused, so `pnpm test:e2e` starts one for both suites.
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { randomBytes } from "node:crypto";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { createServer, type Server } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";

const e2e = path.resolve(import.meta.dirname, "..");
const compose = path.join(e2e, "compose.yaml");

export interface Stack {
  /** The app, API and /__test origin. */
  origin: string;
  /** The media origin: the fault proxy in front of media-gateway. */
  media: string;
  stop(): Promise<void>;
}

const env = (name: string) => process.env[name];

/** The published stack, if one runs. */
export function published(): Pick<Stack, "origin" | "media"> | null {
  const origin = env("CKUI_E2E_ORIGIN");
  const media = env("CKUI_E2E_MEDIA");
  return origin && media ? { origin, media } : null;
}

export interface StackOptions {
  /** Served at /: the built demo apps. */
  static?: string;
  log?: (line: string) => void;
}

export async function startStack(o: StackOptions = {}): Promise<Stack> {
  const running = published();
  if (running) return { ...running, stop: async () => {} };
  const log = o.log ?? ((l: string) => process.stderr.write(`[e2e] ${l}\n`));
  const project = `ckui-e2e-${process.pid}-${Date.now().toString(36)}`;
  const work = mkdtempSync(path.join(tmpdir(), "ckui-e2e-"));
  const [port, mediaPort, pgPort, minioPort, gatewayPort, workerPort] = await freePorts(6);
  const origin = `http://localhost:${port}`;
  const tokenKey = `e2e:${randomBytes(32).toString("base64")}`;
  const composeEnv = {
    ...process.env,
    E2E_ORIGIN: origin,
    E2E_TOKEN_KEY: tokenKey,
    E2E_APP_PORT: String(port),
    E2E_PG_PORT: String(pgPort),
    E2E_MINIO_PORT: String(minioPort),
    E2E_GATEWAY_PORT: String(gatewayPort),
    E2E_WORKER_PORT: String(workerPort),
  };
  const dc = (...args: string[]) =>
    execFileSync("docker", ["compose", "-p", project, "-f", compose, ...args], { env: composeEnv, encoding: "utf8", stdio: ["ignore", "pipe", "inherit"] });
  let server: ChildProcess | undefined;
  const stop = async () => {
    if (server && server.exitCode === null) {
      server.kill("SIGTERM");
      await new Promise((r) => server!.once("exit", r));
    }
    if (env("CKUI_E2E_KEEP_LOGS")) {
      try {
        process.stderr.write(dc("--profile", "worker", "logs", "--no-color", "media-worker", "media-gateway"));
      } catch {
        // already gone
      }
    }
    try {
      dc("--profile", "worker", "down", "-v", "--remove-orphans", "-t", "2");
    } catch (err) {
      log(`compose down: ${(err as Error).message}`);
    }
    rmSync(work, { recursive: true, force: true });
  };
  try {
    log(`starting ${project}`);
    dc("--profile", "worker", "up", "-d", "--wait", "--quiet-pull", "postgres", "minio", "media-gateway");
    const dsn = `postgres://contentkit:contentkit@127.0.0.1:${pgPort}/contentkit?sslmode=disable`;
    const bin = path.join(work, "ckui-e2e-server");
    execFileSync("go", ["build", "-o", bin, "."], { cwd: path.join(e2e, "server"), env: { ...process.env, GOWORK: "off" }, stdio: "inherit" });
    const args = [
      "-addr", `127.0.0.1:${port}`,
      "-origin", origin,
      "-media-addr", `127.0.0.1:${mediaPort}`,
      "-gateway", `http://127.0.0.1:${gatewayPort}`,
      "-dsn", dsn,
      "-s3-endpoint", `http://127.0.0.1:${minioPort}`,
      "-token-key", tokenKey,
      "-kinds", path.join(e2e, "server", "kinds.json"),
      // It dies with this process (stdin), idles out, and takes the stack down with it.
      "-stdin",
      "-idle", "10m",
      "-lifetime", "90m",
      "-teardown", JSON.stringify(["docker", "compose", "-p", project, "-f", compose, "--profile", "worker", "down", "-v", "--remove-orphans", "-t", "2"]),
    ];
    if (o.static) args.push("-static", o.static);
    server = spawn(bin, args, { stdio: ["pipe", "pipe", "inherit"], env: { ...process.env, ...composeEnv } });
    const ready = await new Promise<string[]>((resolve, reject) => {
      server!.once("exit", (code) => reject(new Error(`e2e server exited ${code}`)));
      createInterface({ input: server!.stdout! }).on("line", (l) => (l.startsWith("READY ") ? resolve(l.slice(6).split(" ")) : log(l)));
    });
    // The worker runs no DDL: it starts once the harness has migrated.
    dc("--profile", "worker", "up", "-d", "--quiet-pull", "media-worker");
    const stack = { origin: ready[0]!, media: ready[1]!, stop };
    process.env.CKUI_E2E_ORIGIN = stack.origin;
    process.env.CKUI_E2E_MEDIA = stack.media;
    log(`ready at ${stack.origin} (media ${stack.media})`);
    return stack;
  } catch (err) {
    await stop();
    throw err;
  }
}

export const builtDemo = (root = path.resolve(e2e, "..")) => existsSync(path.join(root, "dist", "index.js"));

/** n distinct free loopback ports, held together so none repeats. */
async function freePorts(n: number): Promise<number[]> {
  const servers = await Promise.all(
    Array.from({ length: n }, () => new Promise<Server>((resolve, reject) => {
      const srv = createServer();
      srv.once("error", reject);
      srv.listen(0, "127.0.0.1", () => resolve(srv));
    })),
  );
  const ports = servers.map((s) => (s.address() as { port: number }).port);
  await Promise.all(servers.map((s) => new Promise((r) => s.close(r))));
  return ports;
}
