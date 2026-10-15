// @vitest-environment jsdom
import "../test/dom.js";
import { render, screen, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { Item } from "../client/generated/wire.js";
import { BillingNotification, isBillingNotification } from "./billing-notification.js";

afterEach(cleanup);
const notice = (kind: string, data: Record<string, unknown> = {}): Item => ({ id: "n", recipient: { audience: "user", id: "u" }, origin: { system: "openrails", scope: "merchant", id: "event" }, kind, data, created_at: "2026-10-14T12:00:00Z", updated_at: "2026-10-14T12:00:00Z", revision: 1, read_at: null, dismissed_at: null, resolved_at: null });

describe("billing notification guidance", () => {
  it("explains a historical scheduled retry and provides a host action", () => {
    render(<BillingNotification item={notice("payment_method_failed", { retry_status: "scheduled", next_attempt_at: "2026-10-15T12:00:00Z", recovery_action: "update_payment_method" })} actions={{ update_payment_method: "/account/billing" }} />);
    expect(screen.getByText(/When this notice was created/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Update payment method" }).getAttribute("href")).toBe("/account/billing");
  });
  it("explains stopped retries and never invents a payment URL", () => {
    render(<BillingNotification item={notice("payment_method_failed", { retry_status: "stopped", recovery_action: "contact_support" })} />);
    expect(screen.getByText("No further automatic payment attempt was scheduled.")).toBeTruthy();
    expect(screen.getByText("Contact support for help with this payment.")).toBeTruthy();
    expect(screen.queryByRole("link")).toBeNull();
  });
  it("renders applied downgrades but not routine renewals", () => {
    expect(isBillingNotification(notice("premium_renewed"))).toBe(false);
    render(<BillingNotification item={notice("premium_renewed", { downgrade_applied: true, from_product_name: "Plus", to_product_name: "Basic" })} />);
    expect(screen.getByText("Your membership changed from Plus to Basic.")).toBeTruthy();
  });
  it("falls back safely when retry information is absent or malformed", () => {
    render(<BillingNotification item={notice("payment_method_failed", { retry_status: "scheduled", next_attempt_at: "invalid" })} actions={{ manage_billing: "/billing" }} />);
    expect(screen.getByText("No retry schedule was available when this notice was created.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "View billing" })).toBeTruthy();
  });
  it.each([
    ["payment_method_update_required", "Update payment method"],
    ["payment_authentication_required", "Payment authentication required"],
    ["order_collection_stopped", "Automatic payments stopped"],
  ])("distinguishes %s from the initial failure", (kind, title) => {
    render(<BillingNotification item={notice(kind)} />);
    expect(screen.getByText(title)).toBeTruthy();
  });

});
