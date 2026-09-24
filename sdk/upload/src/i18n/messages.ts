import { en } from "../locales/en.js";

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
      const code = typeof error === "string" ? error : (error as { code?: unknown } | null)?.code;
      const retryAfter = (error as { retryAfter?: number } | null)?.retryAfter;
      const vars = { seconds: retryAfter ?? 60 };
      return (typeof code === "string" && translate(`errors.${code}`, vars)) || messages.errors.generic;
    },
  };
}
