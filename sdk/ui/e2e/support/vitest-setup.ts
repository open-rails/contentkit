import type { TestProject } from "vitest/node";
import { startStack } from "./stack.ts";

declare module "vitest" {
  export interface ProvidedContext {
    origin: string;
    media: string;
  }
}

// Starts the stack for the integration project, or reuses the one
// `pnpm test:e2e` published.
export default async function setup(project: TestProject) {
  const stack = await startStack();
  project.provide("origin", stack.origin);
  project.provide("media", stack.media);
  return stack.stop;
}
