// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import type { Card } from "../../client/generated/wire.js";
import { ContentCard } from "./card.js";

const card: Card = {
  id: "gallery", kind: "gallery", code: "G1", path: "/gallery", title: "A gallery", language: "en", cover: [],
  version: { id: "version", code: "V1", languages: ["en"], traits: ["colored", "uncensored"], pages: 24, duration_ms: 120000, has_audio: false },
  entities: {
    series: [{ id: "generic", slug: "original", name: "Original" }, { id: "series", slug: "series", name: "A specific series" }, { id: "story", slug: "original-story", name: "Original Story" }],
    tag: [{ id: "tag", slug: "tag", name: "A tag" }],
  },
  counts: { likes: 120, favorites: 35 }, access: { allowed: true, level: "public" }, attributes: {},
};

it("keeps the default card metadata and counts unless the host chooses otherwise", () => {
  const view = render(<ContentCard card={card} labels={{ colored: "Colored", uncensored: "Uncensored" }} />);
  expect(screen.getByText("English · 24 pages · 2 min")).toBeVisible();
  expect(screen.getByText("Colored")).toHaveAttribute("data-ckui", "card-trait");
  expect(screen.getByText("Uncensored")).toHaveAttribute("data-ckui", "card-trait");
  expect(screen.getByText("A tag")).toBeVisible();
  expect(view.container.querySelector('[data-ckui="card-counts"]')).not.toBeNull();
  expect(view.container.querySelector('[data-ckui="card-cover"]')?.contains(screen.getByRole("link", { name: "A gallery" }))).toBe(false);
});

it("lets a host show only page count, selected traits and filtered series without losing specific names", () => {
  const view = render(<ContentCard card={card} groupKinds={["series"]} chips={2} filterEntity={(entity) => entity.slug !== "original"} metadata={{ languages: false, traits: ["colored"], duration: false }} showCounts={false} labels={{ colored: "Colored" }} />);
  expect(screen.getByText("Colored")).toHaveAttribute("data-variant", "outline");
  expect(screen.getByText("24 pages")).toBeVisible();
  expect(screen.queryByText(/English|Uncensored|2 min/)).toBeNull();
  expect(screen.queryByText("Original")).toBeNull();
  expect(screen.queryByText("A tag")).toBeNull();
  expect(screen.getByText("A specific series")).toBeVisible();
  expect(screen.getByText("Original Story")).toBeVisible();
  expect(view.container.querySelector('[data-ckui="card-counts"]')).toBeNull();
});

it.each([false, []] as const)("omits trait badges when the host disables or selects no traits: %j", (traits) => {
  const view = render(<ContentCard card={card} metadata={{ traits }} labels={{ colored: "Colored", uncensored: "Uncensored" }} />);
  expect(view.container.querySelector('[data-ckui="card-traits"]')).toBeNull();
  expect(screen.queryByText("Colored")).toBeNull();
  expect(screen.queryByText("Uncensored")).toBeNull();
  expect(screen.getByText("English · 24 pages · 2 min")).toBeVisible();
});

it("can show a selected trait as the only metadata without restoring hidden version fields", () => {
  const view = render(<ContentCard card={card} metadata={{ languages: false, traits: ["colored"], pages: false, duration: false }} labels={{ colored: "Colored" }} />);
  expect(screen.getByText("Colored")).toHaveAttribute("data-trait", "colored");
  expect(screen.queryByText(/English|Uncensored|24 pages|2 min/)).toBeNull();
  view.rerender(<ContentCard card={{ ...card, version: { ...card.version, traits: [] } }} metadata={{ traits: ["colored"] }} labels={{ colored: "Colored" }} />);
  expect(view.container.querySelector('[data-ckui="card-trait"]')).toBeNull();
});

it("holds back cover and decoration images while media loading is disabled", () => {
  const view = render(<ContentCard card={{ ...card, cover: [{ url: "https://media.test/cover.webp", w: 200, h: 300 }] }} loadMedia={false} decorate={() => <img src="https://media.test/badge.svg" alt="badge" />} />);
  expect(screen.getByText("A gallery")).toBeVisible();
  expect(view.container.querySelector("img[src]")).toBeNull();
});

it("supports a tall cover with one accessible overlay title and plain linked entities", () => {
  const opened = vi.fn();
  const view = render(<ContentCard
    card={{ ...card, entities: { series: [{ id: "series", slug: "series", name: "A specific series", path: "/series" }] } }}
    aspectRatio="182 / 260" titlePosition="cover" entityStyle="text" onOpen={opened} loadMedia={false}
    metadata={{ languages: false, traits: ["colored"], duration: false }} labels={{ colored: "Colored" }}
  />);
  const title = screen.getByRole("link", { name: "A gallery" });
  expect(screen.getAllByRole("link", { name: "A gallery" })).toHaveLength(1);
  expect(view.container.querySelector('[data-ckui="card-cover"]')?.contains(title)).toBe(true);
  expect(view.container.querySelector<HTMLElement>('[data-ckui="card-cover-frame"]')?.style.aspectRatio).toBe("182 / 260");
  const entity = screen.getByRole("link", { name: "A specific series" });
  expect(entity).toHaveAttribute("href", "/series");
  expect(entity).not.toHaveClass("border");
  expect(entity.compareDocumentPosition(screen.getByText("Colored")) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0);
  expect(view.container.querySelector("img[src]")).toBeNull();
  title.addEventListener("click", (event) => event.preventDefault());
  fireEvent.click(title);
  expect(opened).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ id: "gallery" }));
});

it("keeps reading progress above a cover title without changing the default layering", () => {
  const reading: Card = { ...card, viewer: { favorited: false, seen: true, completed: false, progress: 5, progress_max: 10 } };
  const view = render(<ContentCard card={reading} titlePosition="cover" />);
  let progress = view.container.querySelector('[data-ckui="card-progress"]')!;
  expect(progress).toHaveClass("z-20");
  expect(progress.firstElementChild).toHaveStyle({ width: "50%" });
  expect(screen.getByRole("link", { name: "A gallery" })).toHaveClass("z-10");
  view.rerender(<ContentCard card={reading} />);
  progress = view.container.querySelector('[data-ckui="card-progress"]')!;
  expect(progress).not.toHaveClass("z-20");
  expect(progress.firstElementChild).toHaveStyle({ width: "50%" });
});
