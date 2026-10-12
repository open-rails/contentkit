import { useId, useState } from "react";
import type { Offer, Terms } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { priceLabel } from "./paywall.js";

type Mode = "once" | "rent" | "renew";

/** An offer's form: price, currency and whether it rents or renews; the current offer with its withdrawal. */
export function OfferFields({ offer, onSell, onWithdraw, pending }: { offer?: Pick<Offer, "terms"> & Partial<Pick<Offer, "state" | "synced">>; onSell: (t: Terms) => void; onWithdraw: () => void; pending: boolean }) {
  const m = useMessages();
  const { t } = m;
  const id = useId();
  const active = offer && (offer.state ?? "active") === "active" ? offer : undefined;
  const [amount, setAmount] = useState(active?.terms.amount ?? "");
  const [currency, setCurrency] = useState(active?.terms.currency ?? "USD");
  const [mode, setMode] = useState<Mode>(active?.terms.renew_hours ? "renew" : active?.terms.access_hours ? "rent" : "once");
  const [days, setDays] = useState(String(((active?.terms.renew_hours ?? active?.terms.access_hours ?? 720) / 24) | 0));
  return (
    <div className="grid gap-2" data-ckui="offer-fields">
      {active ? (
        <p className="flex flex-wrap items-center gap-2" data-ckui="current-offer">
          <Badge variant="secondary">{t("contentEditor.onSale")}</Badge>
          <span>{priceLabel(m, active.terms)}</span>
          {active.synced === false && <span className="text-xs text-muted-foreground">{t("contentEditor.syncing")}</span>}
          <Button type="button" size="xs" variant="ghost" disabled={pending} onClick={onWithdraw}>
            {t("contentEditor.withdraw")}
          </Button>
        </p>
      ) : (
        <p className="text-muted-foreground">{t("contentEditor.notForSale")}</p>
      )}
      <div className="flex flex-wrap items-end gap-2">
        <div className="grid gap-1">
          <Label htmlFor={`${id}-amount`} className="text-xs text-muted-foreground">
            {t("contentEditor.price")}
          </Label>
          <Input id={`${id}-amount`} className="h-8 w-28" inputMode="decimal" value={amount} placeholder="4.99" onChange={(e) => setAmount(e.target.value)} />
        </div>
        <div className="grid gap-1">
          <Label htmlFor={`${id}-currency`} className="text-xs text-muted-foreground">
            {t("contentEditor.currency")}
          </Label>
          <Input id={`${id}-currency`} className="h-8 w-20 uppercase" maxLength={3} value={currency} onChange={(e) => setCurrency(e.target.value.toUpperCase())} />
        </div>
        <Select value={mode} onValueChange={(v) => setMode(v as Mode)}>
          <SelectTrigger size="sm" aria-label={t("contentEditor.priceMode")}>
            <SelectValue>{(v: string) => t(`contentEditor.mode_${v as Mode}`)}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {(["once", "rent", "renew"] as const).map((x) => (
              <SelectItem key={x} value={x}>
                {t(`contentEditor.mode_${x}`)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {mode !== "once" && (
          <div className="grid gap-1">
            <Label htmlFor={`${id}-days`} className="text-xs text-muted-foreground">
              {t("contentEditor.days")}
            </Label>
            <Input id={`${id}-days`} className="h-8 w-20" type="number" min={1} value={days} onChange={(e) => setDays(e.target.value)} />
          </div>
        )}
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={pending || !/^\d+(\.\d{1,2})?$/.test(amount.trim()) || currency.length !== 3}
          onClick={() => {
            const hours = Math.max(1, Math.trunc(Number(days) || 0)) * 24;
            onSell({ amount: amount.trim(), currency, access_hours: mode === "rent" ? hours : undefined, renew_hours: mode === "renew" ? hours : undefined });
          }}
        >
          {t(active ? "contentEditor.reprice" : "contentEditor.sell")}
        </Button>
      </div>
    </div>
  );
}

