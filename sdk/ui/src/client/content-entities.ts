import type { EntitySummary } from "./generated/wire.js";

/** Shared entities plus this edition's own credits, with one entry per entity. */
export function entitiesForVersion(
  content: { entities: readonly EntitySummary[] },
  version?: { entities: readonly EntitySummary[] } | null,
): EntitySummary[] {
  const entities = new Map(content.entities.map((entity) => [entity.id, entity]));
  for (const entity of version?.entities ?? []) entities.set(entity.id, entity);
  return [...entities.values()];
}
