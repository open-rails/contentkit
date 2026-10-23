import { expect, test } from "@playwright/test";
import type { Entity } from "@openrails/contentkit-ui/client";
import { contentKit, seedContent } from "../support/seed.ts";
import { admin, harness, inheritsHostTheme, page as demo, screenshots as dir } from "./pages.ts";

// Entities in demo/groups.tsx (directory, header, a content's groups) and
// EntityManager in demo/admin.tsx, against the real ContentKit.
test.describe.configure({ timeout: 120_000 });
test.use({ actionTimeout: 15_000 });
const h = harness();
const run = () => Math.random().toString(36).slice(2, 8);
const name = (language: string, n: string, kind: "name" | "alias" = "name") => ({ language, kind, name: n });

/** Waits until the typeahead finds a group just made: search documents follow writes within a moment. */
const searchable = (ck: ReturnType<typeof contentKit>, kind: string, text: string) =>
  expect.poll(async () => (await ck.discovery.typeahead({ q: text, entity_kinds: [kind] })).entities.length, { timeout: 30_000 }).toBeGreaterThan(0);

async function staffClient() {
  const staff = await h.user({ role: "staff" });
  return { staff, ck: contentKit(h, staff) };
}

for (const theme of ["light", "dark"] as const) {
  test(`group directory: letter buckets in the URL, counts, and a group's page (${theme})`, async ({ page }, info) => {
    const { ck } = await staffClient();
    const r = run();
    const [parent] = await ck.entities.create([{ kind: "series", slug: `p-${r}`, names: [name("en", `Parent ${r}`)] }]);
    const children = await ck.entities.create(
      ["Alpha", "Beta", "3x3"].map((n) => ({ kind: "series", slug: `${n.toLowerCase()}-${r}`, parent_id: parent!.id, names: [name("en", `${n} ${r}`)] })),
    );
    const beta = children[1]!;
    for (let i = 0; i < 2; i++) {
      const c = await seedContent(ck, { kind: "comic", titles: { en: `Member ${r} ${i}` }, versions: [{ languages: ["en"] }] });
      await ck.entities.addMembers(beta.id, [{ content_id: c.id }]);
    }
    await ck.entities.update(beta.id, { text: [{ language: "en", description: `Beta's story ${r}.` }] });
    await h.recount();

    await page.goto(demo("entities.html", null, { kind: "series", parent: parent!.id, theme }));
    const dir_ = page.locator("[data-ckui=entity-directory]");
    const groups = dir_.locator("[data-ckui=group]");
    await expect(groups).toHaveText([`3x3 ${r}0`, `Alpha ${r}0`, `Beta ${r}2`], { timeout: 30_000 });
    await page.screenshot({ path: `${dir}/group-directory-${theme}-${info.project.name}.png`, fullPage: true });

    await dir_.getByRole("button", { name: "B", exact: true }).click();
    await expect(page).toHaveURL(/letter=b/);
    await expect(groups).toHaveText([`Beta ${r}2`]);
    await expect(dir_.getByRole("button", { name: "B", exact: true })).toHaveAttribute("aria-pressed", "true");
    await dir_.getByRole("button", { name: "Other characters" }).click();
    await expect(groups).toHaveText([`3x3 ${r}0`]);
    await page.goBack();
    await expect(groups).toHaveText([`Beta ${r}2`]);

    await groups.getByRole("link", { name: `Beta ${r}` }).click();
    const header = page.locator("[data-ckui=entity-header]");
    await expect(header.getByRole("heading", { name: `Beta ${r}` })).toBeVisible({ timeout: 30_000 });
    await expect(header).toContainText("2 items");
    await expect(header).toContainText(`Beta's story ${r}.`);
    await expect(header.locator("[data-ckui=entity-parent]")).toHaveText(`Parent ${r}`);
    await page.screenshot({ path: `${dir}/group-header-${theme}-${info.project.name}.png`, fullPage: true });
  });
}

test("group header: names in other languages, aliases and typed attributes", async ({ page }) => {
  const { ck } = await staffClient();
  const r = run();
  const [artist] = await ck.entities.create([
    {
      kind: "artist",
      slug: `circle-${r}`,
      names: [name("en", `Circle ${r}`), name("ja", `サークル ${r}`), name("en", `The Circle ${r}`, "alias")],
      attributes: { artist_type: "circle", works: 1200, active: true, debut: "2020-05-01T12:00:00Z" },
    },
  ]);
  await page.goto(demo("entities.html", null, { group: artist!.id }));
  const header = page.locator("[data-ckui=entity-header]");
  await expect(header.getByRole("heading", { name: `Circle ${r}` })).toBeVisible({ timeout: 30_000 });
  const names = header.locator("[data-ckui=entity-names]");
  await expect(names).toContainText(`サークル ${r} (Japanese)`);
  await expect(names).toContainText(`The Circle ${r}`);
  const attrs = header.locator("[data-ckui=entity-attributes]");
  await expect(attrs).toContainText("Artist typeCircle");
  await expect(attrs).toContainText("Works1,200");
  await expect(attrs).toContainText("ActiveYes");
  await expect(attrs).toContainText("Debut");
});

test("membership editor: a content's groups with roles, positions and versions", async ({ page }) => {
  const { staff, ck } = await staffClient();
  const r = run();
  const content = await seedContent(ck, { kind: "comic", titles: { en: `Content ${r}` }, versions: [{ languages: ["en"] }, { languages: ["ja"] }] });
  const ja = content.versions[1]!.id;
  const [artist, series, voice] = (await ck.entities.create([
    { kind: "artist", slug: `pub-${r}`, names: [name("en", `Publisher ${r}`)] },
    { kind: "series", slug: `saga-${r}`, names: [name("en", `Saga ${r}`)] },
    { kind: "voice_actor", slug: `voice-${r}`, names: [name("en", `Voice ${r}`)] },
  ])) as [Entity, Entity, Entity];
  await page.goto(demo("entities.html", staff, { content: content.id, content_kind: "comic", versions: `${content.versions[0]!.id}:English,${ja}:Japanese` }));
  const editor = page.locator("[data-ckui=membership-editor]");
  const kind = (k: string) => editor.locator(`[data-ckui=membership-kind][data-entity-kind=${k}]`);
  await expect(kind("artist")).toBeVisible({ timeout: 30_000 });

  await searchable(ck, "artist", `Publisher ${r}`);
  await kind("artist").getByRole("combobox", { name: "Add: Artist" }).fill(`Publisher ${r}`);
  await page.getByRole("option", { name: new RegExp(`Publisher ${r}`) }).click();
  await kind("artist").getByRole("combobox", { name: `Role in Publisher ${r}` }).click();
  await page.getByRole("option", { name: "Publisher" }).click();

  await kind("series").getByRole("combobox", { name: "Add: Series" }).fill(`Saga ${r}`);
  await page.getByRole("option", { name: new RegExp(`Saga ${r}`) }).click();
  await kind("series").getByLabel("Position").fill("4");

  await kind("voice_actor").getByRole("combobox", { name: "Add: Voice actor" }).fill(`Voice ${r}`);
  await page.getByRole("option", { name: new RegExp(`Voice ${r}`) }).click();
  await kind("voice_actor").getByRole("combobox", { name: `Version in Voice ${r}` }).click();
  await page.getByRole("option", { name: "Japanese" }).click();

  await editor.getByRole("button", { name: "Save" }).click();
  await expect(editor.getByText("Saved.")).toBeVisible();
  const saved = (await ck.entities.ofContent(content.id)).memberships;
  expect(saved).toEqual(
    expect.arrayContaining([
      expect.objectContaining({ group_id: artist.id, role: "publisher" }),
      expect.objectContaining({ group_id: series.id, position: 4 }),
      expect.objectContaining({ group_id: voice.id, version_id: ja }),
    ]),
  );
  expect(saved).toHaveLength(3);

  await kind("series").getByRole("button", { name: `Remove Saga ${r}` }).click();
  await editor.getByRole("button", { name: "Save" }).click();
  await expect(editor.getByText("Saved.")).toBeVisible();
  expect((await ck.entities.ofContent(content.id)).memberships.map((m) => m.group_id).sort()).toEqual([artist.id, voice.id].sort());
});

for (const [mode, theme] of [
  ["alone", "light"],
  ["router", "dark"],
] as const) {
  test(`group manager: create, edit, names, members, cover, merge, delete and restore (${mode}, ${theme})`, async ({ page }, info) => {
    const { staff, ck } = await staffClient();
    const r = run();
    await page.goto(admin("/admin/groups/artist", staff, { mode, theme }));
    const flow = page.locator("[data-ckui=entity-manager]");
    await expect(flow.getByRole("heading", { name: "Artist", exact: true })).toBeVisible({ timeout: 30_000 });
    expect(await inheritsHostTheme(page, "group-manager")).toBe(true);

    await flow.getByRole("button", { name: "New: Artist" }).click();
    await expect(page).toHaveURL(/\/admin\/groups\/artist\/new$/);
    await flow.getByLabel("Name (English)").fill(`Studio ${r}`);
    await expect(flow.getByLabel("Slug")).toHaveValue(`studio-${r}`);
    await flow.getByRole("combobox", { name: "Artist type" }).click();
    await page.getByRole("option", { name: "Company" }).click();
    await flow.getByRole("button", { name: "Create" }).click();
    await expect(page).toHaveURL(/\/admin\/groups\/artist\/[0-9a-f-]{36}$/);
    const id = page.url().split("/").pop()!;
    if (mode === "router") await expect(page.locator("[data-demo=router-location]")).toHaveText(`/admin/groups/artist/${id}`);
    await expect(flow.locator("[data-ckui=entity-head]").getByRole("heading", { name: `Studio ${r}` })).toBeVisible();
    expect((await ck.entities.get(id)).attributes).toEqual({ artist_type: "company" });

    await flow.getByLabel("Works").fill("12");
    await flow.locator("[data-ckui=entity-details]").getByRole("button", { name: "Save" }).click();
    await expect(flow.locator("[data-ckui=entity-details]").getByText("Saved.")).toBeVisible();
    expect((await ck.entities.get(id)).attributes).toEqual({ artist_type: "company", works: 12 });

    const names = flow.locator("[data-ckui=entity-names-editor]");
    await names.getByRole("textbox", { name: "A name or alias" }).fill(`S. ${r}`);
    await names.getByRole("button", { name: "Add" }).click();
    await expect(names.locator("[data-ckui=entity-name][data-kind=alias]")).toContainText(`S. ${r}`);

    const texts = flow.locator("[data-ckui=entity-text]");
    await texts.getByLabel("Description (English)").fill(`Makes things ${r}.`);
    await texts.getByRole("button", { name: "Save" }).click();
    await expect(texts.getByText("Saved.")).toBeVisible();

    // A member, then its cover as the group's.
    const content = await seedContent(ck, { kind: "comic", titles: { en: `Member ${r}` }, versions: [{ languages: ["en"] }] });
    const members = flow.locator("[data-ckui=entity-members]");
    await members.getByRole("combobox", { name: "Search titles to add" }).fill(`Member ${r}`);
    await page.getByRole("option", { name: new RegExp(`Member ${r}`) }).click();
    // Artists have roles: picking opens the role choice before adding.
    await members.getByRole("button", { name: "Add member" }).click();
    await expect(members.locator(`[data-ckui=entity-member][data-content="${content.id}"]`)).toContainText(`Member ${r}`);
    await members.getByRole("button", { name: `Use as cover: Member ${r}` }).click();
    await expect(flow.locator("[data-ckui=cover-mode]")).toHaveText("A member's cover.");
    await page.screenshot({ path: `${dir}/group-manager-${mode}-${theme}-${info.project.name}.png`, fullPage: true });

    // Merge into another artist: the flow moves to the survivor.
    const [target] = await ck.entities.create([{ kind: "artist", slug: `target-${r}`, names: [name("en", `Target ${r}`)] }]);
    await searchable(ck, "artist", `Target ${r}`);
    await flow.getByRole("button", { name: "Merge" }).click();
    const dialog = page.locator("[data-ckui=entity-merge-dialog]");
    await dialog.getByRole("combobox", { name: "Merge into" }).fill(`Target ${r}`);
    await page.getByRole("option", { name: new RegExp(`Target ${r}`) }).click();
    await expect(dialog.getByRole("button", { name: "Merge" })).toBeDisabled();
    await dialog.getByLabel("Type merge to confirm").fill("merge");
    await dialog.getByRole("button", { name: "Merge" }).click();
    await expect(page).toHaveURL(new RegExp(`/admin/groups/artist/${target!.id}$`));
    await expect(flow.locator("[data-ckui=entity-head]").getByRole("heading", { name: `Target ${r}` })).toBeVisible();
    await expect(flow.locator(`[data-ckui=entity-members] [data-ckui=entity-member][data-content="${content.id}"]`)).toContainText(`Member ${r}`);
    await expect(flow.locator("[data-ckui=entity-names-editor]")).toContainText(`Studio ${r}`);

    await flow.locator("[data-ckui=entity-head]").getByRole("button", { name: "Delete" }).click();
    await page.locator("[data-ckui=entity-delete-dialog]").getByRole("button", { name: "Delete" }).click();
    await expect(flow.locator("[data-ckui=entity-deleted]")).toContainText("is deleted.");
    await flow.getByRole("button", { name: "Restore" }).click();
    await expect(flow.locator("[data-ckui=entity-head]").getByRole("heading", { name: `Target ${r}` })).toBeVisible();

    await flow.getByRole("button", { name: "Back to the list" }).click();
    await expect(page).toHaveURL(/\/admin\/groups\/artist$/);
  });
}

test("a caller without group permission is told so", async ({ page }) => {
  await page.goto(admin("/admin/groups", await h.user({ role: "moderator" }), { mode: "router" }));
  await expect(page.locator("[data-ckui=entity-manager] [data-ckui=no-access]")).toContainText("You don't have permission to manage groups.", { timeout: 30_000 });
});
