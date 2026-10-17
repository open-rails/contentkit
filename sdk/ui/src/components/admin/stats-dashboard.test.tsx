// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { StatsDashboard } from "./stats-dashboard.js";

const days = [
  { date: "2026-10-16", readers: 12, users: 8, active_ms: 90_000 },
  { date: "2026-10-15", readers: 10, users: 7, active_ms: 30_000 },
];
function server(unavailable = false) {
  const requests: URL[] = [];
  const client = createContentKitClient({
    baseUrl: "https://stats.test",
    fetch: async (input) => {
      const url = new URL(String(input));
      requests.push(url);
      if (unavailable && (url.pathname.endsWith("/activity") || url.searchParams.get("metric") !== "comments")) {
        return Response.json({ code: "not_configured", error: "Measured engagement is unavailable" }, { status: 503 });
      }
      if (url.pathname.endsWith("/activity")) return Response.json({ days });
      if (url.pathname.endsWith("/top")) return Response.json({ contents: [{ id: "work", title: "Gallery example", value: 12, path: "/g/example" }] });
      return Response.json({}, { status: 404 });
    },
  });
  return { client, requests };
}

it("renders measured people and attention without turning daily people into a period-unique total", async () => {
  const s = server();
  render(
    <ContentKitProvider client={s.client} viewer="staff">
      <StatsDashboard kinds={["gallery"]} />
    </ContentKitProvider>,
  );
  const table = await screen.findByRole("table");
  expect(within(table).getByRole("columnheader", { name: "People engaged" })).toBeVisible();
  expect(within(table).getByRole("columnheader", { name: "Signed-in people" })).toBeVisible();
  expect(within(table).getByRole("columnheader", { name: "Active time" })).toBeVisible();
  const current = within(table).getByRole("row", { name: "2026-10-16 12 8 1:30" });
  expect(current).toBeVisible();
  expect(within(table).getByRole("row", { name: "Total active time 2:00" })).toBeVisible();
  expect(within(table).queryByRole("cell", { name: "22" })).toBeNull();
  expect(screen.queryByText(/views|sessions/i)).toBeNull();
  await waitFor(() => expect(s.requests.some((u) => u.pathname.endsWith("/top") && u.searchParams.get("metric") === "readers")).toBe(true));
});

it("ranks by distinct engaged people or comments and never offers the retired views metric", async () => {
  const s = server();
  render(
    <ContentKitProvider client={s.client} viewer="staff">
      <StatsDashboard kinds={["gallery"]} />
    </ContentKitProvider>,
  );
  await screen.findByRole("link", { name: "Gallery example" });
  const user = userEvent.setup();
  await user.click(screen.getByRole("combobox", { name: "Ranked by" }));
  expect(screen.queryByRole("option", { name: "Views" })).toBeNull();
  await user.click(await screen.findByRole("option", { name: "Comments" }));
  await waitFor(() => expect(s.requests.some((u) => u.pathname.endsWith("/top") && u.searchParams.get("metric") === "comments")).toBe(true));
  expect(s.requests.filter((u) => u.pathname.endsWith("/top")).every((u) => u.searchParams.get("metric") !== "views")).toBe(true);
});

it("keeps comment rankings usable when measured engagement is unavailable", async () => {
  const s = server(true);
  render(
    <ContentKitProvider client={s.client} viewer="staff">
      <StatsDashboard kinds={["gallery"]} />
    </ContentKitProvider>,
  );
  expect((await screen.findAllByText("Measured engagement is not available on this site.")).length).toBeGreaterThan(0);
  const user = userEvent.setup();
  await user.click(screen.getByRole("combobox", { name: "Ranked by" }));
  await user.click(await screen.findByRole("option", { name: "Comments" }));
  expect(await screen.findByRole("link", { name: "Gallery example" })).toBeVisible();
});
