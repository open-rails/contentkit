// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { Card } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/index.js";
import { ContentCard, PostCard } from "./card.js";

const card = (o: Partial<Card> = {}): Card => ({
  id: "c1",
  kind: "gallery",
  code: "AB12",
  path: "/en/g/AB12/atlas",
  title: "Atlas",
  language: "en",
  cover: [],
  version: { id: "v1", code: "X1", languages: ["en"], traits: [], pages: 24 },
  groups: { tag: [{ id: "g1", slug: "vanilla", name: "Vanilla" }] },
  counts: { likes: 3, dislikes: 0, favorites: 5, views: 40 },
  access: { allowed: true },
  attributes: {},
  ...o,
});
// What a locked view showing only cover and groups leaves of a premium card.
const locked = card({ version: { id: "v1", code: "X1", languages: ["en"], traits: [] }, counts: {}, access: { allowed: false, sell: ["premium"] } });

function Wrapper({ children }: { children: ReactNode }) {
  return <ContentKitProvider client={createContentKitClient({ baseUrl: "", fetch: vi.fn() })}>{children}</ContentKitProvider>;
}

it("ContentCard shows the counts and length a card carries and leaves out hidden ones: no zero counts", () => {
  const { container, rerender } = render(<ContentCard card={card()} />, { wrapper: Wrapper });
  expect(container.querySelector('[data-ckui="card-counts"]')).toHaveTextContent(/5.*3/);
  expect(screen.getByText(/24 pages/)).toBeInTheDocument();

  rerender(<ContentCard card={locked} tiers={["premium"]} />);
  expect(container.querySelector('[data-ckui="card-counts"]')).toBeNull();
  expect(screen.queryByText(/pages/)).toBeNull();
  expect(screen.getByText("Vanilla")).toBeInTheDocument();
  expect(container.querySelector('[data-ckui="card-lock"]')).toHaveTextContent("Premium");
});

it("PostCard leaves out a hidden favorite count", () => {
  const { container, rerender } = render(<PostCard card={card({ kind: "blog" })} />, { wrapper: Wrapper });
  expect(container.querySelector('[data-ckui="card-counts"]')).toHaveTextContent("5");
  rerender(<PostCard card={{ ...locked, kind: "blog" }} />);
  expect(container.querySelector('[data-ckui="card-counts"]')).toBeNull();
});
