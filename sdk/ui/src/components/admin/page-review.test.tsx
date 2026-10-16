// @vitest-environment jsdom
import "../../test/dom.js";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import type { PageReview as Review, PageReviewEntry, PageReviewPatch } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/provider.js";
import { PageReview } from "./page-review.js";

const entry = (id: string, included = true): PageReviewEntry => ({
  id, path: `private/${id}.png`, source_name: `${Number(id)}.png`, source_sha256: "a".repeat(64), included, mode: "auto",
  decision: { reason: included ? "keep" : "exact_duplicate", confidence: included ? "high" : "exact" },
  flags: included ? [] : ["duplicate"], preview_url: "javascript:bad()", source_url: `https://media.test/source-${id}.png`,
});

function setup(options: { segmented?: boolean } = {}) {
  let current: Review = { revision: "r1", pages: [entry("001"), entry("002", false), entry("003")], selected: 2, flagged: 1 };
  if (options.segmented) {
    current.pages[0]!.flags.push("segmented_selection_preserved");
    current.pages[0]!.preview_url = "https://media.test/preview-001.png";
  }
  const patches: PageReviewPatch[] = [];
  let gets = 0;
  const client = createContentKitClient({ baseUrl: "https://api.test", fetch: (async (_input, init) => {
    if (init?.method !== "PATCH") { gets++; return Response.json(current); }
    const patch = JSON.parse(String(init.body)) as PageReviewPatch;
    patches.push(patch);
    if (patch.revision !== current.revision) return Response.json({ code: "conflict", error: "Page review changed" }, { status: 409 });
    for (const change of patch.pages ?? []) {
      const page = current.pages.find((p) => p.id === change.id)!;
      page.mode = change.mode;
      page.included = change.mode === "include" || (change.mode === "auto" && page.id !== "002");
    }
    if (patch.order) current.pages = patch.order.map((id) => current.pages.find((page) => page.id === id)!);
    current = { ...current, revision: `r${Number(current.revision.slice(1)) + 1}`, selected: current.pages.filter((p) => p.included).length };
    return Response.json(current);
  }) as typeof fetch });
  const view = render(<ContentKitProvider client={client}><PageReview content="content" version="version" onError={vi.fn()} /></ContentKitProvider>);
  return { ...view, patches, current: () => current, changeElsewhere: () => { current = { ...current, revision: "r2" }; }, gets: () => gets };
}

const pageRow = (name: string) => screen.getByText(name).closest<HTMLElement>('[data-ckui="page-review-row"]')!;
async function choose(user: ReturnType<typeof userEvent.setup>, name: string, choice: string) {
  await waitFor(() => expect(within(pageRow(name)).getByRole("combobox")).toBeEnabled());
  await user.click(within(pageRow(name)).getByRole("combobox"));
  await user.click(await screen.findByRole("option", { name: choice }));
}

it("restores an excluded original and can reset the manual choice without sending order", async () => {
  const s = setup();
  const user = userEvent.setup();
  await screen.findByText("2.png");
  expect(within(pageRow("2.png")).getByText("exact duplicate", { exact: false })).toBeVisible();
  expect(s.container.querySelector("img")).toBeNull();
  expect(within(pageRow("2.png")).getByRole("link", { name: "Open original" })).toHaveAttribute("href", "https://media.test/source-002.png");
  expect(s.container.textContent).not.toContain("private/002.png");
  await choose(user, "2.png", "Include");
  await waitFor(() => expect(pageRow("2.png")).toHaveAttribute("data-included", "true"));
  expect(s.patches[0]).toEqual({ revision: "r1", pages: [{ id: "002", mode: "include" }] });
  await choose(user, "2.png", "Use automatic choice");
  await waitFor(() => expect(pageRow("2.png")).toHaveAttribute("data-included", "false"));
  expect(s.patches[1]).toEqual({ revision: "r2", pages: [{ id: "002", mode: "auto" }] });
});

it("sends all source IDs, including excluded pages, only for an actual order move", async () => {
  const s = setup();
  await screen.findByText("1.png");
  const rect = vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
    const li = this.closest("[data-overlay]") ? document.querySelector("li[data-dragging]") : this.closest("li");
    const i = li?.parentElement ? [...li.parentElement.children].indexOf(li) : 0;
    return { x: 0, y: i * 40, top: i * 40, left: 0, bottom: i * 40 + 36, right: 300, width: 300, height: 36, toJSON: () => ({}) } as DOMRect;
  });
  try {
    const handle = screen.getByRole("button", { name: "Reorder 1.png" });
    const tick = () => act(() => new Promise((resolve) => setTimeout(resolve, 20)));
    handle.focus();
    fireEvent.keyDown(handle, { key: " ", code: "Space" });
    await tick();
    fireEvent.keyDown(document.activeElement!, { key: "Escape", code: "Escape" });
    await tick();
    expect(s.patches).toHaveLength(0);
    handle.focus();
    fireEvent.keyDown(handle, { key: " ", code: "Space" });
    await tick();
    fireEvent.keyDown(document.activeElement!, { key: "ArrowDown", code: "ArrowDown" });
    await tick();
    fireEvent.keyDown(document.activeElement!, { key: " ", code: "Space" });
    await waitFor(() => expect(s.patches).toHaveLength(1));
    expect(s.patches[0]).toEqual({ revision: "r1", order: ["002", "001", "003"] });
  } finally { rect.mockRestore(); }
});

it("reloads a stale revision without retrying the write, then uses the new revision", async () => {
  const s = setup();
  const user = userEvent.setup();
  await screen.findByText("2.png");
  s.changeElsewhere();
  await choose(user, "2.png", "Include");
  await screen.findByText("Page choices changed elsewhere. Review the latest choices before trying again.");
  await waitFor(() => expect(s.gets()).toBe(2));
  expect(s.patches).toHaveLength(1);
  await choose(user, "2.png", "Include");
  await waitFor(() => expect(s.patches).toHaveLength(2));
  expect(s.patches[1]!.revision).toBe("r2");
});

it("preserves explicit chapter boundaries while leaving original previews available", async () => {
  const s = setup({ segmented: true });
  await screen.findByText("Chapter boundaries are preserved. Use a chapter-aware editor to change page order or inclusion.");
  for (const control of screen.getAllByRole("combobox")) expect(control).toBeDisabled();
  for (const handle of screen.getAllByRole("button", { name: /^Reorder / })) expect(handle).toBeDisabled();
  expect(s.container.querySelector('img[src="https://media.test/preview-001.png"]')).toBeVisible();
  expect(within(pageRow("1.png")).getByRole("link", { name: "Open original" })).toBeVisible();
  expect(s.patches).toHaveLength(0);
});
