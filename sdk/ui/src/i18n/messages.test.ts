import { expect, it } from "vitest";
import { ContentKitError, ERROR_CODES } from "../client/errors.js";
import { de } from "../locales/de.js";
import { en } from "../locales/en.js";
import { es } from "../locales/es.js";
import { ja } from "../locales/ja.js";
import { ko } from "../locales/ko.js";
import { zh } from "../locales/zh.js";
import { createTranslator, resolveMessages } from "./messages.js";

const bundles = { en, de, es, ja, ko, zh } as Record<string, { errors?: Record<string, string> }>;

it("every error code has a message in every bundle", () => {
  for (const [lang, bundle] of Object.entries(bundles)) {
    const missing = ERROR_CODES.filter((code) => !bundle.errors?.[code]);
    expect(missing, lang).toEqual([]);
  }
});

it("words video limits with their numbers, else the code's line", () => {
  const { error } = createTranslator(resolveMessages(de));
  expect(error(new ContentKitError("video_too_long", "x", { status: 422, details: { seconds: 900, max_seconds: 600 } }))).toBe(
    "Diese Datei läuft 900 s; hier sind höchstens 600 s erlaubt.",
  );
  expect(error(new ContentKitError("video_too_long", "x", { status: 422 }))).toBe(de.errors!.video_too_long);
  expect(error(new ContentKitError("comment_banned", "you can't comment here", { status: 403 }))).toBe(de.errors!.comment_banned);
});

it("every content string is in every bundle with the same placeholders", async () => {
  const { social } = await import("../locales/social/en.js");
  const leaves = (tree: object, prefix = ""): [string, string][] =>
    Object.entries(tree).flatMap(([k, v]) => (typeof v === "string" ? [[prefix + k, v] as [string, string]] : leaves(v as object, `${prefix}${k}.`)));
  const vars = (s: string) => [...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();
  const english = leaves(social);
  for (const [lang, bundle] of Object.entries(bundles)) {
    const own = new Map(leaves(bundle as object));
    for (const [key, text] of english) {
      expect(own.get(key), `${lang} ${key}`).toBeTruthy();
      expect(vars(own.get(key)!), `${lang} ${key}`).toEqual(vars(text));
    }
  }
});

it("plural picks the language's form", () => {
  const m = createTranslator(resolveMessages(), undefined, "en");
  expect(m.plural("comments.count", 1)).toBe("1 comment");
  expect(m.plural("comments.count", 2)).toBe("2 comments");
  expect(createTranslator(resolveMessages(ja), undefined, "ja").plural("poll.votes", 1)).toBe("1 票");
});
