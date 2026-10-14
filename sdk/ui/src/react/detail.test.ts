import { expect, it } from "vitest";
import type { Content, Version } from "../client/generated/wire.js";
import { defaultVersion, editionOf } from "./detail.js";

const v = (id: string, position: number, languages: string[], live = true): Version => ({
  id,
  code: id.toUpperCase().padEnd(4, "X"),
  position,
  languages,
  traits: [],
  live,
  has_audio: false,
  attributes: {},
  segments: [],
  decision: { allowed: true },
});
const content = (versions: Version[]) => ({ versions }) as unknown as Content;

it("opens the first live version by position serving the language, else the first live one", () => {
  const [a, b, z] = [v("a", 0, ["en"]), v("b", 1, ["en"]), v("z", 2, ["zh"])];
  const c = content([z, b, a]);
  expect(defaultVersion(c, "en")?.id).toBe("a");
  expect(defaultVersion(c, "zh")?.id).toBe("z");
  expect(defaultVersion(c, "ja")?.id).toBe("a");
  expect(defaultVersion(c)?.id).toBe("a");
  // Staff reorder the versions: B first.
  const reordered = content([{ ...a, position: 1 }, { ...b, position: 0 }, z]);
  expect(defaultVersion(reordered, "en")?.id).toBe("b");
  // A version that is not live is never the default.
  expect(defaultVersion(content([{ ...a, live: false }, b, z]), "en")?.id).toBe("b");
  expect(defaultVersion(content([{ ...z, live: false }, a]), "zh")?.id).toBe("a");
  expect(defaultVersion(content([{ ...a, live: false }]), "en")).toBeNull();
});

it("editionOf applies the rule to the versions given, live or not", () => {
  expect(editionOf([v("b", 1, ["en"], false), v("a", 0, ["ja"], false)], "en")?.id).toBe("b");
  expect(editionOf([], "en")).toBeNull();
});
