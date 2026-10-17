// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { PageNotices as Notices } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/provider.js";
import { PageNotices } from "./page-notices.js";

const removed = { id: "removed-1", kind: "removed", pages: [{ id: "004", source_name: "4.png", reason: "blank" }, { id: "006", source_name: "6.png", reason: "spread" }] };
const reordered = { id: "reordered-1", kind: "reordered", pages: [{ id: "001", source_name: "1.png", from: 2, to: 1 }, { id: "010", source_name: "10.png", from: 1, to: 2 }] };

function setup(initial: Notices) {
  let notices = initial;
  const calls: string[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = new URL(String(input), "https://test.invalid").pathname.replace(/^\/v1/, "");
    calls.push(`${init?.method ?? "GET"} ${path}`);
    if (path === "/contents/c1/versions/v1/page-notices") return Response.json(notices);
    if (path.endsWith("/dismiss")) return new Response(null, { status: 204 });
    if (path.endsWith("/restore")) {
      notices = { notices: notices.notices.filter((n) => n.id !== "removed-1") };
      return Response.json(notices);
    }
    return Response.json({ code: "not_found" }, { status: 404 });
  });
  const client = createContentKitClient({ baseUrl: "https://content.test", fetch: fetch as typeof globalThis.fetch });
  const view = render(
    <ContentKitProvider client={client} viewer="admin">
      <PageNotices content="c1" version="v1" />
    </ContentKitProvider>,
  );
  return { calls, view };
}

it("stays out of the way without notices", async () => {
  const { calls, view } = setup({ notices: [] });
  await waitFor(() => expect(calls).toContain("GET /contents/c1/versions/v1/page-notices"));
  expect(view.container.querySelector('[data-ckui="page-notices"]')).toBeNull();
});

it("lists removed and reordered pages quietly, restoring and dismissing in one click", async () => {
  const user = userEvent.setup();
  const { calls } = setup({ notices: [removed, reordered] });
  const summary = await screen.findByText("2 notes on automatic page changes");
  expect(summary.closest("details")).not.toHaveAttribute("open");
  await user.click(summary);
  expect(screen.getByText("Removed 2 pages")).toBeVisible();
  expect(screen.getByText("blank")).toBeVisible();
  expect(screen.getByText("in a two-page spread")).toBeVisible();
  expect(screen.getByText("10.png, 1.png")).toBeVisible();
  expect(screen.getByText("1.png, 10.png")).toBeVisible();
  await user.click(screen.getByRole("button", { name: "Restore" }));
  await waitFor(() => expect(screen.queryByText("Removed 2 pages")).toBeNull());
  expect(calls).toContain("POST /contents/c1/versions/v1/page-notices/removed-1/restore");
  await user.click(screen.getByRole("button", { name: "Dismiss" }));
  await waitFor(() => expect(screen.queryByText(/notes? on automatic page changes/)).toBeNull());
  expect(calls).toContain("POST /contents/c1/versions/v1/page-notices/reordered-1/dismiss");
});
