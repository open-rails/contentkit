import assert from "node:assert/strict";
import test from "node:test";
import { saveEditionAccess, selectedEdition } from "../src/client/edition-access.ts";
import type { Content, Rule, Version, VersionInput } from "../src/client/generated/wire.ts";

function edition(id: string, access: Rule): Version {
  return { id, state: "published", access } as Version;
}

test("default content-scope configuration still edits only the selected edition", async () => {
  const first = edition("english", { level: "public" });
  const second = edition("german", { level: "ppv" });
  const content = { id: "gallery", access: { level: "public" }, selected_version_id: second.id, versions: [first, second] } as Content;
  assert.equal(selectedEdition(content)?.access?.level, "ppv");
  const calls: unknown[] = [];
  const target = {
    async updateVersion(id: string, version: string, patch: VersionInput): Promise<Version> {
      calls.push({ id, version, patch });
      return { ...second, ...patch } as Version;
    },
  };
  const saved = await saveEditionAccess(target, content, { level: "tier", tiers: ["gold"] });
  assert.deepEqual(calls, [{ id: "gallery", version: "german", patch: { access: { level: "tier", tiers: ["gold"] } } }]);
  assert.equal(saved.access?.level, "tier");
  assert.equal(content.access.level, "public");
  assert.equal(first.access?.level, "public");
});

test("a hidden edition keeps its own rule after the parent creation default changes", async () => {
  const hidden = edition("article-edition", { level: "tier", tiers: ["gold"] });
  const content = { id: "article", access: { level: "public" }, versions: [hidden] } as Content;
  assert.equal(selectedEdition(content), hidden);
  content.access = { level: "ppv" };
  assert.deepEqual(selectedEdition(content)?.access, { level: "tier", tiers: ["gold"] });
  let edited = "";
  await saveEditionAccess({ async updateVersion(_id, version, patch) { edited = version; return { ...hidden, ...patch } as Version; } }, content, { level: "public" });
  assert.equal(edited, hidden.id);
  assert.equal(content.access.level, "ppv");
});

test("reader edition selection is explicit and an invalid selection cannot edit a sibling", async () => {
  const first = edition("first", { level: "public" });
  const second = edition("second", { level: "ppv" });
  const content = { id: "gallery", selected_version_id: "first", versions: [first, second] } as Content;
  let writes = 0;
  const target = { async updateVersion(_id: string, version: string, patch: VersionInput): Promise<Version> {
    writes++;
    assert.equal(version, "second");
    return { ...second, ...patch } as Version;
  } };
  await saveEditionAccess(target, content, { level: "public" }, "second");
  assert.equal(writes, 1);
  content.selected_version_id = "removed";
  assert.equal(selectedEdition(content), undefined);
  assert.throws(() => saveEditionAccess(target, content, { level: "public" }), /select an existing edition/);
  assert.equal(writes, 1);
});
