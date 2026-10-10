// The demo pages run against the e2e harness (e2e/server), on its origin: a
// host page signing in with auth-ui and handing ContentKit its authFetch.
// ?user=&pw= signs that account in; item ids come as query parameters too.
import { createAuthClient } from "@openrails/auth-ui/client";
import { createContentKitClient, type RefBody } from "@openrails/contentkit-ui/client";
import type { ContentKitUiTheme } from "@openrails/contentkit-ui";

export const q = new URLSearchParams(location.search);

export const theme = (q.get("theme") ?? "light") as ContentKitUiTheme;
export const dark = theme === "dark";
document.documentElement.style.colorScheme = dark ? "dark" : "light";
document.body.style.cssText = `margin:0;font-family:Inter,ui-sans-serif,system-ui,sans-serif;background:${dark ? "#09090b" : "#fafafa"};color:${dark ? "#fafafa" : "#09090b"}`;

export const auth = createAuthClient({ baseUrl: "/api/auth/v1" });
const user = q.get("user");
const pw = q.get("pw");
if (user && pw) await auth.signInWithPassword({ identifier: user, password: pw });

const signedIn = auth.getSnapshot();
/** The signed-in account's id; null signed out. */
export const viewer = signedIn.status === "authenticated" ? signedIn.userId : null;

/** ?mount=members: the ContentKit that takes nothing from signed-out visitors. */
export const client = createContentKitClient({
  baseUrl: q.get("mount") === "members" ? "/api/contentkit-members" : "/api/contentkit",
  fetch: auth.authFetch,
  token: () => auth.getAccessToken(),
});

/** The item of kind whose id is the query parameter name; null when absent. */
export function item(name: string, kind: string): RefBody | null {
  const id = q.get(name);
  return id ? { kind, id } : null;
}
