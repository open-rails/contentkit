// @vitest-environment jsdom
import "../../src/test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeAll, expect, it } from "vitest";
import { ContentManager } from "../../src/index.js";
import type { Config, TestUser } from "../support/harness.js";
import { client, harness, png, wait } from "./setup.js";

const h = harness();
let cfg: Config;
let staff: TestUser;
beforeAll(async () => {
  cfg = await h.config();
  staff = await h.user({ role: "staff" });
});

it("bulk upload makes one draft per file, titled from its name, with the file as its page", async () => {
  const c = client(h, cfg, staff);
  const user = userEvent.setup({ applyAccept: false });
  const suffix = crypto.randomUUID().slice(0, 8);
  render(<ContentManager client={c} kind="comic" languages={["en"]} />);
  await user.click(await screen.findByRole("button", { name: "Bulk upload" }, wait));
  const panel = screen.getByRole("region", { name: "Bulk upload" });
  await user.upload(within(panel).getByLabelText("Add files"), [png(`Bulk_one-${suffix}.png`, 171), png(`Bulk_two-${suffix}.png`, 172)]);
  await waitFor(() => expect(panel.querySelectorAll('[data-ckui="upload-entry"][data-status="done"]')).toHaveLength(2), wait);

  const drafts = await c.contents.manage({ kind: "comic", state: "draft", limit: 100 });
  for (const title of [`Bulk one ${suffix}`, `Bulk two ${suffix}`]) {
    const draft = drafts.find((d) => d.title === title);
    expect(draft, title).toBeDefined();
    const version = [...draft!.versions].sort((a, b) => a.position - b.position)[0]!;
    const read = await c.media.read({ kind: "comic", id: draft!.id, version: version.id }, { editor: true });
    expect(read.files.filter((f) => f.upload && f.path.startsWith("originals/"))).toHaveLength(1);
  }

  // Each finished entry opens its draft in the editor.
  await user.click(within(panel).getAllByRole("button", { name: "Open" })[0]!);
  expect(await screen.findByRole("textbox", { name: "Title" }, wait)).toHaveValue(`Bulk one ${suffix}`);
});
