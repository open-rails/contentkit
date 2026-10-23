import { LockIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useCallback, useContext, useMemo, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { Decision, Sale, Terms, Version } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import type { Translator } from "../../i18n/messages.js";
import { ContentKitContext, useContentKitClient, type ContentKitErrorHandler } from "../../react/context.js";
import { editionOf, findVersion } from "../../react/detail.js";
import { useVersionParam } from "../../react/location.js";
import { useKinds } from "../../react/groups.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { contentError, ErrorLine, formatDate, useAction } from "../social/parts.js";
import { labelOf, type Labels } from "../groups/parts.js";
import type { PlayerMenuItem } from "../video-player.js";

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
   * (no offer), its subscription page. A free group offer joins here instead.
   */
  onCheckout: (o: { key: string; offer?: Sale }) => void;
  /** Tiers' names and prices from the host's catalog, by tier key. */
  tiers?: Readonly<Record<string, { label?: string; price?: string }>>;
  /** Asks a signed-out reader to sign in (they may hold it already); default the provider's. */
  onSignIn?: () => void;
  labels?: Labels;
  /** Below the offers: the host's own. */
  children?: ReactNode;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Why content is locked and how to open it: its offers, the tiers that include it, previews and when it turns free. */
export function Paywall({ decision, onCheckout, tiers, onSignIn, labels, children, client: own, onError, className, appearance }: PaywallProps) {
  const m = useMessages();
  const { t } = m;
  const ctx = useContext(ContentKitContext);
  const client = useContentKitClient(own);
  const { kinds } = useKinds({ client: own });
  const join = useAction("membership.join", onError);
  if (decision.allowed) return null;
  const signIn = onSignIn ?? ctx?.onSignIn;
  const offers = decision.offers ?? [];
  const sold = new Set(offers.map((o) => o.key));
  const siteTiers = kinds?.tiers ?? Object.keys(tiers ?? {});
  const tierKeys = (decision.sell ?? []).filter((k) => !sold.has(k) && (siteTiers.includes(k) || !!tiers?.[k]));
  const required = decision.requires;
  // A required key nothing here offers gets its own button; the others wait for it.
  const requires = required && !offers.some((o) => o.key === required) ? required : undefined;
  const waits = (key: string) => !!required && key !== required;
  const preview = decision.preview;
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-3 rounded-lg border border-border p-4 text-sm", className)} data-ckui="paywall">
      <p className="flex items-center gap-2 font-medium">
        <HugeiconsIcon icon={LockIcon} strokeWidth={2} className="size-4" aria-hidden />
        {decision.unknown ? t("paywall.unknown") : required ? t("paywall.requiresFirst") : t("paywall.locked")}
      </p>
      {preview?.pages ? <p className="text-muted-foreground">{m.plural("paywall.previewPages", preview.pages)}</p> : null}
      {preview?.segments ? <p className="text-muted-foreground">{m.plural("paywall.previewSegments", preview.segments)}</p> : null}
      
      {!decision.unknown && (
        <div className="flex flex-wrap gap-2" data-ckui="paywall-offers">
          {requires && (
            <Button size="sm" variant="outline" onClick={() => onCheckout({ key: requires })}>
              {t("paywall.joinFirst")}
            </Button>
          )}
          {offers.map((o) =>
            o.target.kind === "group" && Number(o.terms.amount) === 0 ? (
              <Button
                key={o.key}
                size="sm"
                disabled={waits(o.key) || join.pending}
                onClick={() => (ctx?.viewer === null && signIn ? signIn() : void join.run(() => client.offers.join(o.target.id)))}
                data-ckui="paywall-offer"
                data-target="group"
                data-free=""
              >
                {t("paywall.joinFirst")}
              </Button>
            ) : (
              <Button key={o.key} size="sm" disabled={waits(o.key)} onClick={() => onCheckout({ key: o.key, offer: o })} data-ckui="paywall-offer" data-target={o.target.kind}>
                {t(o.target.kind === "group" ? "paywall.joinFor" : o.target.kind === "version" ? "paywall.unlockVersionFor" : "paywall.unlockFor", { price: priceLabel(m, o.terms) })}
              </Button>
            ),
          )}
          {tierKeys.map((k) => (
            <Button key={k} size="sm" variant={offers.length ? "outline" : "default"} onClick={() => onCheckout({ key: k })} data-ckui="paywall-tier" data-tier={k}>
              {t("paywall.subscribe", { tier: tiers?.[k]?.label ?? labelOf(labels, k) })}
              {tiers?.[k]?.price && <span className="ms-1 opacity-80">{tiers[k]!.price}</span>}
            </Button>
          ))}
        </div>
      )}
      {join.error && <ErrorLine>{contentError(m, join.error)}</ErrorLine>}
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
  /** The version shown, by code or id; default the page's ?v=, else the reader's language's default. */
  value?: string | null;
  /** Receives the chosen version; default writes its code to the page's ?v=. */
  onChange?: (version: Version) => void;
  labels?: Labels;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** A version's name: its label, else its traits, else its languages. */
function versionName(v: Version, languageName: (l: string) => string, labels?: Labels): string {
  return v.label || (v.traits.length ? v.traits.map((x) => labelOf(labels, x)).join(" · ") : v.languages.map(languageName).join(", "));
}

const primary = (v: Version) => v.languages[0] ?? "";

/**
 * The shown version (value, else the page's ?v=, else the reader's
 * language's default), the versions' languages, the shown language's
 * variants, and the change handler (default: write ?v=).
 */
function useChoices(versions: readonly Version[], value: string | null | undefined, onChange: ((v: Version) => void) | undefined) {
  const [param, setParam] = useVersionParam();
  const reader = useContext(ContentKitContext)?.language;
  const selected = value === undefined ? param : value;
  const change = useCallback((v: Version) => (onChange ? onChange(v) : setParam(v.code)), [onChange, setParam]);
  return useMemo(() => {
    const sorted = [...versions].sort((a, b) => a.position - b.position);
    const current = findVersion(versions, selected) ?? editionOf(sorted, reader ?? undefined);
    const language = current ? primary(current) : "";
    return { current, language, languages: [...new Set(sorted.map(primary))], variants: sorted.filter((v) => primary(v) === language), change };
  }, [versions, selected, reader, change]);
}

export interface LanguageFallbackNoticeProps extends Omit<VersionPickerProps, "labels"> {
  /** The page's language: the notice shows when the shown version doesn't serve it. */
  language: string;
}

/**
 * A quiet note that the content isn't available in the page's language, which
 * version it shows instead, and each language it does serve to switch to.
 */
export function LanguageFallbackNotice({ versions, value, onChange, language, className, appearance }: LanguageFallbackNoticeProps) {
  const { t } = useMessages();
  const languageName = useLanguageName();
  const { current, languages, change } = useChoices(versions, value, onChange);
  if (!current || !language || current.languages.includes(language)) return null;
  const sorted = [...versions].sort((a, b) => a.position - b.position);
  return (
    <ContentKitUiRoot appearance={appearance} role="note" className={cn("flex flex-wrap items-center gap-2 text-sm text-muted-foreground", className)} data-ckui="language-fallback">
      <span>{t("contents.notInLanguage", { language: languageName(language), shown: languageName(primary(current)) })}</span>
      {languages.length > 1 &&
        languages.map((l) => {
          const v = sorted.find((x) => primary(x) === l)!;
          return (
            <Button key={l} size="xs" variant={l === primary(current) ? "secondary" : "outline"} aria-pressed={l === primary(current)} onClick={() => change(v)}>
              {languageName(l)}
            </Button>
          );
        })}
    </ContentKitUiRoot>
  );
}

/**
 * Chooses among a content's versions: a language first when they differ in
 * language, then its variants (traits, labels). The choice is the page's ?v=
 * unless value and onChange say otherwise.
 */
export function VersionPicker({ versions, value, onChange, labels, className, appearance }: VersionPickerProps) {
  const { t } = useMessages();
  const languageName = useLanguageName();
  const { current, language, languages, variants, change } = useChoices(versions, value, onChange);
  if (versions.length < 2 || !current) return null;
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("flex flex-wrap items-center gap-2 text-sm", className)} data-ckui="version-picker">
      {languages.length > 1 && (
        <Select
          value={language}
          onValueChange={(l) => {
            const v = [...versions].sort((a, b) => a.position - b.position).find((x) => primary(x) === l);
            if (v) change(v);
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
              onClick={() => change(v)}
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

export interface VersionMenuOptions {
  /** The version shown, by code or id; default the page's ?v=, else the reader's language's default. */
  value?: string | null;
  /** Receives the chosen version; default writes its code to the page's ?v=. */
  onChange?: (version: Version) => void;
  labels?: Labels;
}

/** VersionPicker as VideoPlayer settings entries (menuItems), for a watch page. */
export function useVersionMenuItems(versions: readonly Version[], o: VersionMenuOptions = {}): PlayerMenuItem[] {
  const { t } = useMessages();
  const languageName = useLanguageName();
  const { current, language, languages, variants, change } = useChoices(versions, o.value, o.onChange);
  const { labels } = o;
  return useMemo(() => {
    if (versions.length < 2 || !current) return [];
    const out: PlayerMenuItem[] = [];
    if (languages.length > 1)
      out.push({
        label: t("versions.language"),
        value: language,
        options: languages.map((l) => ({ value: l, label: languageName(l) })),
        onChange: (l) => {
          const v = versions.find((x) => primary(x) === l);
          if (v) change(v);
        },
      });
    if (variants.length > 1)
      out.push({
        label: t("versions.version"),
        value: current.id,
        options: variants.map((v) => {
          const name = versionName(v, languageName, labels);
          return { value: v.id, label: v.decision.allowed ? name : t("versions.lockedOption", { name }) };
        }),
        onChange: (id) => {
          const v = versions.find((x) => x.id === id);
          if (v) change(v);
        },
      });
    return out;
  }, [versions, current, language, languages, variants, change, labels, t, languageName]);
}
