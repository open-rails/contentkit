// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import { ContentKitProvider } from "../../react/index.js";
import { VersionDownloadButton } from "../../index.js";

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function setup(answers: Response[]) {
  const fetch = vi.fn(async () => answers.shift() ?? json(500, { error: "x", code: "internal_error" }));
  const client = createContentKitClient({ baseUrl: "https://api.test/ck", fetch: fetch as typeof globalThis.fetch });
  const clicked: string[] = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    clicked.push(this.href + " " + this.download);
  });
  render(
    <ContentKitProvider client={client}>
      <VersionDownloadButton content="c1" version="v1" />
    </ContentKitProvider>,
  );
  return { fetch, clicked };
}

const grant = (url: string) => ({ url, name: "Movie [A1B2].mp4", type: "video/mp4", size: 3, expires_at: new Date(Date.now() + 300_000).toISOString() });

it("waits while the file is prepared, then saves the media host's URL", async () => {
  const { fetch, clicked } = setup([json(202, { status: "pending" }), json(200, grant("https://media.test/v1/x/download/b?t=k"))]);
  vi.useFakeTimers({ shouldAdvanceTime: true });
  fireEvent.click(screen.getByRole("button", { name: /download/i }));
  await waitFor(() => expect(screen.getByRole("button").textContent).toMatch(/preparing/i));
  await vi.advanceTimersByTimeAsync(3500);
  await waitFor(() => expect(clicked).toEqual(["https://media.test/v1/x/download/b?t=k Movie [A1B2].mp4"]));
  expect(String(fetch.mock.calls[0]?.[0])).toBe("https://api.test/ck/v1/contents/c1/versions/v1/download");
  vi.useRealTimers();
});

it("resolves a marked archive's URL against the download route", async () => {
  const { clicked } = setup([json(200, grant("archive?for=u1&ticket=t"))]);
  fireEvent.click(screen.getByRole("button", { name: /download/i }));
  await waitFor(() => expect(clicked[0]).toMatch(/^https:\/\/api\.test\/ck\/v1\/contents\/c1\/versions\/v1\/archive\?for=u1&ticket=t /));
});

it("says how long a quota holds and disables the button meanwhile", async () => {
  setup([json(429, { error: "download quota reached", code: "rate_limited", retry_after: 600 })]);
  fireEvent.click(screen.getByRole("button", { name: /download/i }));
  await waitFor(() => expect(screen.getByRole("status").textContent).toMatch(/10 min/));
  expect((screen.getByRole("button") as HTMLButtonElement).disabled).toBe(true);
});

it("says a locked version must be unlocked, and when there is nothing to download", async () => {
  setup([json(403, { error: "not allowed", code: "forbidden" }), json(409, { error: "x", code: "no_download" })]);
  fireEvent.click(screen.getByRole("button", { name: /download/i }));
  await waitFor(() => expect(screen.getByRole("status").textContent).toMatch(/unlock/i));
  fireEvent.click(screen.getByRole("button", { name: /download/i }));
  await waitFor(() => expect(screen.getByRole("status").textContent).toMatch(/nothing to download/i));
});

it("says when a download the site holds back opens", async () => {
  const at = new Date(Date.now() + 2.5 * 86_400_000).toISOString();
  setup([json(403, { error: "x", code: "download_unavailable", reason: "release_window", available_at: at })]);
  fireEvent.click(screen.getByRole("button", { name: /download/i }));
  await waitFor(() => expect(screen.getByRole("status").textContent).toMatch(/in 3 days/));
});
