// @vitest-environment jsdom
import "../../test/dom.js";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { ReaderStats } from "./reader-stats.js";

afterEach(cleanup);

it("shows private billing tenure with content counts and clears them at sign-out", async () => {
  const fetcher = vi.fn(async (_input: RequestInfo | URL) => Response.json({ completed: { gallery: 1234 }, comments: 12, comment_likes: 24, comment_dislikes: 3, access_seconds: { premium: 60 * 24 * 60 * 60 } }));
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  let viewer: string | null = "reader";
  const tree = () => <ContentKitProvider client={client} viewer={viewer}><ReaderStats kinds={["gallery"]} tiers={["premium"]} labels={{ gallery: "Galleries", premium: "Premium" }} /></ContentKitProvider>;
  const { rerender } = render(tree());
  await screen.findByText("2 months");
  expect(screen.getByText("Premium for")).toBeVisible();
  expect(screen.getByText("Galleries finished")).toBeVisible();
  expect(screen.getByText("1,234")).toBeVisible();
  expect(String(fetcher.mock.calls[0]?.[0])).toContain("/ck/v1/me/stats");
  const calls = fetcher.mock.calls.length;
  viewer = null;
  rerender(tree());
  expect(screen.queryByText("2 months")).toBeNull();
  expect(fetcher).toHaveBeenCalledTimes(calls);
});

it("distinguishes missing statistics from real zero values", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async () => Response.json({ completed: {}, comments: 0, comment_likes: 0, comment_dislikes: 0 }) });
  render(<ContentKitProvider client={client} viewer="reader"><ReaderStats kinds={["gallery"]} tiers={["premium"]} /></ContentKitProvider>);
  await screen.findByText("Some statistics are temporarily unavailable.");
  expect(screen.getAllByText("—")).toHaveLength(2);
  expect(screen.getAllByText("0")).toHaveLength(3);
});

it.each<[number, string]>([
  [0, "0 days"],
  [3600, "Less than 1 day"],
  [86400, "1 day"],
  [29 * 86400, "29 days"],
  [30 * 86400, "1 month"],
  [60 * 86400, "2 months"],
])("renders four selected account statistics for %i seconds", async (seconds, expected) => {
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async () => Response.json({ completed: { gallery: 0 }, comments: 5, comment_likes: 9, comment_dislikes: 4, access_seconds: { premium: seconds } }) });
  const { container } = render(<ContentKitProvider client={client} viewer="reader"><ReaderStats kinds={["gallery"]} tiers={["premium"]} showDislikes={false} completedLabels={{ gallery: "Galleries read" }} /></ContentKitProvider>);
  await screen.findByText(expected);
  expect(container.querySelectorAll('[data-ckui="reader-stat"]')).toHaveLength(4);
  expect(screen.getByText("Galleries read")).toBeVisible();
  expect(screen.queryByText("Dislikes received")).toBeNull();
});
