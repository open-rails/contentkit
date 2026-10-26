import assert from "node:assert/strict";
import test from "node:test";
import { saveContentAccess, contentAccess, selectedEdition } from "../src/client/edition-access.ts";
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
    async update() { throw new Error("must not edit parent"); },
    async updateVersion(id: string, version: string, patch: VersionInput): Promise<Version> {
      calls.push({ id, version, patch });
      return { ...second, ...patch } as Version;
    },
  };
  const saved = await saveContentAccess(target, content, { level: "tier", tiers: ["gold"] });
  assert.deepEqual(calls, [{ id: "gallery", version: "german", patch: { access: { level: "tier", tiers: ["gold"] } } }]);
  assert.equal(contentAccess(saved)?.level, "tier");
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
  await saveContentAccess({ async update() { throw new Error("must not edit parent"); }, async updateVersion(_id, version, patch) { edited = version; return { ...hidden, ...patch } as Version; } }, content, { level: "public" });
  assert.equal(edited, hidden.id);
  assert.equal(content.access.level, "ppv");
});

test("reader edition selection is explicit and an invalid selection cannot edit a sibling", async () => {
  const first = edition("first", { level: "public" });
  const second = edition("second", { level: "ppv" });
  const content = { id: "gallery", selected_version_id: "first", versions: [first, second] } as Content;
  let writes = 0;
  const target = { async update() { throw new Error("must not edit parent"); }, async updateVersion(_id: string, version: string, patch: VersionInput): Promise<Version> {
    writes++;
    assert.equal(version, "second");
    return { ...second, ...patch } as Version;
  } };
  await saveContentAccess(target, content, { level: "public" }, "second");
  assert.equal(writes, 1);
  content.selected_version_id = "removed";
  assert.equal(selectedEdition(content), undefined);
  await assert.rejects(() => saveContentAccess(target, content, { level: "public" }), /select an existing edition/);
  assert.equal(writes, 1);
});


test("channel content edits one parent policy without writing individual editions", async () => {
  const first = edition("english", { level: "public" });
  const second = edition("chinese", { level: "public" });
  const content = { id: "post", channel_id: "studio", revision: 9, access: { level: "public" }, selected_version_id: first.id, versions: [first, second] } as Content;
  const calls: unknown[] = [];
  const target = {
    async update(id: string, patch: { access?: Rule; revision?: number }): Promise<Content> { calls.push({ id, patch }); return { ...content, ...patch }; },
    async updateVersion(): Promise<Version> { throw new Error("channel policy must not be version-specific"); },
  };
  const access: Rule = { level: "group", group_id: "studio" };
  const saved = await saveContentAccess(target, content, access, second.id);
  assert.deepEqual(calls, [{ id: "post", patch: { access, revision: 9 } }]);
  assert.deepEqual(contentAccess(saved, first.id), access);
  assert.deepEqual(contentAccess(saved, second.id), access);
  assert.deepEqual(saved.versions, [first, second]);
});
