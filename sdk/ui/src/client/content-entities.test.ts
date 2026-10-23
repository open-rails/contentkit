import { expect, it } from "vitest";
import { entitiesForVersion } from "./content-entities.js";
import type { EntitySummary } from "./generated/wire.js";

const entity = (id: string, kind = "translator"): EntitySummary => ({ id, kind, slug: id, name: id });

it("combines shared entities with only the selected edition's credits", () => {
  const shared = entity("artist", "artist"), a = entity("translator-a"), b = entity("translator-b");
  const content = { entities: [shared] };
  expect(entitiesForVersion(content, { entities: [a] })).toEqual([shared, a]);
  expect(entitiesForVersion(content, { entities: [b] })).toEqual([shared, b]);
  expect(content.entities).toEqual([shared]);
});

it("shows one entity when it is attached to both the parent and edition", () => {
  const shared = entity("shared");
  const localized = { ...shared, name: "Selected edition display" };
  expect(entitiesForVersion({ entities: [shared] }, { entities: [localized] })).toEqual([localized]);
});

it("retains legacy parent credits without a selected edition", () => {
  const shared = entity("legacy-parent-translator");
  expect(entitiesForVersion({ entities: [shared] })).toEqual([shared]);
  expect(entitiesForVersion({ entities: [shared] }, null)).toEqual([shared]);
});

it("shows shared tags plus the selected version's tags, never its sibling's", () => {
  const shared = entity("shared", "tag"), colored = entity("colored", "tag"), uncensored = entity("uncensored", "tag");
  expect(entitiesForVersion({ entities: [shared] }, { entities: [colored, shared] })).toEqual([shared, colored]);
  expect(entitiesForVersion({ entities: [shared] }, { entities: [uncensored] })).toEqual([shared, uncensored]);
});
