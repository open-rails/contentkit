import { Harness, type TestUser } from "../support/harness.ts";

export const harness = () => new Harness(process.env.CKUI_E2E_ORIGIN!);

export const screenshots = process.env.SCREENSHOT_DIR ?? "test-results/screenshots";

/** A demo page signed in as user, with item ids as query parameters. */
export function page(file: "" | `${string}.html`, user: TestUser | null, params: Record<string, string | undefined>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v) q.set(k, v);
  if (user) {
    q.set("user", user.email);
    q.set("pw", user.password);
  }
  return `/${file}?${q}`;
}
