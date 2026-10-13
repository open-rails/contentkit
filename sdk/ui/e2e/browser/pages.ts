import { Harness, type TestUser } from "../support/harness.ts";

export { itemParam, mediaRef } from "../support/harness.ts";

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

/** The admin fixture (demo/admin.tsx) at path, signed in as user; mode "router" mounts the flow in React Router. */
export function admin(path: string, user: TestUser | null, o: { mode: "alone" | "router"; theme?: "light" | "dark"; params?: Record<string, string | undefined> }): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(o.params ?? {})) if (v) q.set(k, v);
  if (o.mode === "router") q.set("router", "1");
  if (o.theme) q.set("theme", o.theme);
  if (user) {
    q.set("user", user.email);
    q.set("pw", user.password);
  }
  return `${path}?${q}`;
}

/** The flow's text color is the host's foreground: theme "inherit" took the host's shadcn variables. */
export async function inheritsHostTheme(page: import("@playwright/test").Page, flow: string): Promise<boolean> {
  return page.evaluate((flow) => {
    const host = document.querySelector("[data-demo=host-foreground]")!;
    const root = document.querySelector(`[data-ckui=${flow}]`)!;
    return root.getAttribute("data-ckui-theme") === "inherit" && getComputedStyle(root).color === getComputedStyle(host).color;
  }, flow);
}
