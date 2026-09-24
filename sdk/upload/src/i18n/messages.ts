import { en } from "../locales/en.js";
import type { ErrorDetails } from "../wire.gen.js";

type Widen<T> = { [K in keyof T]: T[K] extends string ? string : Widen<T[K]> };
type DeepPartial<T> = { [K in keyof T]?: T[K] extends string ? string : DeepPartial<T[K]> };
type Paths<T, P extends string = ""> = {
  [K in keyof T & string]: T[K] extends string ? `${P}${K}` : Paths<T[K], `${P}${K}.`>;
}[keyof T & string];

/** The complete message tree; English is the reference shape. */
export type UploadUiMessages = Widen<typeof en>;
/** A locale bundle or host override: any subset of the tree. */
export type UploadUiMessageBundle = DeepPartial<UploadUiMessages>;
/** Dotted key of any message, e.g. `crop.title`. */
export type MessageKey = Paths<UploadUiMessages>;
export type MessageVars = Record<string, string | number>;
/** Host translation hook; return nothing (or the key) to use ours. */
export type UploadUiTranslate = (key: string, vars?: MessageVars) => string | null | undefined;

export const defaultMessages: UploadUiMessages = en;

export function defineMessages(bundle: UploadUiMessageBundle): UploadUiMessageBundle {
  return bundle;
}

type Tree = { [key: string]: string | Tree };

function mergeInto(target: Tree, source: Tree): Tree {
  const out: Tree = { ...target };
  for (const [key, value] of Object.entries(source)) {
    if (value === undefined || value === null) continue;
    const current = out[key];
    if (typeof value === "string") {
      if (value !== "") out[key] = value;
    } else out[key] = mergeInto(typeof current === "object" ? current : {}, value);
  }
  return out;
}

/** Layers bundles over English; later bundles win, missing keys fall back. */
export function resolveMessages(bundles?: UploadUiMessageBundle | readonly UploadUiMessageBundle[]): UploadUiMessages {
  const list = bundles ? (Array.isArray(bundles) ? bundles : [bundles]) : [];
  let tree = en as unknown as Tree;
  for (const b of list) tree = mergeInto(tree, b as Tree);
  return tree as unknown as UploadUiMessages;
}

export function interpolate(template: string, vars?: MessageVars): string {
  if (!vars) return template;
  return template.replace(/\{(\w+)\}/g, (m, name: string) => (name in vars ? String(vars[name]) : m));
}

function lookup(messages: UploadUiMessages, key: string): string | undefined {
  let node: unknown = messages;
  for (const part of key.split(".")) {
    if (!node || typeof node !== "object") return undefined;
    node = (node as Tree)[part];
  }
  return typeof node === "string" ? node : undefined;
}

export interface Translator {
  messages: UploadUiMessages;
  t(key: MessageKey, vars?: MessageVars): string;
  /** Message for an UploadError (or its code). */
  error(error: unknown): string;
}

export function createTranslator(messages: UploadUiMessages, hostT?: UploadUiTranslate): Translator {
  const translate = (key: string, vars?: MessageVars) => {
    const hosted = hostT?.(key, vars);
    if (hosted && hosted !== key) return hosted;
    const own = lookup(messages, key);
    return own === undefined ? undefined : interpolate(own, vars);
  };
  return {
    messages,
    t: (key, vars) => translate(key, vars) ?? key,
    error(error) {
      if (typeof error === "string") return translate(`errors.${error}`, { seconds: 60 }) ?? messages.errors.generic;
      const e = (error ?? {}) as { code?: unknown; message?: unknown; status?: number; retryAfter?: number; details?: ErrorDetails; refusal?: boolean };
      const code = typeof e.code === "string" ? e.code : undefined;
      const server = typeof e.message === "string" && e.message ? sentence(e.message) : undefined;
      const d = e.details ?? {};
      const vars: MessageVars = {
        seconds: e.retryAfter ?? 60,
        width: d.width ?? "",
        height: d.height ?? "",
        min: d.min_width ?? "",
        megapixels: d.max_pixels ? Math.round(d.max_pixels / 1e6) : "",
        format: formatName(d.type),
        allowed: (d.allowed ?? []).map(formatName).join(", "),
        size: d.size ? megabytes(d.size) : "",
        max: d.max_bytes ? megabytes(d.max_bytes) : "",
      };
      // A refusal the server words itself states its rule; show it rather than a vaguer line.
      if (code === "invalid_request" && server) return server;
      if (code === "type_not_allowed" && d.allowed?.length) return `${translate("errors.type_not_allowed", vars)} ${translate("errors.allowedTypes", vars)}`;
      if (code === "too_large" && d.max_bytes) return translate("errors.tooLargeBy", vars) ?? messages.errors.too_large;
      const own = code && translate(`errors.${code}`, vars);
      if (own) return own;
      // An unknown refusal (4xx) says what is wrong; faults get the generic line.
      if (server && (e.refusal ?? (e.status !== undefined && e.status >= 400 && e.status < 500))) return server;
      return messages.errors.generic;
    },
  };
}

/** "image/avif" → "AVIF". */
export function formatName(type?: string): string {
  if (!type) return "";
  const sub = type.split("/")[1] ?? type;
  return (sub.split("+")[0] ?? sub).replace(/^x-/, "").toUpperCase().replace(/^JPG$/, "JPEG").replace(/^WEBP$/, "WebP");
}

function megabytes(n: number): string {
  const mb = n / (1024 * 1024);
  return mb >= 1024 ? `${+(mb / 1024).toFixed(1)} GB` : `${+mb.toFixed(mb < 10 ? 1 : 0)} MB`;
}

function sentence(s: string): string {
  const t = s.replace(/^media(\/\w+)?: /, "");
  return t.charAt(0).toUpperCase() + t.slice(1) + (/[.!?]$/.test(t) ? "" : ".");
}
