import type { AttributeSpec } from "./kinds.js";

/** How ContentKit compares names (contentkit_keyword_normalize): accents dropped, lowercase, single spaces. */
export function fold(s: string): string {
  return s.normalize("NFKD").replace(/[̀-ͯ]/g, "").normalize("NFC").toLowerCase().replace(/\s+/g, " ").trim();
}

/** The letter bucket a name lists under: its first letter a-z, else "#". */
export function letterOf(name: string): string {
  const c = fold(name).charAt(0);
  return c >= "a" && c <= "z" ? c : "#";
}

/** The letter buckets of a directory, in order. */
export const LETTERS: readonly string[] = [..."abcdefghijklmnopqrstuvwxyz", "#"];

/** A slug from a name: lowercase words joined by dashes. */
export function slugify(name: string): string {
  return fold(name)
    .replace(/[\s/]+/g, "-")
    .replace(/[^\p{L}\p{N}-]+/gu, "")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, 200);
}

/** A datetime-local input's value for an instant, in local time ("" for none). */
export function toLocalInput(iso: string | null | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** The instant a datetime-local input names, as RFC 3339; undefined when empty or invalid. */
export function fromLocalInput(value: string): string | undefined {
  if (!value) return undefined;
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

/** A form field's value for an attribute: text for the inputs, a boolean for a switch. */
export function attributeInput(spec: AttributeSpec, value: unknown): string | boolean {
  if (spec.type === "bool") return value === true;
  if (value === undefined || value === null) return "";
  return spec.type === "time" ? toLocalInput(String(value)) : String(value);
}

/**
 * An attribute's value from its form field; undefined drops it. A value that
 * is not of the attribute's type throws a RangeError.
 */
export function parseAttribute(spec: AttributeSpec, input: string | boolean): unknown {
  if (spec.type === "bool") return input === true;
  const s = String(input).trim();
  if (!s) return undefined;
  switch (spec.type) {
    case "int": {
      const n = Number(s);
      if (!Number.isSafeInteger(n)) throw new RangeError(`${s} is not a whole number`);
      return n;
    }
    case "enum":
      if (!spec.values?.includes(s)) throw new RangeError(`${s} is not one of ${spec.values?.join(", ")}`);
      return s;
    case "time": {
      const t = fromLocalInput(s);
      if (!t) throw new RangeError(`${s} is not a time`);
      return t;
    }
    default:
      return s;
  }
}

/** Every declared attribute's value from the form, undeclared ones dropped. */
export function parseAttributes(declared: Readonly<Record<string, AttributeSpec>>, form: Readonly<Record<string, string | boolean>>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [name, spec] of Object.entries(declared)) {
    const v = name in form ? parseAttribute(spec, form[name]!) : undefined;
    if (v !== undefined) out[name] = v;
  }
  return out;
}

/** "voice_actor" → "Voice actor": an identifier's fallback label. */
export function humanize(id: string): string {
  const s = id.replace(/[_-]+/g, " ").trim();
  return s.charAt(0).toUpperCase() + s.slice(1);
}
