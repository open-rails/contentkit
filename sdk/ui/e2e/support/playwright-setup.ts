import { buildDemo } from "./demo.ts";
import { seedShared } from "./fixtures.ts";
import { Harness } from "./harness.ts";
import { startStack } from "./stack.ts";

// Builds the demo pages, starts the stack (or reuses the published one) and
// seeds the shared items; workers read CKUI_E2E_* from the environment.
export default async function setup() {
  const stack = await startStack({ static: await buildDemo() });
  try {
    process.env.CKUI_E2E_FIXTURES ??= JSON.stringify(await seedShared(new Harness(stack.origin)));
  } catch (err) {
    await stack.stop();
    throw err;
  }
  return stack.stop;
}
