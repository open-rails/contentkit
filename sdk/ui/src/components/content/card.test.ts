import { expect, it } from "vitest";
import { createTranslator, resolveMessages } from "../../i18n/messages.js";
import { lockLabel } from "./card.js";

it("names why a card is locked: a tier, a group, a sale, or nothing it opens", () => {
  const m = createTranslator(resolveMessages(), undefined, "en");
  expect(lockLabel(m, { allowed: true })).toBeNull();
  expect(lockLabel(m, { allowed: false, sell: ["premium", "t:content:x"] }, ["premium"])).toBe("Premium");
  expect(lockLabel(m, { allowed: false, sell: ["premium"] }, ["premium"], { premium: "Gold" })).toBe("Gold");
  expect(lockLabel(m, { allowed: false, sell: ["t:group:g"] })).toBe("Members");
  expect(lockLabel(m, { allowed: false, sell: ["t:content:x"] })).toBe("For sale");
  expect(lockLabel(m, { allowed: false, unknown: true })).toBe("Unavailable right now");
  expect(lockLabel(m, { allowed: false })).toBe("Locked");
});
