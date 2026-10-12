// `pnpm demo`: the stack with the demo pages and seeded items, until Ctrl-C
// or stdin closes. Run `pnpm build` first.
import { buildDemo } from "./demo.ts";
import { seedShared } from "./fixtures.ts";
import { Harness, itemParam } from "./harness.ts";
import { seedChannel, seedSocial, seedVideo } from "./seed.ts";
import { startStack } from "./stack.ts";

const stack = await startStack({ static: await buildDemo() });
let stopping = false;
const stop = async () => {
  if (stopping) return;
  stopping = true;
  await stack.stop();
  process.exit(0);
};
process.on("SIGINT", () => void stop());
process.on("SIGTERM", () => void stop());
process.stdin.on("end", () => void stop());
process.stdin.resume();
try {
  const h = new Harness(stack.origin);
  const f = await seedShared(h);
  const [channel, empty, video, draft, social] = await Promise.all([
    seedChannel(h, f.owner),
    h.item({ kind: "channel", owner: f.owner.id }),
    seedVideo(h, f.owner),
    h.item({ kind: "album", owner: f.owner.id }),
    seedSocial(h, { viewer: f.viewer }),
  ]);
  const as = (u: typeof f.owner) => `user=${encodeURIComponent(u.email)}&pw=${u.password}`;
  const p = f.player;
  process.stdout.write(
    [
      `profile  ${stack.origin}/?${as(f.owner)}&channel=${channel.id}&empty=${empty.id}&video=${itemParam(video)}`,
      `gallery  ${stack.origin}/gallery.html?${as(f.viewer)}&post=${itemParam(f.post)}&locked=${itemParam(f.locked)}&single=${itemParam(f.single)}`,
      `player   ${stack.origin}/gallery.html?page=player&${as(f.viewer)}&no-cors=${itemParam(p["no-cors"])}&refresh=${itemParam(p.refresh)}&denied=${itemParam(p.denied)}&busy=${itemParam(p.busy)}&missing=${itemParam(p.missing)}`,
      `abr      ${stack.origin}/gallery.html?page=abr&${as(f.viewer)}&abr=${itemParam(f.abr)}`,
      `watch    ${stack.origin}/gallery.html?page=watch&${as(f.viewer)}&watch=${itemParam(f.watch)}`,
      `folder   ${stack.origin}/folder.html?${as(f.owner)}&post=${itemParam(f.post)}&draft=${itemParam(draft)}`,
      `social   ${stack.origin}/social.html?${as(f.viewer)}&item=${social.item.id}&poll=${social.poll.id}`,
      "Ctrl-C stops the stack.",
      "",
    ].join("\n"),
  );
} catch (err) {
  console.error(err);
  await stop();
}
