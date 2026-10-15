import type { Item } from "../client/generated/wire.js";
import { useMessages } from "../i18n/context.js";
import type { Translator } from "../i18n/messages.js";

export type BillingNotificationAction = "update_payment_method" | "resubscribe" | "authenticate_payment" | "contact_support" | "manage_billing";
export type BillingNotificationActions = Partial<Record<BillingNotificationAction, string>>;

function dataOf(item: Item): Record<string, unknown> {
  return item.data && typeof item.data === "object" && !Array.isArray(item.data) ? item.data as Record<string, unknown> : {};
}

/** The selected billing lifecycle outcomes, rather than signup/cancel confirmations. */
function isPaymentFailure(kind: string): boolean {
  return ["payment_method_failed", "payment_method_update_required", "payment_authentication_required", "order_collection_stopped"].includes(kind);
}

export function isBillingNotification(item: Item): boolean {
  return isPaymentFailure(item.kind) || item.kind === "premium_ended" || (item.kind === "premium_renewed" && dataOf(item).downgrade_applied === true);
}

/** The immutable event's guidance; it is not a claim that a debt remains open. */
export function billingNotificationAction(item: Item): BillingNotificationAction {
  const action = dataOf(item).recovery_action ?? (item.kind === "payment_method_update_required" ? "update_payment_method" : item.kind === "payment_authentication_required" ? "authenticate_payment" : undefined);
  return action === "update_payment_method" || action === "resubscribe" || action === "authenticate_payment" || action === "contact_support" ? action : "manage_billing";
}

/** Shared billing wording. Hosts supply only their authorized destination URLs. */
export function BillingNotification({ item, actions = {} }: { item: Item; actions?: BillingNotificationActions }) {
  const translator = useMessages();
  const { t } = translator;
  if (!isBillingNotification(item)) return null;
  const data = dataOf(item);
  const title = item.kind === "payment_method_update_required" ? t("billingNotifications.paymentMethodRequiredTitle")
    : item.kind === "payment_authentication_required" ? t("billingNotifications.authenticationRequiredTitle")
    : item.kind === "order_collection_stopped" ? t("billingNotifications.collectionStoppedTitle")
    : isPaymentFailure(item.kind) ? t("billingNotifications.paymentFailed")
    : item.kind === "premium_ended" ? t("billingNotifications.membershipEnded") : t("billingNotifications.membershipDowngraded");
  let detail: string;
  if (isPaymentFailure(item.kind)) {
    const attempt = date(data.next_attempt_at, translator);
    detail = data.retry_status === "scheduled" && attempt ? t("billingNotifications.retryScheduled", { date: attempt })
      : data.retry_status === "payment_method_required" ? t("billingNotifications.paymentMethodRequired")
      : data.retry_status === "stopped" ? t("billingNotifications.retryStopped")
      : t("billingNotifications.retryUnknown");
  } else if (item.kind === "premium_ended") {
    const ended = date(data.ended_at, translator);
    detail = ended ? t("billingNotifications.endedAt", { date: ended }) : t("billingNotifications.ended");
  } else {
    detail = typeof data.from_product_name === "string" && typeof data.to_product_name === "string" && data.from_product_name && data.to_product_name
      ? t("billingNotifications.downgradedTo", { from: data.from_product_name, to: data.to_product_name })
      : t("billingNotifications.downgraded");
  }
  const action = billingNotificationAction(item);
  const href = actions[action] ?? actions.manage_billing;
  const label = actions[action] ? action : "manage_billing";
  return <div className="grid gap-2" data-ckui="billing-notification">
    <p className="font-medium">{title}</p>
    <p className="text-muted-foreground">{detail}</p>
    {isPaymentFailure(item.kind) && <p className="text-muted-foreground">{t(`billingNotifications.instructions.${action}`)}</p>}
    {href && <a href={href} className="font-medium underline underline-offset-4">{t(`billingNotifications.actions.${label}`)}</a>}
  </div>;
}

function date(value: unknown, translator: Translator): string | undefined {
  if (typeof value !== "string" || !Number.isFinite(Date.parse(value))) return undefined;
  return new Intl.DateTimeFormat(translator.language, { dateStyle: "medium", timeStyle: "short" }).format(new Date(value));
}
