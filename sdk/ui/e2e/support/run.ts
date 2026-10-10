// `pnpm test:e2e [vitest filters] [-- playwright args]`: one stack for the
// integration project and the browser specs, always taken down. Run
// `pnpm build` first (the demo pages use dist).
import { spawnSync } from "node:child_process";
import { buildDemo } from "./demo.ts";
import { startStack } from "./stack.ts";

const stack = await startStack({ static: await buildDemo() });
let failed = false;
const stop = async () => {
  await stack.stop();
  process.exit(failed ? 1 : 0);
};
process.on("SIGINT", () => void stop());
process.on("SIGTERM", () => void stop());
const argv = process.argv.slice(2);
const cut = argv.includes("--") ? argv.indexOf("--") : argv.length;
const vitest = argv.slice(0, cut);
for (const [cmd, ...args] of [
  ["vitest", "run", "--project", "integration", "--passWithNoTests", ...vitest],
  ["vitest", "run", "--project", "integration-dom", "--passWithNoTests", ...vitest],
  ["playwright", "test", ...argv.slice(cut + 1)],
]) {
  // Vite's build of the demo pages set NODE_ENV=production in this process:
  // the suites need React's development build (act).
  const { NODE_ENV: _built, ...env } = process.env;
  const r = spawnSync("pnpm", ["exec", cmd!, ...args], { stdio: "inherit", env });
  failed ||= r.status !== 0;
}
await stop();
