// Read-only items the browser specs share, seeded once per run, and the
// generated 4K source of the ABR ladder.
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, renameSync } from "node:fs";
import path from "node:path";
import { Harness, type Item, type TestUser } from "./harness.ts";
import { mediaClient, processed, seedAlbum, seedWatch } from "./seed.ts";

export interface SharedFixtures {
  owner: TestUser;
  /** Signed in on the gallery pages: not the albums' owner, so "none" locks. */
  viewer: TestUser;
  post: Item;
  locked: Item;
  single: Item;
  /** One landscape clip each, for the player's failure modes. */
  player: Record<"no-cors" | "refresh" | "denied" | "busy" | "missing", Item>;
  abr: Item;
  /** The watch page's video: two audio languages, two subtitle uploads, an MP4 download. */
  watch: Item;
}

const cache = path.resolve(import.meta.dirname, "..", ".cache");

/** A 20 s 2160p test-pattern source, generated once per checkout. */
export function abrSource(): string {
  const out = path.join(cache, "abr-2160-v2.mp4");
  if (existsSync(out)) return out;
  mkdirSync(cache, { recursive: true });
  const tmp = `${out}.partial.mp4`;
  execFileSync("nice", ["-n", "19", "ffmpeg", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=3840x2160:rate=24:duration=20",
    "-c:v", "libx264", "-preset", "ultrafast", "-crf", "36", "-pix_fmt", "yuv420p", "-threads", "2",
    "-movflags", "+faststart", tmp], { stdio: "inherit" });
  renameSync(tmp, out);
  return out;
}

export async function seedShared(h: Harness): Promise<SharedFixtures> {
  const [owner, viewer] = await Promise.all([h.user(), h.user()]);
  const images = ["media/img/1.jpg", "media/img/2.jpg", "media/img/3.jpg", "media/img/4.jpg"];
  const clip = ["landscape.mp4"];
  const abr = (async () => {
    const it = await h.item({ kind: "abr", owner: owner.id });
    const ref = { kind: it.kind, id: it.id };
    await mediaClient(h, owner).put(new File([readFileSync(abrSource())], "abr.mp4", { type: "video/mp4" }), { ref, path: "source" });
    await processed(h, owner, ref);
    return it;
  })();
  const watch = seedWatch(h, owner);
  const [post, locked, single, nocors, refresh, denied, busy, missing] = await Promise.all([
    seedAlbum(h, owner, [...images, "landscape.mp4", "portrait.mp4"]),
    seedAlbum(h, owner, [...images.slice(0, 3), "landscape.mp4", "portrait.mp4"], { access: "none" }),
    seedAlbum(h, owner, ["portrait.mp4"]),
    ...Array.from({ length: 5 }, () => seedAlbum(h, owner, clip)),
  ]);
  return { owner, viewer, post: post!, locked: locked!, single: single!, player: { "no-cors": nocors!, refresh: refresh!, denied: denied!, busy: busy!, missing: missing! }, abr: await abr, watch: await watch };
}

/** The fixtures the global setup published (CKUI_E2E_FIXTURES). */
export function shared(): SharedFixtures {
  const raw = process.env.CKUI_E2E_FIXTURES;
  if (!raw) throw new Error("CKUI_E2E_FIXTURES is unset: run through playwright's global setup");
  return JSON.parse(raw) as SharedFixtures;
}
