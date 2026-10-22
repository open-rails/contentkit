// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, within } from "@testing-library/react";
import { expect, it } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { StatsDashboard, type ActivitySeries } from "./stats-dashboard.js";

const groups: ActivitySeries[] = [
  { id: "guest", label: "Guests", color: "#123456" },
  { id: "member", label: "Members", color: "#654321" },
  { id: "patron", label: "Patrons", color: "#abcdef" },
];
const active: Record<
  string,
  { cohorts: Record<string, number>; active_ms: number }
> = {
  "2026-10-16": {
    cohorts: { guest: 5, member: 4, patron: 3 },
    active_ms: 90_000,
  },
  "2026-10-15": {
    cohorts: { guest: 6, member: 3, patron: 1 },
    active_ms: 30_000,
  },
};

function series(days: number) {
  return Array.from({ length: days }, (_, i) => {
    const date = new Date(Date.UTC(2026, 9, 16 - i)).toISOString().slice(0, 10);
    const d = active[date] ?? {
      cohorts: { guest: 0, member: 0, patron: 0 },
      active_ms: 0,
    };
    return {
      date,
      readers: Object.values(d.cohorts).reduce((sum, n) => sum + n, 0),
      ...d,
    };
  });
}

function server({
  unavailable = false,
  empty = false,
  totalOnly = false,
} = {}) {
  const requests: URL[] = [];
  const client = createContentKitClient({
    baseUrl: "https://stats.test",
    fetch: async (input) => {
      const url = new URL(String(input));
      requests.push(url);
      if (unavailable)
        return Response.json(
          {
            code: "not_configured",
            error: "Measured engagement is unavailable",
          },
          { status: 503 },
        );
      if (url.pathname.endsWith("/activity")) {
        const days = series(Number(url.searchParams.get("days") ?? 10)).map(
          (d) =>
            empty
              ? {
                  ...d,
                  readers: 0,
                  cohorts: { guest: 0, member: 0, patron: 0 },
                  active_ms: 0,
                }
              : d,
        );
        return Response.json({
          cohorts: totalOnly ? [] : ["guest", "member", "patron"],
          days: days.map((d) => (totalOnly ? { ...d, cohorts: {} } : d)),
        });
      }
      return Response.json({}, { status: 404 });
    },
  });
  return { client, requests };
}

function dashboard(s: ReturnType<typeof server>, days?: number) {
  return render(
    <ContentKitProvider client={s.client} viewer="staff">
      <StatsDashboard kinds={["gallery"]} series={groups} days={days} />
    </ContentKitProvider>,
  );
}

it("charts ten daily points on three host-defined lines, including zero days", async () => {
  const s = server();
  dashboard(s);
  const today = await screen.findByText("Today");
  const tiles = today.closest('[data-ckui="stats-today"]') as HTMLElement;
  for (const [name, count] of [
    ["Readers", "12"],
    ["Guests", "5"],
    ["Members", "4"],
    ["Patrons", "3"],
  ] as const) {
    expect(within(tiles).getByText(name).closest("div")).toHaveTextContent(
      `${name}${count}`,
    );
  }
  const points = document.querySelectorAll('[data-ckui="stats-day"]');
  expect(points).toHaveLength(10);
  expect(points[9]).toHaveAccessibleName(
    /Readers 12, Guests 5, Members 4, Patrons 3$/,
  );
  expect(points[8]).toHaveAccessibleName(
    /Readers 10, Guests 6, Members 3, Patrons 1$/,
  );
  expect(points[0]).toHaveAccessibleName(
    /Readers 0, Guests 0, Members 0, Patrons 0$/,
  );
  const lines = document.querySelectorAll('polyline[data-ckui="stats-line"]');
  expect(lines).toHaveLength(3);
  expect([...lines].map((line) => line.getAttribute("stroke"))).toEqual(
    groups.map((g) => g.color),
  );
  for (const line of lines)
    expect(line.getAttribute("points")?.split(" ")).toHaveLength(10);
  expect(
    s.requests.every(
      (u) =>
        u.pathname.endsWith("/stats/activity") &&
        u.searchParams.get("days") === "10",
    ),
  ).toBe(true);
  expect(screen.queryByRole("combobox", { name: "Days" })).toBeNull();
  expect(
    screen.queryByText(/premium|anon|free|views|sessions|top content/i),
  ).toBeNull();
  const table = screen.getByRole("table", { hidden: true });
  expect(
    within(table).getByRole("row", {
      name: "2026-10-16 12 5 4 3 1:30",
      hidden: true,
    }),
  ).toBeInTheDocument();
});

it("lets a host choose another period without imposing a built-in period picker", async () => {
  const s = server();
  dashboard(s, 30);
  await screen.findByText("Today");
  expect(document.querySelectorAll('[data-ckui="stats-day"]')).toHaveLength(30);
  expect(s.requests.some((u) => u.searchParams.get("days") === "30")).toBe(
    true,
  );
});

it("renders totals without inventing cohorts when the host defines none", async () => {
  dashboard(server({ totalOnly: true }));
  await screen.findByText("Today");
  expect(document.querySelectorAll('[data-ckui="stats-line"]')).toHaveLength(1);
  expect(screen.queryByText("Patrons")).toBeNull();
});

it("shows empty and unavailable states", async () => {
  const { unmount } = dashboard(server({ empty: true }));
  expect(
    await screen.findByText("No measured activity in this period."),
  ).toBeVisible();
  expect(document.querySelectorAll('[data-ckui="stats-day"]')).toHaveLength(10);
  unmount();
  dashboard(server({ unavailable: true }));
  expect(
    await screen.findByText(
      "Measured engagement is not available on this site.",
    ),
  ).toBeVisible();
  expect(document.querySelector('[data-ckui="stats-chart"]')).toBeNull();
});
