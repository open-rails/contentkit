import { useContext, useId, useMemo } from "react";
import type { Offer, Terms } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { useDraft } from "../../react/use-draft.js";
import { EditorSaveContext, useEditorSaveTask } from "./editor-save.js";
import { priceLabel } from "./paywall.js";

type Mode = "once" | "rent" | "renew";

/** An offer's form: price, currency and whether it rents or renews; the current offer with its withdrawal. */
export function OfferFields({ offer, onSell, onWithdraw, pending }: { offer?: Pick<Offer, "terms"> & Partial<Pick<Offer, "state" | "synced">>; onSell: (t: Terms) => unknown | Promise<unknown>; onWithdraw: () => void; pending: boolean }) {
  const m = useMessages();
  const { t } = m;
  const coordinated = useContext(EditorSaveContext) !== null;
  const id = useId();
  const active = offer && (offer.state ?? "active") === "active" ? offer : undefined;
  const fields=useDraft(useMemo(()=>({amount:active?.terms.amount??"",currency:active?.terms.currency??"USD",mode:(active?.terms.renew_hours?"renew":active?.terms.access_hours?"rent":"once")as Mode,days:String(((active?.terms.renew_hours??active?.terms.access_hours??720)/24)|0)}),[active?.terms]));
  const {amount,currency,mode,days}=fields.value;
  const edit=(patch:Partial<typeof fields.value>)=>fields.edit((old)=>({...old,...patch}));
  const valid=/^\d+(\.\d{1,2})?$/.test(amount.trim())&&currency.length===3&&(mode==="once"||Number(days)>0);
  const submit=async()=>{
    if(!valid)return false;
    const settle=fields.checkpoint();
    const hours=Math.max(1,Math.trunc(Number(days)||0))*24;
    const terms:Terms={amount:amount.trim(),currency,...(mode==="rent"?{access_hours:hours}:mode==="renew"?{renew_hours:hours}:{})};
    const out=await onSell(terms);
    if(!out)return false;
    return settle({amount:terms.amount,currency:terms.currency,mode,days});
  };
  useEditorSaveTask(`offer:${id}`,{label:t("contentEditor.price"),dirty:fields.dirty.length>0,pending,valid,save:submit,order:40});
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
          <Input id={`${id}-amount`} className="h-8 w-28" inputMode="decimal" value={amount} placeholder="4.99" onChange={(e) => edit({amount:e.target.value})} />
        </div>
        <div className="grid gap-1">
          <Label htmlFor={`${id}-currency`} className="text-xs text-muted-foreground">
            {t("contentEditor.currency")}
          </Label>
          <Input id={`${id}-currency`} className="h-8 w-20 uppercase" maxLength={3} value={currency} onChange={(e) => edit({currency:e.target.value.toUpperCase()})} />
        </div>
        <Select value={mode} onValueChange={(v) => edit({mode:v as Mode})}>
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
            <Input id={`${id}-days`} className="h-8 w-20" type="number" min={1} value={days} onChange={(e) => edit({days:e.target.value})} />
          </div>
        )}
        {!coordinated && <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={pending || !valid}
          onClick={()=>void submit()}
        >
          {t(active ? "contentEditor.reprice" : "contentEditor.sell")}
        </Button>}
      </div>
    </div>
  );
}

