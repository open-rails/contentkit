import type { TestProject } from "vitest/node";
import { startStack } from "./stack.ts";

/** Explicit opt-in: default integration/browser runs keep their no-signal plane. */
export default async function setup(project: TestProject) {
  if (!process.env.CKUI_E2E_CH_ADDR) throw new Error("signals integration requires CKUI_E2E_CH_ADDR and an isolated external ClickHouse fixture");
  const stack = await startStack();
  project.provide("origin", stack.origin);
  project.provide("media", stack.media);
  return stack.stop;
}
