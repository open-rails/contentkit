// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { GroupCard, Membership } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/index.js";
import { EntityChips, resolveToken, splitTokens } from "./entity-chips.js";

const card = (id: string, name: string, slug = name.toLowerCase().replace(/\s+/g, "-")): GroupCard =>
  ({ id, kind: "tag", slug, internal: false, name, count: 0, attributes: {}, created_at: "2026-01-01T00:00:00Z" }) as GroupCard;
const catalog = [card("g1", "Vanilla"), card("g2", "Big Breasts"), card("g3", "Café Au Lait", "cafe-au-lait")];

function Harness({ initial = [] as Membership[] }) {
  const [memberships, setMemberships] = useState<Membership[]>(initial);
  const [cards, setCards] = useState(new Map(catalog.map((g) => [g.id, g])));
  return (
    <EntityChips
      kind="tag"
      label="Tags"
      memberships={memberships}
      cards={cards}
      spec={{}}
      versionLabel={(id) => id}
      onAdd={(g) => {
        setCards((all) => new Map(all).set(g.id, g));
        setMemberships((m) => [...m, { group_id: g.id }]);
      }}
      onChange={() => {}}
      onRemove={(i) => setMemberships((m) => m.filter((_, j) => j !== i))}
    />
  );
}

function setup(initial?: Membership[]) {
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const q = (new URL(String(input), "https://ck.test").searchParams.get("q") ?? "").toLowerCase();
    const fold = (s: string) => s.normalize("NFKD").replace(/\p{M}/gu, "").toLowerCase();
    return Response.json({ contents: [], entities: catalog.filter((g) => fold(g.name).includes(fold(q))) });
  });
  const client = createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch });
  render(
    <ContentKitProvider client={client} viewer="admin">
      <Harness initial={initial} />
    </ContentKitProvider>,
  );
  return screen.getByRole("combobox", { name: "Add: Tags" });
}

it("splits commas and newlines into trimmed tokens", () => {
  expect(splitTokens(" Vanilla, big breasts ,\nCafé,, ")).toEqual(["Vanilla", "big breasts", "Café"]);
});

it("resolves exact names and slugs before a lone alias match", () => {
  expect(resolveToken("cafe au lait", catalog, "tag")?.id).toBe("g3");
  expect(resolveToken("BIG-BREASTS", catalog, "tag")?.id).toBe("g2");
  expect(resolveToken("van", [catalog[0]!], "tag")?.id).toBe("g1");
  expect(resolveToken("a", catalog, "tag")).toBeUndefined();
});

it("turns a pasted comma list into chips and marks names without a match", async () => {
  const user = userEvent.setup();
  const input = setup();
  await user.click(input);
  await user.paste("vanilla, Cafe au lait, Nonexistent, vanilla");
  await user.keyboard("{Enter}");
  await waitFor(() => expect(screen.getByRole("button", { name: "Remove Café Au Lait" })).toBeVisible());
  expect(screen.getByRole("button", { name: "Remove Vanilla" })).toBeVisible();
  expect(screen.getAllByRole("button", { name: /^Remove Vanilla$/ })).toHaveLength(1);
  expect(await screen.findByRole("status")).toHaveTextContent("Not found, so not saved: Nonexistent");
});

it("removes chips with × and with Backspace in an empty box", async () => {
  const user = userEvent.setup();
  const input = setup([{ group_id: "g1" }, { group_id: "g2" }]);
  await user.click(screen.getByRole("button", { name: "Remove Vanilla" }));
  expect(screen.queryByRole("button", { name: "Remove Vanilla" })).toBeNull();
  await user.click(input);
  await user.keyboard("{Backspace}");
  expect(screen.queryByRole("button", { name: "Remove Big Breasts" })).toBeNull();
});

it("adds a suggestion picked with the keyboard", async () => {
  const user = userEvent.setup();
  const input = setup();
  await user.click(input);
  await user.keyboard("big");
  await screen.findByRole("option", { name: /Big Breasts/ });
  await user.keyboard("{Enter}");
  expect(await screen.findByRole("button", { name: "Remove Big Breasts" })).toBeVisible();
});
