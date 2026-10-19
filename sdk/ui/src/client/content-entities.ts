import type { GroupSummary } from "./generated/wire.js";

/** Shared entities plus this edition's own credits, with one entry per entity. */
export function entitiesForVersion(
  content: { entities: readonly GroupSummary[] },
  version?: { entities: readonly GroupSummary[] } | null,
): GroupSummary[] {
  const entities = new Map(content.entities.map((entity) => [entity.id, entity]));
  for (const entity of version?.entities ?? []) entities.set(entity.id, entity);
  return [...entities.values()];
}
