// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { StatsDashboard } from "./stats-dashboard.js";

const active: Record<string, { anon: number; free: number; premium: number; active_ms: number }> = {
  "2026-10-16": { anon: 5, free: 4, premium: 3, active_ms: 90_000 },
  "2026-10-15": { anon: 6, free: 3, premium: 1, active_ms: 30_000 },
};

// Zero-filled, newest first, like the server.
function series(days: number) {
  return Array.from({ length: days }, (_, i) => {
    const date = new Date(Date.UTC(2026, 9, 16 - i)).toISOString().slice(0, 10);
    const d = active[date] ?? { anon: 0, free: 0, premium: 0, active_ms: 0 };
    return { date, readers: d.anon + d.free + d.premium, ...d };
  });
}

function server({ unavailable = false, empty = false } = {}) {
  const requests: URL[] = [];
  const client = createContentKitClient({
    baseUrl: "https://stats.test",
    fetch: async (input) => {
      const url = new URL(String(input));
      requests.push(url);
      if (unavailable) return Response.json({ code: "not_configured", error: "Measured engagement is unavailable" }, { status: 503 });
      if (url.pathname.endsWith("/activity")) {
        const days = series(Number(url.searchParams.get("days") ?? 10));
        return Response.json({ days: empty ? days.map((d) => ({ ...d, readers: 0, anon: 0, free: 0, premium: 0, active_ms: 0 })) : days });
      }
      return Response.json({}, { status: 404 });
    },
  });
  return { client, requests };
}

function dashboard(s: ReturnType<typeof server>) {
  return render(
    <ContentKitProvider client={s.client} viewer="staff">
      <StatsDashboard kinds={["gallery"]} />
    </ContentKitProvider>,
  );
}

it("charts ten days of distinct visitors by audience with today's split and no period-unique total", async () => {
  const s = server();
  dashboard(s);
  const today = await screen.findByText("Today");
  const tiles = today.closest('[data-ckui="stats-today"]') as HTMLElement;
  for (const [name, value] of [["Visitors", "12"], ["Anon", "5"], ["Free", "4"], ["Premium", "3"]] as const) {
    expect(within(tiles).getByText(name).closest("div")).toHaveTextContent(`${name}${value}`);
  }
  const bars = document.querySelectorAll('[data-ckui="stats-day"]');
  expect(bars).toHaveLength(10);
  // Oldest to newest: today is the last bar.
  expect(bars[9]).toHaveAccessibleName(/Visitors 12, Anon 5, Free 4, Premium 3$/);
  expect(bars[8]).toHaveAccessibleName(/Visitors 10, Anon 6, Free 3, Premium 1$/);
  expect(bars[0]).toHaveAccessibleName(/Visitors 0, Anon 0, Free 0, Premium 0$/);
  expect(s.requests.every((u) => u.pathname.endsWith("/stats/activity") && u.searchParams.get("days") === "10")).toBe(true);
  expect(screen.queryByText("22")).toBeNull();
  expect(screen.queryByText(/views|sessions|top content/i)).toBeNull();

  const table = screen.getByRole("table", { hidden: true });
  expect(within(table).getByRole("row", { name: "2026-10-16 12 5 4 3 1:30", hidden: true })).toBeInTheDocument();
});

it("offers 10, 30 and 90 days and charts the chosen period", async () => {
  const s = server();
  dashboard(s);
  await screen.findByText("Today");
  const user = userEvent.setup();
  await user.click(screen.getByRole("combobox", { name: "Days" }));
  expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["Last 10 days", "Last 30 days", "Last 90 days"]);
  await user.click(screen.getByRole("option", { name: "Last 30 days" }));
  await waitFor(() => expect(document.querySelectorAll('[data-ckui="stats-day"]')).toHaveLength(30));
  expect(s.requests.some((u) => u.searchParams.get("days") === "30")).toBe(true);
});

it("shows an empty chart for a quiet period and the unavailable state without a signal plane", async () => {
  const quiet = server({ empty: true });
  const { unmount } = dashboard(quiet);
  expect(await screen.findByText("No measured activity in this period.")).toBeVisible();
  expect(document.querySelectorAll('[data-ckui="stats-day"]')).toHaveLength(10);
  unmount();

  dashboard(server({ unavailable: true }));
  expect(await screen.findByText("Measured engagement is not available on this site.")).toBeVisible();
  expect(document.querySelector('[data-ckui="stats-chart"]')).toBeNull();
});
