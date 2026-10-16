// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
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
  expect(screen.getByText(/English.*Colored.*Uncensored.*24 pages.*2 min/)).toBeVisible();
  expect(screen.getByText("A tag")).toBeVisible();
  expect(view.container.querySelector('[data-ckui="card-counts"]')).not.toBeNull();
});

it("lets a host show only page count, selected traits and filtered series without losing specific names", () => {
  const view = render(<ContentCard card={card} groupKinds={["series"]} chips={2} filterEntity={(entity) => entity.slug !== "original"} metadata={{ languages: false, traits: ["colored"], duration: false }} showCounts={false} labels={{ colored: "Colored" }} />);
  expect(screen.getByText("Colored · 24 pages")).toBeVisible();
  expect(screen.queryByText(/English|Uncensored|2 min/)).toBeNull();
  expect(screen.queryByText("Original")).toBeNull();
  expect(screen.queryByText("A tag")).toBeNull();
  expect(screen.getByText("A specific series")).toBeVisible();
  expect(screen.getByText("Original Story")).toBeVisible();
  expect(view.container.querySelector('[data-ckui="card-counts"]')).toBeNull();
});

it("holds back cover and decoration images while media loading is disabled", () => {
  const view = render(<ContentCard card={{ ...card, cover: [{ url: "https://media.test/cover.webp", w: 200, h: 300 }] }} loadMedia={false} decorate={() => <img src="https://media.test/badge.svg" alt="badge" />} />);
  expect(screen.getByText("A gallery")).toBeVisible();
  expect(view.container.querySelector("img[src]")).toBeNull();
});
