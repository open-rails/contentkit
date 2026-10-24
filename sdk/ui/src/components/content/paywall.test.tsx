// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import { ContentKitProvider } from "../../react/index.js";
import type { Decision } from "../../client/generated/wire.js";
import { Paywall } from "./paywall.js";

function show(decision: Decision) {
  const requests: string[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    requests.push(String(input));
    return Response.json({ languages: ["en"], tiers: ["premium"], signals: false, kinds: {}, entity_kinds: {} });
  });
  const client = createContentKitClient({ baseUrl: "http://localhost", fetch });
  const checkout = vi.fn();
  const result = render(
    <ContentKitProvider client={client} viewer="reader">
      <Paywall decision={decision} onCheckout={checkout} tiers={{ premium: { label: "Premium", price: "$10" } }} />
    </ContentKitProvider>,
  );
  return { ...result, checkout, requests };
}

it("passes only an entitlement key to host checkout and keeps tier display host-owned", async () => {
  const user = userEvent.setup();
  const { checkout, requests } = show({ allowed: false, sell: ["premium", "t:content:c1"] });
  await user.click(screen.getByRole("button", { name: "Get Premium $10" }));
  await user.click(screen.getByRole("button", { name: "Get access" }));
  expect(checkout.mock.calls).toEqual([[{ key: "premium" }], [{ key: "t:content:c1" }]]);
  expect(requests.every((url) => new URL(url).pathname === "/v1/kinds")).toBe(true);
});

it("deduplicates a required key and disables other acquisition actions until it is held", async () => {
  const user = userEvent.setup();
  const { checkout } = show({ allowed: false, requires: "premium", sell: ["premium", "t:content:c1"] });
  const required = screen.getByRole("button", { name: "Get Premium $10" });
  expect(screen.getAllByRole("button")).toHaveLength(2);
  expect(screen.getByRole("button", { name: "Get access" })).toBeDisabled();
  await user.click(required);
  expect(checkout).toHaveBeenCalledExactlyOnceWith({ key: "premium" });
});

it("shows no checkout action when entitlement checks are unavailable", () => {
  show({ allowed: false, unknown: true, sell: ["premium"] });
  expect(screen.queryByRole("button")).toBeNull();
  expect(screen.getByText("We can't check your access right now. Try again shortly.")).toBeInTheDocument();
});
