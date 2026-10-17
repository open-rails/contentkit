// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/index.js";
import type { Card, GroupCard } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/index.js";
import { GroupDirectory } from "../groups/directory.js";
import { ContentGrid } from "./grid.js";
import { Pagination, pageWindow } from "./pagination.js";

const card = (id: string): Card => ({
  id,
  kind: "gallery",
  code: id,
  path: `/en/g/${id}/${id}`,
  title: `Title ${id}`,
  language: "en",
  cover: [],
  version: { id: `${id}v`, code: "X1", languages: ["en"], traits: [], has_audio: false },
  entities: {},
  counts: {},
  access: { allowed: true, level: "public" },
  attributes: {},
});

const group = (n: number): GroupCard => ({ id: `g${n}`, kind: "tag", slug: `tag-${n}`, internal: false, name: `Tag ${n}`, count: n, attributes: {}, created_at: "2026-10-01T00:00:00Z" });

const wrap = (client: ContentKitClient) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ContentKitProvider client={client} navigate={vi.fn()}>
        {children}
      </ContentKitProvider>
    );
  };

it("windows page numbers around the current page with gaps", () => {
  expect(pageWindow(1, 1)).toEqual([1]);
  expect(pageWindow(1, 4)).toEqual([1, 2, 3, 4]);
  expect(pageWindow(6, 20)).toEqual([1, "gap", 4, 5, 6, 7, 8, "gap", 20]);
  expect(pageWindow(4, 20)).toEqual([1, 2, 3, 4, 5, 6, "gap", 20]);
  expect(pageWindow(20, 20)).toEqual([1, "gap", 18, 19, 20]);
});

it("shows the range, marks the current page and links each page", () => {
  const change = vi.fn();
  render(<Pagination page={2} total={60} pageSize={24} onPageChange={change} pageHref={(n) => `/tags/x?page=${n}`} />);
  expect(screen.getByRole("status")).toHaveTextContent("Showing 25–48 of 60");
  const nav = screen.getByRole("navigation", { name: "Pages" });
  expect(within(nav).getByRole("link", { name: "Page 2" })).toHaveAttribute("aria-current", "page");
  const third = within(nav).getByRole("link", { name: "Page 3" });
  expect(third).toHaveAttribute("href", "/tags/x?page=3");
  fireEvent.click(third);
  expect(change).toHaveBeenLastCalledWith(3);
  fireEvent.click(within(nav).getByRole("link", { name: "Previous page" }));
  expect(change).toHaveBeenLastCalledWith(1);
  expect(within(nav).queryByRole("link", { name: "Next page" })).toBeInTheDocument();
});

it("keeps a lower-bound total honest and disables the ends", () => {
  render(<Pagination page={1} total={10000} atLeast pageSize={100} onPageChange={vi.fn()} />);
  expect(screen.getByRole("status")).toHaveTextContent("Showing 1–100 of 10,000+");
  expect(screen.getByRole("button", { name: "Previous page" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Next page" })).toBeEnabled();
});

it("reads one numbered page of a list with its total and moves through the host's page", async () => {
  const reads: URL[] = [];
  const client = createContentKitClient({
    baseUrl: "https://ck.test",
    fetch: async (input) => {
      const u = new URL(String(input));
      reads.push(u);
      const page = Number(u.searchParams.get("page"));
      if (u.pathname !== "/v1/contents") return Response.json({ error: "not found", code: "not_found" }, { status: 404 });
      return Response.json({ contents: [card(`p${page}a`), card(`p${page}b`)], summary: { filtered_count: 0, filtered_entities: [], locked_count: 0, total: 5 } });
    },
  });
  const change = vi.fn();
  const { rerender } = render(<ContentGrid query={{ kind: "gallery", sort: "newest" }} pagination="pages" page={2} pageSize={2} onPageChange={change} />, { wrapper: wrap(client) });
  expect(await screen.findByText("Title p2a")).toBeInTheDocument();
  const first = reads.find((u) => u.pathname === "/v1/contents")!;
  expect(first.searchParams.get("page")).toBe("2");
  expect(first.searchParams.get("limit")).toBe("2");
  expect(first.searchParams.get("summary")).toBe("true");
  expect(first.searchParams.has("cursor")).toBe(false);
  expect(screen.getAllByRole("status").some((s) => s.textContent?.includes("Showing 3–4 of 5"))).toBe(true);
  fireEvent.click(screen.getAllByRole("button", { name: "Page 3" })[0]!);
  expect(change).toHaveBeenLastCalledWith(3);
  rerender(<ContentGrid query={{ kind: "gallery", sort: "newest" }} pagination="pages" page={3} pageSize={2} onPageChange={change} />);
  expect(await screen.findByText("Title p3a")).toBeInTheDocument();
  expect(reads.filter((u) => u.pathname === "/v1/contents").at(-1)!.searchParams.get("page")).toBe("3");
});

it("offers a directory sort and numbered pages of groups", async () => {
  const reads: URL[] = [];
  const client = createContentKitClient({
    baseUrl: "https://ck.test",
    fetch: async (input) => {
      const u = new URL(String(input));
      reads.push(u);
      const page = Number(u.searchParams.get("page"));
      if (u.pathname !== "/v1/entities") return Response.json({ error: "not found", code: "not_found" }, { status: 404 });
      return Response.json({ entities: [group(page * 10 + 1), group(page * 10 + 2)], total: 5 });
    },
  });
  const sort = vi.fn();
  render(<GroupDirectory kind="tag" letters={false} sorts={["name", "count", "newest"]} sort="count" onSortChange={sort} pagination="pages" pageSize={2} />, { wrapper: wrap(client) });
  expect(await screen.findByText("Tag 11")).toBeInTheDocument();
  const read = reads.find((u) => u.pathname === "/v1/entities")!;
  expect(read.searchParams.get("page")).toBe("1");
  expect(read.searchParams.get("sort")).toBe("count");
  expect(screen.getAllByRole("status").some((s) => s.textContent?.includes("Showing 1–2 of 5"))).toBe(true);
  fireEvent.click(screen.getAllByRole("button", { name: "Page 2" })[0]!);
  await waitFor(() => expect(screen.getByText("Tag 21")).toBeInTheDocument());
  expect(reads.filter((u) => u.pathname === "/v1/entities").at(-1)!.searchParams.get("page")).toBe("2");
  expect(screen.getByRole("combobox", { name: "Sort" })).toHaveTextContent("Most titles");
});
