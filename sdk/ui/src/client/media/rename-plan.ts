import { ContentKitError } from "../errors.js";
import type { Op } from "../generated/wire.js";

/** Vacate rename targets first; cycles use a temporary manifest name, not a blob copy. */
export function planFileRenames(paths: readonly string[], targets: Readonly<Record<string, string>>): Op[] {
  const fold = (path: string) => path.toLowerCase();
  const existing = new Set(paths);
  const final = new Set<string>();
  for (const path of paths) {
    const target = targets[path] ?? path;
    const key = fold(target);
    if (final.has(key)) throw new ContentKitError("conflict", `Filename ${target} is already used.`);
    final.add(key);
  }
  const pending = new Map<string, string>();
  for (const [path, target] of Object.entries(targets)) {
    if (!existing.has(path)) throw new ContentKitError("conflict", `File ${path} changed on the server.`);
    if (path !== target) pending.set(path, target);
  }
  const occupied = new Set(paths.map(fold));
  const ops: Op[] = [];
  let sequence = 0;
  while (pending.size) {
    const ready = [...pending].find(([path, target]) => fold(path) === fold(target) || !occupied.has(fold(target)));
    if (ready) {
      const [path, target] = ready;
      ops.push({ op: "rename", path, to: target });
      occupied.delete(fold(path));
      occupied.add(fold(target));
      pending.delete(path);
      continue;
    }
    const [path, target] = pending.entries().next().value!;
    const slash = path.lastIndexOf("/");
    const extension = /\.[^./]+$/.exec(path)?.[0] ?? "";
    let temporary: string;
    do temporary = `${path.slice(0, slash + 1)}__ck_rename_${++sequence}${extension}`;
    while (occupied.has(fold(temporary)) || final.has(fold(temporary)));
    ops.push({ op: "rename", path, to: temporary });
    occupied.delete(fold(path));
    occupied.add(fold(temporary));
    pending.delete(path);
    pending.set(temporary, target);
  }
  return ops;
}
