import { LockIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useContext, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { Decision, Terms, Version } from "../../client/generated/wire.js";
import type { DecisionOffer } from "../../client/offers.js";
import { useMessages } from "../../i18n/context.js";
import type { Translator } from "../../i18n/messages.js";
import { ContentKitContext } from "../../react/context.js";
import { offersOf } from "../../react/detail.js";
import { useKinds } from "../../react/groups.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { formatDate } from "../social/parts.js";
import { labelOf, useLanguageName, type Labels } from "../groups/parts.js";

/** An offer's price and period in words: "$2.99", "$4.99 / 30 days", "$1.99 for 48 hours". */
export function priceLabel(m: Translator, terms: Terms): string {
  let price: string;
  try {
    price = new Intl.NumberFormat(m.language, { style: "currency", currency: terms.currency }).format(Number(terms.amount));
  } catch {
    price = `${terms.amount} ${terms.currency}`;
  }
  const period = (hours: number) => (hours % 24 === 0 ? m.plural("paywall.days", hours / 24) : m.plural("paywall.hours", hours));
  if (terms.renew_hours) return m.t("paywall.renews", { price, period: period(terms.renew_hours) });
  if (terms.access_hours) return m.t("paywall.rents", { price, period: period(terms.access_hours) });
  return price;
}

export interface PaywallProps {
  /** The locked version's (or card's) decision. */
  decision: Decision;
  /**
   * Starts a purchase: the host creates the order through billing-ui
   * (createOrder with offer.price_id) and shows OrderCheckout; for a tier
   * (no offer), its subscription page.
   */
  onCheckout: (o: { key: string; offer?: DecisionOffer }) => void;
  /** Tiers' names and prices from the host's catalog, by tier key. */
  tiers?: Readonly<Record<string, { label?: string; price?: string }>>;
  /** Asks a signed-out reader to sign in (they may hold it already); default the provider's. */
  onSignIn?: () => void;
  labels?: Labels;
  /** Below the offers: the host's own. */
  children?: ReactNode;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Why content is locked and how to open it: its offers, the tiers that include it, previews and when it turns free. */
export function Paywall({ decision, onCheckout, tiers, onSignIn, labels, children, className, appearance }: PaywallProps) {
  const m = useMessages();
  const { t } = m;
  const ctx = useContext(ContentKitContext);
  const { kinds } = useKinds();
  if (decision.allowed) return null;
  const signIn = onSignIn ?? ctx?.onSignIn;
  const offers = offersOf(decision);
  const sold = new Set(offers.map((o) => o.key));
  const siteTiers = kinds?.tiers ?? Object.keys(tiers ?? {});
  const tierKeys = (decision.sell ?? []).filter((k) => !sold.has(k) && (siteTiers.includes(k) || !!tiers?.[k]));
  const requires = decision.requires && !offers.some((o) => o.key === decision.requires) ? decision.requires : undefined;
  const preview = decision.preview;
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-3 rounded-lg border border-border p-4 text-sm", className)} data-ckui="paywall">
      <p className="flex items-center gap-2 font-medium">
        <HugeiconsIcon icon={LockIcon} strokeWidth={2} className="size-4" aria-hidden />
        {decision.unknown ? t("paywall.unknown") : requires ? t("paywall.requiresFirst") : t("paywall.locked")}
      </p>
      {preview?.pages ? <p className="text-muted-foreground">{m.plural("paywall.previewPages", preview.pages)}</p> : null}
      {preview?.segments ? <p className="text-muted-foreground">{m.plural("paywall.previewSegments", preview.segments)}</p> : null}
      {decision.free_after && <p className="text-muted-foreground">{t("paywall.freeFrom", { date: formatDate(m, decision.free_after, false) })}</p>}
      {!decision.unknown && (
        <div className="flex flex-wrap gap-2" data-ckui="paywall-offers">
          {requires && (
            <Button size="sm" variant="outline" onClick={() => onCheckout({ key: requires })}>
              {t("paywall.joinFirst")}
            </Button>
          )}
          {offers.map((o) => (
            <Button key={o.key} size="sm" disabled={!!requires} onClick={() => onCheckout({ key: o.key, offer: o })} data-ckui="paywall-offer" data-target={o.target.kind}>
              {t(o.target.kind === "group" ? "paywall.joinFor" : o.target.kind === "version" ? "paywall.unlockVersionFor" : "paywall.unlockFor", { price: priceLabel(m, o.terms) })}
            </Button>
          ))}
          {tierKeys.map((k) => (
            <Button key={k} size="sm" variant={offers.length ? "outline" : "default"} onClick={() => onCheckout({ key: k })} data-ckui="paywall-tier" data-tier={k}>
              {t("paywall.subscribe", { tier: tiers?.[k]?.label ?? labelOf(labels, k) })}
              {tiers?.[k]?.price && <span className="ms-1 opacity-80">{tiers[k]!.price}</span>}
            </Button>
          ))}
        </div>
      )}
      {ctx?.viewer === null && signIn && (
        <p className="text-muted-foreground">
          {t("paywall.haveIt")}{" "}
          <button type="button" className="font-medium text-foreground underline-offset-4 hover:underline" onClick={signIn}>
            {t("paywall.signIn")}
          </button>
        </p>
      )}
      {children}
    </ContentKitUiRoot>
  );
}

export interface VersionPickerProps {
  /** The content's live versions (useContentDetail().versions). */
  versions: readonly Version[];
  /** The version shown. */
  value: string | null | undefined;
  onChange: (version: string) => void;
  labels?: Labels;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** A version's name: its label, else its traits, else its languages. */
function versionName(v: Version, languageName: (l: string) => string, labels?: Labels): string {
  return v.label || (v.traits.length ? v.traits.map((x) => labelOf(labels, x)).join(" · ") : v.languages.map(languageName).join(", "));
}

/** Chooses among a content's versions: a language first when they differ in language, then its variants (traits, labels). */
export function VersionPicker({ versions, value, onChange, labels, className, appearance }: VersionPickerProps) {
  const { t } = useMessages();
  const languageName = useLanguageName();
  if (versions.length < 2) return null;
  const current = versions.find((v) => v.id === value) ?? versions[0]!;
  const primary = (v: Version) => v.languages[0] ?? "";
  const languages = [...new Set(versions.map(primary))];
  const language = primary(current);
  const variants = versions.filter((v) => primary(v) === language);
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("flex flex-wrap items-center gap-2 text-sm", className)} data-ckui="version-picker">
      {languages.length > 1 && (
        <Select
          value={language}
          onValueChange={(l) => {
            const v = versions.find((x) => primary(x) === l);
            if (v) onChange(v.id);
          }}
        >
          <SelectTrigger size="sm" aria-label={t("versions.language")}>
            <SelectValue>{(l: string) => languageName(l)}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {languages.map((l) => (
              <SelectItem key={l} value={l}>
                {languageName(l)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
      {variants.length > 1 && (
        <div role="radiogroup" aria-label={t("versions.version")} className="flex flex-wrap gap-1">
          {variants.map((v) => (
            <Button
              key={v.id}
              size="sm"
              role="radio"
              aria-checked={v.id === current.id}
              variant={v.id === current.id ? "default" : "outline"}
              onClick={() => onChange(v.id)}
              data-ckui="version-option"
              data-locked={v.decision.allowed ? undefined : ""}
            >
              {!v.decision.allowed && <HugeiconsIcon icon={LockIcon} strokeWidth={2} className="size-3.5" aria-label={t("versions.locked")} />}
              {versionName(v, languageName, labels)}
            </Button>
          ))}
        </div>
      )}
    </ContentKitUiRoot>
  );
}
