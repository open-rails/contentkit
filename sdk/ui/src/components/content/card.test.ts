import { expect, it } from "vitest";
import { createTranslator, resolveMessages } from "../../i18n/messages.js";
import { lockLabel } from "./card.js";

it("names why a card is locked: a tier, a group (free to join), a sale, or nothing it opens", () => {
  const m = createTranslator(resolveMessages(), undefined, "en");
  expect(lockLabel(m, { allowed: true })).toBeNull();
  expect(lockLabel(m, { allowed: false, sell: ["premium", "t:content:x"] }, ["premium"])).toBe("Premium");
  expect(lockLabel(m, { allowed: false, sell: ["premium"] }, ["premium"], { premium: "Gold" })).toBe("Gold");
  expect(lockLabel(m, { allowed: false, sell: ["t:group:g"] })).toBe("Members");
  const group = { key: "t:group:g", target: { kind: "group" as const, id: "g" } };
  expect(lockLabel(m, { allowed: false, sell: ["t:group:g"], offers: [{ ...group, terms: { amount: "0.00", currency: "USD" } }] })).toBe("Free to join");
  expect(lockLabel(m, { allowed: false, sell: ["t:group:g"], offers: [{ ...group, terms: { amount: "4.99", currency: "USD" } }] })).toBe("$4.99");
  expect(lockLabel(m, { allowed: false, sell: ["t:content:x"] })).toBe("For sale");
  expect(lockLabel(m, { allowed: false, unknown: true })).toBe("Unavailable right now");
  expect(lockLabel(m, { allowed: false })).toBe("Locked");
});
