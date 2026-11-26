import { uuidv4 } from "./uuid.js";

const KEY = "contentkit.visitor.v1";
const USED = "contentkit.visitor.used.v1";

let unstored: string | undefined;
let unstoredUsed = false;

function read(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function write(key: string, value: string | null): boolean {
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
    return true;
  } catch {
    return false;
  }
}

/**
 * The browser's anonymous id: a random UUIDv4 kept in local storage, shared
 * by its tabs. Signed out, every request sends it as ContentKit-Anonymous-Id;
 * the server keeps its reading state, hidden items and recent searches under
 * a hash of it. It is a bearer identity, never an account.
 */
export function anonymousId(): string {
  const kept = read(KEY);
  if (kept) return kept;
  const made = uuidv4();
  if (!write(KEY, made)) return (unstored ??= made);
  return made;
}

/** Replaces the anonymous id: at sign-in once linked, and at sign-out. */
export function renewAnonymousId(): string {
  const made = uuidv4();
  if (!write(KEY, made)) unstored = made;
  write(USED, null);
  unstoredUsed = false;
  return made;
}

/** Notes that a request carried the anonymous id, so signing in links it. */
export function markAnonymousUsed(id: string): void {
  if (read(USED) === id) return;
  if (!write(USED, id)) unstoredUsed = true;
}

/** Whether the anonymous id was sent since it was made. */
export function anonymousUsed(id: string): boolean {
  return read(USED) === id || (unstoredUsed && unstored === id);
}
