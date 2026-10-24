import { expect, it } from "vitest";
import { createTranslator, resolveMessages } from "../../i18n/messages.js";
import { lockLabel } from "./card.js";

it("names required access without inventing prices or join terms", () => {
  const m = createTranslator(resolveMessages(), undefined, "en");
  expect(lockLabel(m, { allowed: true, level: "public" })).toBeNull();
  expect(lockLabel(m, { allowed: false, level: "tier", sell: ["premium", "t:content:x"] }, ["premium"])).toBe("Premium");
  expect(lockLabel(m, { allowed: false, level: "tier", sell: ["premium"] }, ["premium"], { premium: "Gold" })).toBe("Gold");
  expect(lockLabel(m, { allowed: false, level: "tier", sell: ["t:group:g"] })).toBe("Members");
  expect(lockLabel(m, { allowed: false, level: "ppv", sell: ["t:content:x"] })).toBe("Locked");
  expect(lockLabel(m, { allowed: false, level: "tier", unknown: true })).toBe("Unavailable right now");
  expect(lockLabel(m, { allowed: false, level: "tier" })).toBe("Locked");
});
