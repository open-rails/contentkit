import type { ContentKitClient } from "./client.js";
import { eventId, type Attribution } from "./beacons.js";

const PARAM = "ck_nav", PREFIX = "contentkit.navigation.v1.", MAX_AGE = 30 * 60_000, MAX_LINKS = 100;
type Origin = Attribution & { id: string; scope: string; owner: string | null; content: string; version?: string; at: number; sourceSent?: boolean };
export interface VisitOrigin { clickId: string; visitId: string; attribution: Attribution; version?: string; clicked?: boolean }
const local = new Map<string, Origin>();
const emitters = new Map<string, (id: string) => void>();
// Mounted anchors can renew even after another list evicts their persisted
// handoff. Their owner releases these small templates when the list unmounts.
const templates = new Map<string, { origin: Origin; emit?: (id: string) => void }>();
const visits = new WeakMap<ContentKitClient, { content: string; owner: string | null; path: string; origin: VisitOrigin; holders: number }>();

/** Only the random navigation ID enters the URL; queries stay in exposures. */
export function originLink(client: ContentKitClient, content: string, version: string | undefined, attribution: Attribution, href: string, emit?: (id: string) => void): string {
  const owner = client.preferences.identity();
  if (owner === undefined || typeof location === "undefined") return href;
  try {
    const url = new URL(href, location.href);
    if (url.origin !== location.origin) return href;
    const origin: Origin = { ...attribution, id: eventId(), content, version, scope: client.url("discovery"), owner, at: Date.now() };
    local.set(origin.id, origin);
    templates.set(origin.id, { origin, emit });
    if (emit) emitters.set(origin.id, emit);
    while (local.size > MAX_LINKS) { const oldest = local.keys().next().value!; local.delete(oldest); emitters.delete(oldest); }
    try {
      const keys: { key: string; at: number }[] = [];
      for (let i = 0; i < localStorage.length; i++) {
        const key = localStorage.key(i)!;
        if (!key.startsWith(PREFIX)) continue;
        const at = Number(JSON.parse(localStorage.getItem(key) ?? "null")?.at);
        if (!at || Date.now() - at > MAX_AGE) { localStorage.removeItem(key); i--; }
        else keys.push({ key, at });
      }
      keys.sort((a, b) => a.at - b.at);
      for (const entry of keys.slice(0, Math.max(0, keys.length - MAX_LINKS + 1))) localStorage.removeItem(entry.key);
      localStorage.setItem(PREFIX + origin.id, JSON.stringify(origin));
    } catch { /* Same-tab attribution can use memory when browser storage is off. */ }
    url.searchParams.set(PARAM, origin.id);
    return url.pathname + url.search + url.hash;
  } catch { return href; }
}

export function originID(href: string): string | undefined {
  try { return new URL(href, location.href).searchParams.get(PARAM) ?? undefined; } catch { return undefined; }
}

/** Give each activation its own click, including native modifier/middle clicks. */
export function renewOriginLink(href: string, recordClick = false): string {
  try {
    const url = new URL(href, location.href), id = url.searchParams.get(PARAM);
    if (!id) return href;
    const template = templates.get(id);
    const raw = template?.origin ?? local.get(id) ?? JSON.parse(localStorage.getItem(PREFIX + id) ?? "null") as Origin | undefined;
    if (!raw) return href;
    const next = { ...raw, id: eventId(), at: Date.now() };
    const emit = template?.emit ?? emitters.get(id);
    next.sourceSent = recordClick && !!emit;
    if (emit) { emitters.set(next.id, emit); if (recordClick) emit(next.id); }
    local.set(next.id, next);
    while (local.size > MAX_LINKS) { const oldest = local.keys().next().value!; local.delete(oldest); emitters.delete(oldest); }
    try { localStorage.setItem(PREFIX + next.id, JSON.stringify(next)); } catch { /* Memory covers ordinary navigation. */ }
    url.searchParams.set(PARAM, next.id);
    return url.pathname + url.search + url.hash;
  } catch { return href; }
}

export function forgetOriginLink(href: string): void {
  const id = originID(href);
  if (id) templates.delete(id);
}

/** Read an exact link handoff; never borrow the last click for an item. */
export function visitOrigin(client: ContentKitClient, content: string): VisitOrigin | undefined {
  const owner = client.preferences.identity();
  if (owner === undefined || typeof location === "undefined") return;
  const url = new URL(location.href), id = url.searchParams.get(PARAM);
  const previous = visits.get(client);
  // Version selection keeps the same visit; a different content page ends it.
  if (previous && (previous.content !== content || previous.owner !== owner)) visits.delete(client);
  if (!id) return previous?.content === content && previous.owner === owner && (previous.holders > 0 || previous.path === url.pathname) ? previous.origin : undefined;
  let raw = local.get(id);
  try { raw ??= JSON.parse(localStorage.getItem(PREFIX + id) ?? "null") as Origin | undefined; } catch { /* Missing attribution never blocks reading. */ }
  if (!raw || raw.id !== id || raw.content !== content || raw.owner !== owner || raw.scope !== client.url("discovery") || Date.now() - raw.at > MAX_AGE) return;
  const origin: VisitOrigin = previous?.origin.clickId === id ? previous.origin : {
    clickId: id, visitId: eventId(), version: raw.version,
    clicked: raw.sourceSent,
    attribution: { render_id: raw.render_id, surface: raw.surface, position: raw.position, language: raw.language },
  };
  visits.set(client, { content, owner, path: url.pathname, origin, holders: previous?.origin === origin ? previous.holders : 0 });
  return origin;
}

export function cleanOriginURL(client: ContentKitClient, origin: VisitOrigin | undefined): void {
  if (!origin || typeof location === "undefined") return;
  const url = new URL(location.href);
  if (url.searchParams.get(PARAM) !== origin.clickId) return;
  url.searchParams.delete(PARAM);
  // Keep the router's state and reader anchor. This is URL cleanup, not navigation.
  history.replaceState(history.state, "", url.pathname + url.search + url.hash);
  const kept = visits.get(client);
  if (kept) kept.path = url.pathname;
}

export function holdOriginVisit(client: ContentKitClient, origin: VisitOrigin | undefined): () => void {
  const visit = visits.get(client);
  if (!origin || visit?.origin !== origin) return () => {};
  visit.holders++;
  return () => {
    visit.holders--;
    // Effect replays and edition switches acquire their new holder first.
    queueMicrotask(() => { if (visit.holders === 0 && visits.get(client) === visit) visits.delete(client); });
  };
}
