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
