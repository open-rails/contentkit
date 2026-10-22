// @vitest-environment jsdom
import "../../src/test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeAll, expect, it } from "vitest";
import type { Content } from "../../src/client/index.js";
import { ContentEditor } from "../../src/index.js";
import type { Config, TestUser } from "../support/harness.js";
import { client, harness, png, recorder, wait } from "./setup.js";

const h = harness();
let cfg: Config;
let staff: TestUser;
beforeAll(async () => { cfg = await h.config(); staff = await h.user({ role: "staff" }); });

it("uploads, associates, chooses a stored cover and publishes the current gallery edits in one workspace", async () => {
  const record = recorder();
  const c = client(h, cfg, staff, { record });
  const visitor = client(h, cfg, null);
  const suffix = crypto.randomUUID().slice(0, 8);
  const artistName = `Workspace artist ${suffix}`;
  const [artist] = await c.entities.create([{ kind: "artist", slug: `workspace-${suffix}`, names: [{ language: "en", kind: "name", name: artistName }] }]);
  let created: Content | undefined;
  const user = userEvent.setup({ applyAccept: false });
  const view = render(<ContentEditor management client={c} kind="comic" languages={["en", "ja"]} id={null} onCreated={(content) => { created = content; }} />);
  const title = await screen.findByLabelText("Title", undefined, wait);
  await user.type(title, `Workspace initial ${suffix}`);
  expect(record.calls.some((path) => path.endsWith("/v1/contents"))).toBe(false);
  const input = screen.getByLabelText("Gallery files") as HTMLInputElement;
  await user.upload(input, [png("1.png", 161, 320, 450), png("2.png", 162, 320, 450)]);
  await waitFor(() => expect(created?.id).toBeTruthy(), wait);
  expect(view.container.querySelector('[data-ckui="reader-content-editor"]')).not.toBeNull();
  expect(screen.queryByRole("button", { name: "Create draft" })).toBeNull();
  await user.clear(title);
  await user.type(title, `Workspace published ${suffix}`);
  await user.type(screen.getByRole("combobox", { name: "Add: Artist" }), artistName);
  await user.click(await screen.findByRole("option", { name: new RegExp(artistName) }, { timeout: 90_000 }));
  await waitFor(() => expect(view.container.querySelectorAll('[data-ckui="page-tile"]')).toHaveLength(2), wait);
  await waitFor(() => expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled(), wait);
  await user.click(screen.getByRole("button", { name: "Reorder 1.png" }));
  await user.click(screen.getByRole("button", { name: "Thumbnail" }));
  const choices = await screen.findByRole("dialog");
  await user.click(within(choices).getByRole("button", { name: "Set as thumbnail" }));
  // Surface a failed copy or cover update instead of only a missing dialog.
  await waitFor(() => {
    const alert = screen.queryAllByRole("alert").map((a) => a.textContent).join("; ");
    expect(alert).toBe("");
    expect(screen.getByRole("dialog")).toBeTruthy();
  }, wait);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled(), wait);
  await user.click(screen.getByRole("button", { name: "Publish" }));
  await waitFor(async () => expect((await c.contents.get(created!.id, "en")).live).toBe(true), wait);
  expect((await c.contents.get(created!.id, "en")).title).toBe(`Workspace published ${suffix}`);
  expect((await c.entities.ofContent(created!.id)).memberships.some((membership) => membership.group_id === artist!.id)).toBe(true);
  const version = (await c.contents.get(created!.id, "en")).versions[0]!;
  await waitFor(async () => {
    const plan = await c.versions.reader(created!.id, version.id);
    expect("pages" in plan ? plan.pages : undefined).toBe(2);
  }, wait);
  await waitFor(async () => expect((await visitor.contents.get(created!.id, "en")).title).toBe(`Workspace published ${suffix}`), wait);
  expect(record.presigns).toHaveLength(2);
  expect(record.commits.flat().some((operation) => operation.op === "copy")).toBe(true);
});
