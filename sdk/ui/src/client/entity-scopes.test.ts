import { expect, it } from "vitest";
import { allowsEntityScope } from "./entity-scopes.js";

it("offers explicitly shared entity kinds at both content and version scope", () => {
  const tag = { membership_scopes: ["content", "version"] as ("content" | "version")[] };
  expect(allowsEntityScope(tag, "content")).toBe(true);
  expect(allowsEntityScope(tag, "version")).toBe(true);
  expect(allowsEntityScope({ membership_scopes: ["version"] }, "content")).toBe(false);
});

it("keeps the old default for configurations without explicit scopes", () => {
  expect(allowsEntityScope({}, "content")).toBe(true);
  expect(allowsEntityScope({}, "version")).toBe(false);
  expect(allowsEntityScope({ version_level: true }, "version")).toBe(true);
  expect(allowsEntityScope({ version_level: true }, "content")).toBe(false);
});
