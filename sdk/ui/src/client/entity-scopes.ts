import type { EntityKind } from "./generated/wire.js";

/** Explicit entity scopes take precedence; omitted scopes retain the legacy default. */
export function allowsEntityScope(spec: EntityKind | undefined, scope: "content" | "version"): boolean {
  if (spec?.membership_scopes?.length) return spec.membership_scopes.includes(scope);
  return scope === "version" ? !!spec?.version_level : !spec?.version_level;
}
