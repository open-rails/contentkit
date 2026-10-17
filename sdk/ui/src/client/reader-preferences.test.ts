// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "./client.js";
import { BrowserReaderPreferences } from "./browser-reader-preferences.js";

const inventory = { applies_to: ["gallery", "blog"], filters: [{ entity: { id: "filter", name: "Filtered", slug: "filtered" }, default_visible: false, visible: false, overridden: false }], include_deleted: false };
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); localStorage.clear(); });

it("persists guest language and filters across clients and browser storage events", () => {
  const one = createContentKitClient({ baseUrl: "https://content.test/prefs-one" }).preferences;
  const two = createContentKitClient({ baseUrl: "https://content.test/prefs-one/" }).preferences;
  const other = createContentKitClient({ baseUrl: "https://content.test/prefs-other" }).preferences;
  const changed = vi.fn();
  const off = two.browser.subscribe(changed);
  one.browser.setLanguage("ja");
  one.browser.setFilters([{ entity_id: "filter", visible: true }]);
  expect(two.browser.snapshot()).toEqual({ language: "ja", filters: [{ entity_id: "filter", visible: true }] });
  expect(other.browser.snapshot().filters).toEqual([]);
  const key = Object.keys(localStorage).find((key) => key.includes("prefs-one"))!;
  const data = JSON.stringify({ language: "es", filters: [{ entity_id: "filter", visible: false }] });
  localStorage.setItem(key, data);
  window.dispatchEvent(new StorageEvent("storage", { key, newValue: data }));
  expect(two.browser.snapshot().language).toBe("es");
  expect(changed).toHaveBeenCalledTimes(3);
  off();
});

it("validates stale stored filters before sending anonymous requests and never sends them for accounts", async () => {
  const calls: { path: string; preferences: string | null }[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/bootstrap", fetch: (async (input: string, init: RequestInit) => {
    calls.push({ path: new URL(input).pathname, preferences: new Headers(init.headers).get("ContentKit-Reader-Preferences") });
    return Response.json(input.includes("content-filters") ? inventory : { contents: [] });
  }) as typeof fetch });
  client.preferences.browser.setFilters([{ entity_id: "obsolete", visible: true }, { entity_id: "filter", visible: true }]);
  client.preferences.configure(null);
  await client.discovery.list();
  expect(calls).toEqual([
    { path: "/bootstrap/v1/content-filters", preferences: null },
    { path: "/bootstrap/v1/contents", preferences: JSON.stringify({ filters: [{ entity_id: "filter", visible: true }] }) },
  ]);
  client.preferences.configure("existing");
  await client.discovery.list();
  expect(calls.at(-1)?.preferences).toBeNull();
  expect(client.preferences.browser.snapshot().filters).toEqual([{ entity_id: "filter", visible: true }, { entity_id: "obsolete", visible: true }]);
});

it("refreshes configured IDs when a database reset invalidates a previously accepted guest filter", async () => {
  let reset = false;
  const headers: (string | null)[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/reset", fetch: (async (input: string, init: RequestInit) => {
    const value = new Headers(init.headers).get("ContentKit-Reader-Preferences");
    if (input.includes("content-filters")) return Response.json(reset ? { applies_to: ["gallery", "blog"], filters: [], include_deleted: false } : inventory);
    headers.push(value);
    if (reset && value) return Response.json({ code: "invalid_request", error: "unknown filter" }, { status: 400 });
    return Response.json({ contents: [] });
  }) as typeof fetch });
  client.preferences.browser.setFilters([{ entity_id: "filter", visible: true }]);
  client.preferences.configure(null);
  await client.discovery.list();
  reset = true;
  await expect(client.discovery.list()).resolves.toEqual({ contents: [] });
  expect(headers).toHaveLength(3);
  expect(headers[2]).toBeNull();
  expect(client.preferences.browser.snapshot().filters).toEqual([{ entity_id: "filter", visible: true }]);
});

it("inherits only an explicit new registration and keeps guest data separate from existing accounts", async () => {
  let language: string | null = "en";
  const initialized: unknown[] = [];
  const save = vi.fn(async (value: string, account: string) => { expect(account).toBe("new"); language = value; });
  const client = createContentKitClient({ baseUrl: "https://content.test/register", fetch: (async (input: string, init: RequestInit) => {
    if (input.endsWith("/initialize")) initialized.push(JSON.parse(String(init.body)));
    return Response.json(inventory);
  }) as typeof fetch });
  client.preferences.browser.setLanguage("es");
  client.preferences.browser.setFilters([{ entity_id: "filter", visible: true }]);
  await client.preferences.inheritAfterRegistration("new");
  client.preferences.configure("new", { load: async () => language, save });
  await vi.waitFor(() => expect(client.preferences.snapshot().pending).toBe(false));
  expect(save).toHaveBeenCalledExactlyOnceWith("es", "new");
  expect(initialized).toEqual([{ account_id: "new", filters: [{ entity_id: "filter", visible: true }], include_deleted: false }]);
  expect(client.preferences.browser.snapshot().language).toBe("es");
  client.preferences.configure("existing", { load: async () => "ja", save });
  await vi.waitFor(() => expect(client.preferences.snapshot().language).toBe("ja"));
  expect(initialized).toHaveLength(1);
  expect(save).toHaveBeenCalledTimes(1);
  client.preferences.configure(null);
  expect(client.preferences.snapshot().language).toBe("es");
});

it("keeps failed registration inheritance durable and retries without overwriting a saved language", async () => {
  let fail = true;
  let language = "en";
  const save = vi.fn(async (value: string) => { language = value; });
  const fetcher = (async (input: string) => {
    if (input.endsWith("/initialize") && fail) return Response.json({ error: "unavailable", code: "unavailable" }, { status: 503 });
    return Response.json(inventory);
  }) as typeof fetch;
  const first = createContentKitClient({ baseUrl: "https://content.test/retry", fetch: fetcher }).preferences;
  first.browser.setLanguage("es");
  first.browser.setFilters([{ entity_id: "filter", visible: true }]);
  await first.inheritAfterRegistration("new");
  first.configure("new", { load: async () => language, save });
  await vi.waitFor(() => expect(first.snapshot().error?.code).toBe("unavailable"));
  expect(first.snapshot().pending).toBe(true);
  expect(save).toHaveBeenCalledOnce();
  expect(first.browser.pending("new")?.languageDone).toBe(true);
  language = "ja";
  fail = false;
  const reloaded = createContentKitClient({ baseUrl: "https://content.test/retry", fetch: fetcher }).preferences;
  reloaded.configure("other", { load: async () => "en", save });
  await vi.waitFor(() => expect(reloaded.snapshot().loading).toBe(false));
  expect(reloaded.browser.pending("new")).toBeDefined();
  reloaded.configure("new", { load: async () => "ja", save });
  await vi.waitFor(() => expect(reloaded.snapshot().pending).toBe(false));
  expect(save).toHaveBeenCalledOnce();
  expect(reloaded.browser.snapshot().language).toBe("es");
});

it("does not overwrite a later account language when retrying a failed initial write", async () => {
  let language = "en";
  let fail = true;
  const save = vi.fn(async (value: string) => { if (fail) throw new Error("offline"); language = value; });
  const preferences = createContentKitClient({ baseUrl: "https://content.test/later-language" }).preferences;
  preferences.browser.setLanguage("es");
  await preferences.inheritAfterRegistration("new");
  preferences.configure("new", { load: async () => language, save });
  await vi.waitFor(() => expect(preferences.snapshot().error).toBeDefined());
  expect(preferences.browser.pending("new")).toMatchObject({ languageObserved: true, initialLanguage: "en" });
  language = "ja";
  fail = false;
  await preferences.retry();
  expect(language).toBe("ja");
  expect(save).toHaveBeenCalledOnce();
  expect(preferences.snapshot().pending).toBe(false);
});

it("treats an explicit account language choice as completing pending inheritance", async () => {
  let language = "en", fail = true;
  const save = vi.fn(async (value: string) => { if (fail) throw new Error("offline"); language = value; });
  const preferences = createContentKitClient({ baseUrl: "https://content.test/explicit-language" }).preferences;
  preferences.browser.setLanguage("es");
  await preferences.inheritAfterRegistration("new");
  preferences.configure("new", { load: async () => language, save });
  await vi.waitFor(() => expect(preferences.snapshot().error).toBeDefined());
  fail = false;
  await preferences.setLanguage("ja");
  await preferences.retry();
  expect(preferences.snapshot().pending).toBe(false);
  expect(language).toBe("ja");
  expect(save.mock.calls.map(([value]) => value)).toEqual(["es", "ja"]);
});

it("does not apply a late language read from the previous account", async () => {
  let resolve!: (language: string | null) => void;
  const preferences = createContentKitClient({ baseUrl: "https://content.test/switch" }).preferences;
  preferences.browser.setLanguage("es");
  await preferences.inheritAfterRegistration("first");
  const save = vi.fn(async () => {});
  preferences.configure("first", { load: () => new Promise((done) => { resolve = done; }), save });
  await vi.waitFor(() => expect(resolve).toBeTypeOf("function"));
  preferences.configure("second", { load: async () => "ja", save });
  resolve(null);
  await vi.waitFor(() => expect(preferences.snapshot().language).toBe("ja"));
  expect(save).not.toHaveBeenCalled();
  expect(preferences.browser.pending("first")).toBeDefined();
});

it.each(["headers", "token"] as const)("does not send the previous account's filter changes after awaiting %s", async (pause) => {
  let resume!: () => void;
  let token = "account-a-token";
  const fetcher = vi.fn(async () => Response.json(inventory));
  const client = createContentKitClient({
    baseUrl: `https://content.test/account-switch-${pause}`,
    fetch: fetcher,
    headers: pause === "headers" ? () => new Promise<HeadersInit>((resolve) => { resume = () => resolve({}); }) : undefined,
    token: pause === "token" ? () => new Promise<string>((resolve) => { resume = () => resolve(token); }) : () => token,
  });
  client.preferences.configure("account-a");
  const result = expect(client.discovery.setContentFilters({ filters: [{ entity_id: "filter", visible: true }], include_deleted: false }))
    .rejects.toMatchObject({ code: "aborted" });
  await vi.waitFor(() => expect(resume).toBeTypeOf("function"));
  token = "account-b-token";
  client.preferences.configure("account-b");
  resume();
  await result;
  expect(fetcher).not.toHaveBeenCalled();
});

it("keeps working in memory when browser storage is unavailable", () => {
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("blocked"); });
  const preferences = createContentKitClient({ baseUrl: "https://content.test/blocked-storage" }).preferences;
  expect(() => preferences.browser.setLanguage("es")).not.toThrow();
  expect(preferences.browser.snapshot().language).toBe("es");
});

it("does not share anonymous preferences between server-rendered requests", () => {
  vi.stubGlobal("window", undefined);
  const one = createContentKitClient({ baseUrl: "/server-render" }).preferences;
  const two = createContentKitClient({ baseUrl: "/server-render" }).preferences;
  one.browser.setLanguage("es");
  expect(two.browser.snapshot().language).toBeUndefined();
});


it("inherits every domain's saved shared choices and keeps them pending until accepted", async () => {
  let fail = true;
  const attempts: unknown[] = [];
  const urls: string[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/all-domain-registration", fetch: async (input, init) => {
    urls.push(String(input));
    attempts.push(JSON.parse(String(init?.body)));
    return fail ? Response.json({ code: "unavailable", error: "Retry" }, { status: 503 }) : Response.json(inventory);
  } });
  const filters = [
    { entity_id: "blog-only", visible: false },
    { entity_id: "current", visible: true },
    { entity_id: "other-domain", visible: false },
    { entity_id: "stale", visible: true },
  ];
  client.preferences.browser.setFilters(filters);
  await client.preferences.inheritAfterRegistration("new");
  client.preferences.configure("new", { load: async () => null, save: async () => {} });
  await vi.waitFor(() => expect(client.preferences.snapshot().error?.code).toBe("unavailable"));
  expect(client.preferences.snapshot().pending).toBe(true);
  expect(client.preferences.browser.pending("new")?.filters).toEqual(filters);
  fail = false;
  await client.preferences.retry();
  expect(client.preferences.snapshot().pending).toBe(false);
  expect(attempts).toEqual([
    { account_id: "new", filters, include_deleted: false },
    { account_id: "new", filters, include_deleted: false },
  ]);
  expect(urls.every((url) => url.endsWith("/me/content-filters/initialize"))).toBe(true);
});

it("migrates unscoped choices and a pending registration without modifying the v1 backup", async () => {
  const baseUrl = "https://content.test/legacy-migration";
  const key = `contentkit:reader-preferences:v1:${baseUrl}/v1`;
  const backup = JSON.stringify({ language: "es", filters: [{ group_id: "filter", visible: true }, { group_id: "other-domain", visible: false }], registrations: [{ account: "new", languageDone: true, filtersDone: false, filters: [{ group_id: "other-domain", visible: false }] }] });
  localStorage.setItem(key, backup);
  const calls: { body: unknown; preferences: string | null }[] = [];
  const client = createContentKitClient({ baseUrl, fetch: async (_url, init) => {
    calls.push({ body: init?.body ? JSON.parse(String(init.body)) : null, preferences: new Headers(init?.headers).get("ContentKit-Reader-Preferences") });
    return Response.json(inventory);
  } });
  expect(client.preferences.browser.snapshot()).toEqual({ language: "es", filters: [{ entity_id: "filter", visible: true }, { entity_id: "other-domain", visible: false }] });
  expect(localStorage.getItem(key)).toBe(backup);
  expect(localStorage.getItem(`contentkit:reader-preferences:v2:${baseUrl}/v1`)).not.toBeNull();
  client.preferences.configure(null);
  await client.discovery.list();
  expect(calls.at(-1)?.preferences).toBe(JSON.stringify({ filters: [{ entity_id: "filter", visible: true }] }));
  client.preferences.configure("new", { load: async () => "en", save: async () => {} });
  await vi.waitFor(() => expect(client.preferences.snapshot().pending).toBe(false));
  expect(calls.at(-1)?.body).toEqual({ account_id: "new", filters: [{ entity_id: "other-domain", visible: false }], include_deleted: false });
  expect(localStorage.getItem(key)).toBe(backup);
});

it("merges conflicting legacy kinds into one shared header choice and retains off-domain choices", async () => {
  const baseUrl = "https://content.test/shared-header";
  localStorage.setItem(`contentkit:reader-preferences:v1:${baseUrl}/v1`, JSON.stringify({ kinds: { gallery: [{ entity_id: "filter", visible: false }], blog: [{ entity_id: "filter", visible: true }], video: [{ entity_id: "off-domain", visible: false }] } }));
  const calls: (string | null)[] = [];
  const client = createContentKitClient({ baseUrl, fetch: async (url, init) => {
    calls.push(new Headers(init?.headers).get("ContentKit-Reader-Preferences"));
    return Response.json(String(url).includes("content-filters") ? inventory : { contents: [] });
  } });
  client.preferences.configure(null);
  await client.discovery.list();
  expect(JSON.parse(calls.at(-1)!)).toEqual({ filters: [{ entity_id: "filter", visible: false }] });
  expect(client.preferences.browser.snapshot().filters).toEqual([{ entity_id: "filter", visible: false }, { entity_id: "off-domain", visible: false }]);
});

it("sends and inherits a default reset without reviving the old unscoped choice", async () => {
  const baseUrl = "https://content.test/default-inheritance";
  localStorage.setItem(`contentkit:reader-preferences:v1:${baseUrl}/v1`, JSON.stringify({ filters: [{ group_id: "filter", visible: false }] }));
  const requests: { body: unknown; preferences: string | null }[] = [];
  const client = createContentKitClient({ baseUrl, fetch: async (_url, init) => {
    requests.push({ body: init?.body ? JSON.parse(String(init.body)) : null, preferences: new Headers(init?.headers).get("ContentKit-Reader-Preferences") });
    return Response.json(inventory);
  } });
  client.preferences.browser.setFilters([{ entity_id: "filter", default: true }]);
  client.preferences.configure(null);
  await client.discovery.list();
  expect(JSON.parse(requests.at(-1)!.preferences!)).toEqual({ filters: [{ entity_id: "filter", default: true }] });
  await client.preferences.inheritAfterRegistration("new");
  client.preferences.configure("new", { load: async () => null, save: async () => {} });
  await vi.waitFor(() => expect(client.preferences.snapshot().pending).toBe(false));
  expect(requests.at(-1)?.body).toEqual({ account_id: "new", filters: [{ entity_id: "filter", default: true }], include_deleted: false });
});

it.each(["entries", "bytes"] as const)("preserves previous choices when a guest edit exceeds the header %s limit", async (limit) => {
  const baseUrl = `https://content.test/capacity-${limit}`;
  let legacy = Array.from({ length: 128 }, (_, i) => ({ entity_id: limit === "entries" ? `tag-${i}` : `id-${String(i).padStart(33, "0")}`, visible: false }));
  if (limit === "bytes") {
    while (new TextEncoder().encode(JSON.stringify({ filters: legacy })).length > 8192) legacy = legacy.slice(0, -1);
    expect(legacy.length).toBeLessThan(128);
  }
  const extra = "id-new-000000000000000000000000000000";
  localStorage.setItem(`contentkit:reader-preferences:v1:${baseUrl}/v1`, JSON.stringify({ legacy_filters: legacy }));
  const rows = [...legacy.map((f) => f.entity_id), extra].map((id) => ({ entity: { id, name: id, slug: id }, default_visible: true, visible: true, overridden: false }));
  const client = createContentKitClient({ baseUrl, fetch: async () => Response.json({ applies_to: ["gallery", "blog"], filters: rows, include_deleted: false }) });
  client.preferences.configure(null);
  await client.discovery.contentFilters();
  const before = client.preferences.browser.snapshot();
  expect(() => client.preferences.setGuestFilters([{ entity_id: extra, visible: false }])).toThrow("previous choices have been kept");
  expect(client.preferences.browser.snapshot()).toEqual(before);
  expect(before.filters).toHaveLength(legacy.length);
});

it.each([
  { name: "hide beats show", kinds: { gallery: [{ entity_id: "shared", visible: true }], blog: [{ entity_id: "shared", visible: false }] }, expected: { entity_id: "shared", visible: false } },
  { name: "hide beats later show", kinds: { gallery: [{ entity_id: "shared", visible: false }], blog: [{ entity_id: "shared", visible: true }] }, expected: { entity_id: "shared", visible: false } },
  { name: "explicit choice beats scoped default", kinds: { gallery: [{ entity_id: "shared", default: true }], blog: [{ entity_id: "shared", visible: true }] }, expected: { entity_id: "shared", visible: true } },
  { name: "later default does not override explicit choice", kinds: { gallery: [{ entity_id: "shared", visible: true }], blog: [{ entity_id: "shared", default: true }] }, expected: { entity_id: "shared", visible: true } },
  { name: "scoped default suppresses global fallback", kinds: { gallery: [{ entity_id: "shared", default: true }] }, expected: { entity_id: "shared", default: true } },
  { name: "unscoped fallback without any scoped choice", kinds: {}, expected: { entity_id: "shared", visible: false } },
])("migrates v1 conflict policy: $name", ({ name, kinds, expected }) => {
  const key = `v2:${name}`, legacyKey = `v1:${name}`;
  const backup = JSON.stringify({ language: "ja", legacy_filters: [{ entity_id: "shared", visible: false }], kinds, registrations: [{ account: "new", language: "ja", languageDone: false, filtersDone: false, languageObserved: true, initialLanguage: "en", legacy_filters: [{ entity_id: "shared", visible: false }], kinds }] });
  localStorage.setItem(legacyKey, backup);
  const store = new BrowserReaderPreferences(key, window, legacyKey);
  expect(store.snapshot()).toEqual({ language: "ja", filters: [expected] });
  expect(store.pending("new")).toMatchObject({ account: "new", language: "ja", filters: [expected], languageDone: false, filtersDone: false, languageObserved: true, initialLanguage: "en" });
  expect(localStorage.getItem(legacyKey)).toBe(backup);
});

it("keeps canonical v2 choices authoritative and ignores old-tab v1 writes", () => {
  const key = "canonical-v2", legacyKey = "canonical-v1";
  localStorage.setItem(legacyKey, JSON.stringify({ language: "es", kinds: { gallery: [{ entity_id: "shared", visible: false }] } }));
  localStorage.setItem(key, JSON.stringify({ language: "ja", filters: [{ entity_id: "shared", visible: true }] }));
  const store = new BrowserReaderPreferences(key, window, legacyKey);
  const changed = vi.fn();
  const off = store.subscribe(changed);
  const oldTab = JSON.stringify({ language: "en", kinds: { blog: [{ entity_id: "shared", visible: false }] } });
  localStorage.setItem(legacyKey, oldTab);
  window.dispatchEvent(new StorageEvent("storage", { key: legacyKey, newValue: oldTab }));
  expect(store.snapshot()).toEqual({ language: "ja", filters: [{ entity_id: "shared", visible: true }] });
  expect(changed).not.toHaveBeenCalled();
  off();
  localStorage.setItem("empty-v2", JSON.stringify({ filters: [] }));
  const empty = new BrowserReaderPreferences("empty-v2", window, legacyKey);
  expect(empty.snapshot()).toEqual({ filters: [] });
});

it("merges stale browser-tab edits and keeps captured registration data intact", () => {
  const one = new BrowserReaderPreferences("shared-tabs-v2", window);
  const two = new BrowserReaderPreferences("shared-tabs-v2", window);
  one.setFilters([{ entity_id: "gallery-tag", visible: false }]);
  one.register("new");
  two.setFilters([{ entity_id: "blog-tag", visible: false }]);
  one.setFilters([{ entity_id: "gallery-tag", visible: true }]);
  two.setLanguage("es");
  expect(two.snapshot()).toEqual({ language: "es", filters: [{ entity_id: "blog-tag", visible: false }, { entity_id: "gallery-tag", visible: true }] });
  expect(two.pending("new")?.filters).toEqual([{ entity_id: "gallery-tag", visible: false }]);
  expect(one.pending("new")?.filters).toEqual([{ entity_id: "gallery-tag", visible: false }]);
});
