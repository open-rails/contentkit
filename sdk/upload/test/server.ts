import { spawn, type ChildProcess } from "node:child_process";
import { execFileSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { createInterface } from "node:readline";

/**
 * An HTTP proxy in front of the bucket (the presign host) that can drop the
 * connection of a part upload after 2 MiB of its body.
 */
export class KillProxy {
  readonly server: http.Server;
  kills = 0;
  private target?: number | "next";

  constructor(upstream: URL) {
    this.server = http.createServer((req, res) => {
      const up = http.request(
        { host: upstream.hostname, port: upstream.port, method: req.method, path: req.url, headers: req.headers },
        (ur) => {
          res.writeHead(ur.statusCode!, ur.headers);
          ur.pipe(res);
        },
      );
      up.on("error", () => res.destroy());
      const part = Number(new URL(req.url!, "http://x").searchParams.get("partNumber"));
      const kill = part > 0 && (this.target === "next" || this.target === part);
      if (kill) this.target = undefined;
      let sent = 0;
      req.on("data", (chunk: Buffer) => {
        sent += chunk.length;
        if (kill && sent >= 2 * 1024 * 1024) {
          if (!up.destroyed) this.kills++;
          up.destroy();
          req.socket.destroy();
          return;
        }
        up.write(chunk);
      });
      req.on("end", () => up.end());
      req.on("close", () => req.complete || up.destroy());
    });
  }

  async listen(): Promise<string> {
    await new Promise<void>((r) => this.server.listen(0, "127.0.0.1", r));
    return `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
  }

  /** Drops part number n (or the next part) mid-body, once. */
  kill(n: number | "next"): void {
    this.target = n;
  }

  close(): Promise<void> {
    this.server.closeAllConnections();
    return new Promise((r) => this.server.close(() => r()));
  }
}

/** Builds and starts media/internal/uploadtestserver; resolves its base URL. */
export async function startServer(publicEndpoint: string): Promise<{ url: string; proc: ChildProcess }> {
  const root = resolve(import.meta.dirname, "../../..");
  const bin = join(mkdtempSync(join(tmpdir(), "ck-sdk-")), "uploadtestserver");
  execFileSync("go", ["build", "-o", bin, "./media/internal/uploadtestserver"], { cwd: root, stdio: "inherit" });
  const proc = spawn(bin, ["-public", publicEndpoint, "-grace", "20s"], { stdio: ["pipe", "pipe", "inherit"] });
  const lines = createInterface({ input: proc.stdout! });
  const url = await new Promise<string>((resolve, reject) => {
    proc.once("exit", (code) => reject(new Error(`uploadtestserver exited ${code}`)));
    lines.on("line", (l) => l.startsWith("READY ") && resolve(l.slice(6)));
  });
  return { url, proc };
}

export function stopServer(proc: ChildProcess): Promise<void> {
  return new Promise((r) => {
    proc.once("exit", () => r());
    proc.stdin!.end();
  });
}
