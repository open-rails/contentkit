import { Add01Icon, Cancel01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useId, useMemo, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentKitError } from "../../client/errors.js";
import type { LocationCheck, LocationEntry, LocationRule, LocationRuleHistory, LocationRuleInput, LocationRuleTarget } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import type { Translator } from "../../i18n/messages.js";
import { useFlowClient, useStaffAccess, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { useLocationRules } from "../../react/comments.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "#ckui/ui/dialog";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Switch } from "#ckui/ui/switch";
import { Textarea } from "#ckui/ui/textarea";
import { ErrorLine, formatDate, useAction } from "../social/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Rows, Segmented, useConfirm, type AdminFlowProps } from "./shell.js";
import { PageHeading } from "../page-heading.js";

export type LocationRuleManagerProps = AdminFlowProps;

/** Whether the caller may manage location rules (root:locations:write). */
export function useLocationRuleManagerAccess(o: FlowClientOptions = {}): StaffAccess<"locations"> {
  return useStaffAccess(["locations"] as const, o);
}

/**
 * Staff: the rules that hide the whole site, some contents or every content
 * with some tags from visitors in a country, a US state or an address range,
 * on chosen domains. Rules switch on and off, apply to the next request, and
 * keep a history of who changed them. Check shows where an address resolves
 * and which rules cover it.
 */
export function LocationRuleManager({ client: own, baseUrl, auth, guard, appearance, className, language, messages, onError }: LocationRuleManagerProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="location-rules" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Rules client={client} onError={onError} />
    </AdminRoot>
  );
}

type Target = "site" | "content" | "entities";
type EntryType = "country" | "subdivision" | "network";
/** A picked content or entity, with what to call it. */
type Picked = { id: string; label: string };

// ISO 3166-2 codes of the US states and DC, where most age-verification laws are.
const US_STATES: readonly [string, string][] = [
  ["AL", "Alabama"], ["AK", "Alaska"], ["AZ", "Arizona"], ["AR", "Arkansas"], ["CA", "California"], ["CO", "Colorado"], ["CT", "Connecticut"],
  ["DE", "Delaware"], ["DC", "District of Columbia"], ["FL", "Florida"], ["GA", "Georgia"], ["HI", "Hawaii"], ["ID", "Idaho"], ["IL", "Illinois"],
  ["IN", "Indiana"], ["IA", "Iowa"], ["KS", "Kansas"], ["KY", "Kentucky"], ["LA", "Louisiana"], ["ME", "Maine"], ["MD", "Maryland"],
  ["MA", "Massachusetts"], ["MI", "Michigan"], ["MN", "Minnesota"], ["MS", "Mississippi"], ["MO", "Missouri"], ["MT", "Montana"], ["NE", "Nebraska"],
  ["NV", "Nevada"], ["NH", "New Hampshire"], ["NJ", "New Jersey"], ["NM", "New Mexico"], ["NY", "New York"], ["NC", "North Carolina"],
  ["ND", "North Dakota"], ["OH", "Ohio"], ["OK", "Oklahoma"], ["OR", "Oregon"], ["PA", "Pennsylvania"], ["RI", "Rhode Island"],
  ["SC", "South Carolina"], ["SD", "South Dakota"], ["TN", "Tennessee"], ["TX", "Texas"], ["UT", "Utah"], ["VT", "Vermont"], ["VA", "Virginia"],
  ["WA", "Washington"], ["WV", "West Virginia"], ["WI", "Wisconsin"], ["WY", "Wyoming"],
];
const STATE_NAMES = new Map(US_STATES.map(([code, name]) => [`US-${code}`, name]));
const ACTIONS = {
  create: "locationRules.action_create",
  update: "locationRules.action_update",
  enable: "locationRules.action_enable",
  disable: "locationRules.action_disable",
  delete: "locationRules.action_delete",
} as const;

function useCountryName(): (code: string) => string {
  const { language } = useMessages();
  return useMemo(() => {
    let names: Intl.DisplayNames | undefined;
    try {
      names = new Intl.DisplayNames(language ? [language] : undefined, { type: "region" });
    } catch {
      names = undefined;
    }
    return (code: string) => {
      try {
        return names?.of(code) ?? code;
      } catch {
        return code;
      }
    };
  }, [language]);
}

/** "Texas, United States", "Germany" or the address range as written. */
function useEntryName(): (e: LocationEntry) => string {
  const country = useCountryName();
  return (e) => {
    if (e.country) return country(e.country);
    if (e.subdivision) return `${STATE_NAMES.get(e.subdivision) ?? e.subdivision}, ${country(e.subdivision.slice(0, 2))}`;
    return e.network ?? "";
  };
}

const entryKey = (e: LocationEntry) => e.country ?? e.subdivision ?? e.network ?? "";
/** A target's title or name, else its id. */
const targetLabel = (t: LocationRuleTarget) => t.title || t.name || t.id;

function Rules({ client, onError }: { client: ContentKitClient; onError?: AdminFlowProps["onError"] }) {
  const m = useMessages();
  const { t } = m;
  const access = useLocationRuleManagerAccess({ client });
  const rules = useLocationRules({ client, enabled: !access.loading && access.allowed.locations });
  const save = useAction("rules.save", onError);
  const { confirm, dialog } = useConfirm();
  const entryName = useEntryName();
  const [editing, setEditing] = useState<LocationRule | "new" | null>(null);
  const [history, setHistory] = useState<LocationRule | null>(null);
  if (access.loading) return <Rows />;
  if (!access.allowed.locations) return <NoAccess>{t("locationRules.noAccess")}</NoAccess>;

  const remove = async (r: LocationRule) => {
    if (await confirm({ title: t("locationRules.deleteTitle"), description: t("locationRules.deleteBody"), confirm: t("locationRules.delete") })) {
      await save.run(() => rules.remove(r.id));
    }
  };
  return (
    <div className="grid gap-6 text-sm" data-ckui="location-rules">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="grid max-w-prose gap-1">
          <PageHeading className="text-xl font-semibold tracking-tight">{t("locationRules.title")}</PageHeading>
          <p className="text-muted-foreground">{t("locationRules.intro")}</p>
        </div>
        <Button size="sm" disabled={rules.loading} onClick={() => setEditing("new")}>
          <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
          {t("locationRules.new")}
        </Button>
      </div>
      <CheckAddress client={client} />
      <ErrorLine error={save.error} />
      {rules.loading && !rules.rules.length ? (
        <Rows />
      ) : rules.error && !rules.rules.length ? (
        <LoadFailed error={rules.error} message={t("locationRules.loadFailed")} retry={rules.reload} />
      ) : !rules.rules.length ? (
        <p className="text-muted-foreground">{t("locationRules.empty")}</p>
      ) : (
        <>
          <ul className="grid divide-y divide-border rounded-lg border border-border" aria-busy={rules.loading}>
            {rules.rules.map((r) => (
              <li key={r.id} className="grid gap-2 p-3 sm:grid-cols-[auto_minmax(0,1fr)_auto] sm:items-start sm:gap-3" data-ckui="location-rule" data-enabled={r.enabled}>
                <Switch
                  checked={r.enabled}
                  disabled={save.pending}
                  aria-label={t(r.enabled ? "locationRules.switchOff" : "locationRules.switchOn")}
                  onCheckedChange={(on) => void save.run(() => rules.update(r.id, { enabled: on }))}
                  className="mt-0.5"
                />
                <div className={cn("grid min-w-0 gap-1", !r.enabled && "text-muted-foreground")}>
                  <p className="font-medium break-words">{r.locations.map(entryName).join(" · ")}</p>
                  <p className="break-words">
                    <RuleTargetSummary rule={r} t={t} />
                  </p>
                  <div className="flex flex-wrap items-center gap-1.5">
                    {r.domains.map((d) => (
                      <Badge key={d} variant="outline">
                        {d}
                      </Badge>
                    ))}
                    {!r.enabled && <Badge variant="secondary">{t("locationRules.off")}</Badge>}
                  </div>
                  {r.note && <p className="break-words text-muted-foreground">{r.note}</p>}
                  <p className="text-xs text-muted-foreground">{r.updated_by_name ? t("locationRules.updatedBy", { date: formatDate(m, r.updated_at), name: r.updated_by_name }) : t("locationRules.updated", { date: formatDate(m, r.updated_at) })}</p>
                </div>
                <div className="flex flex-wrap gap-2 sm:justify-end">
                  <Button size="sm" variant="outline" onClick={() => setEditing(r)}>
                    {t("locationRules.edit")}
                  </Button>
                  <Button size="sm" variant="ghost" onClick={() => setHistory(r)}>
                    {t("locationRules.history")}
                  </Button>
                  <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" disabled={save.pending} onClick={() => void remove(r)}>
                    {t("locationRules.delete")}
                  </Button>
                </div>
              </li>
            ))}
          </ul>
          {rules.total > rules.rules.length && <p className="text-muted-foreground">{t("locationRules.newestShown", { shown: rules.rules.length, total: rules.total })}</p>}
        </>
      )}
      {editing && (
        <RuleDialog
          key={editing === "new" ? "new" : editing.id}
          rule={editing === "new" ? undefined : editing}
          domains={rules.domains}
          client={client}
          onError={onError}
          onClose={() => setEditing(null)}
          onSave={(input) => (editing === "new" ? rules.create(input) : rules.update(editing.id, input))}
        />
      )}
      {history && <HistoryDialog rule={history} client={client} onClose={() => setHistory(null)} />}
      {dialog}
    </div>
  );
}

function RuleTargetSummary({ rule, t }: { rule: LocationRule; t: Translator["t"] }) {
  if (rule.target === "site") return <>{t("locationRules.wholeSite")}</>;
  const names = rule.targets.map((x) => targetLabel(x));
  const shown = names.slice(0, 5).join(", ");
  const more = names.length > 5 ? ` ${t("locationRules.andMore", { count: names.length - 5 })}` : "";
  return <>{t(rule.target === "content" ? "locationRules.hidesContents" : "locationRules.hidesTags", { names: shown + more })}</>;
}

/** Where an address resolves and what covers it, per domain. */
function CheckAddress({ client }: { client: ContentKitClient }) {
  const { t } = useMessages();
  const ids = useId();
  const entryName = useEntryName();
  const country = useCountryName();
  const [ip, setIP] = useState("");
  const [result, setResult] = useState<LocationCheck | null>(null);
  const [error, setError] = useState<ContentKitError | null>(null);
  const [pending, setPending] = useState(false);
  const check = async () => {
    const address = ip.trim();
    if (!address) return;
    setPending(true);
    setError(null);
    try {
      setResult(await client.moderation.checkLocation(address));
    } catch (e) {
      setResult(null);
      setError(e as ContentKitError);
    } finally {
      setPending(false);
    }
  };
  const place = result && (result.location.subdivision
    ? entryName({ subdivision: result.location.subdivision })
    : result.location.country ? country(result.location.country) : "");
  return (
    <section className="grid gap-2 rounded-lg border border-border p-3" aria-labelledby={`${ids}-title`} data-ckui="location-check">
      <h3 id={`${ids}-title`} className="font-medium">
        {t("locationRules.checkTitle")}
      </h3>
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          void check();
        }}
      >
        <div className="grid min-w-48 flex-1 gap-1.5 sm:max-w-xs">
          <Label htmlFor={`${ids}-ip`}>{t("locationRules.address")}</Label>
          <Input id={`${ids}-ip`} value={ip} inputMode="text" placeholder="203.0.113.7" onChange={(e) => setIP(e.target.value)} />
        </div>
        <Button type="submit" variant="outline" disabled={pending || !ip.trim()}>
          {t("locationRules.check")}
        </Button>
      </form>
      <ErrorLine error={error} />
      {result && (
        <div className="grid gap-1" role="status">
          <p>{place ? t("locationRules.resolvesTo", { place }) : t("locationRules.unknownPlace")}</p>
          <ul className="grid gap-0.5 text-muted-foreground">
            {Object.entries(result.domains).map(([domain, matches]) => (
              <li key={domain}>
                <span className="font-medium text-foreground">{domain}</span>:{" "}
                {!matches.length
                  ? t("locationRules.noRuleApplies")
                  : matches.some((x) => x.target === "site")
                    ? t("locationRules.siteBlocked")
                    : t("locationRules.someHidden", { count: matches.length })}
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  );
}

interface RuleDialogProps {
  rule?: LocationRule;
  domains: string[];
  client: ContentKitClient;
  onError?: AdminFlowProps["onError"];
  onClose: () => void;
  onSave: (input: LocationRuleInput) => Promise<LocationRule>;
}

function RuleDialog({ rule, domains: offered, client, onError, onClose, onSave }: RuleDialogProps) {
  const { t } = useMessages();
  const ids = useId();
  const entryName = useEntryName();
  const save = useAction("rules.save", onError);
  const [entries, setEntries] = useState<LocationEntry[]>(rule?.locations ?? []);
  const [target, setTarget] = useState<Target>((rule?.target as Target) ?? "site");
  const [picked, setPicked] = useState<Picked[]>(() => (rule?.targets ?? []).map((x) => ({ id: x.id, label: targetLabel(x) })));
  const [domains, setDomains] = useState<string[]>(rule?.domains ?? offered.slice(0, 1));
  const [enabled, setEnabled] = useState(rule?.enabled ?? true);
  const [note, setNote] = useState(rule?.note ?? "");
  const valid = entries.length > 0 && domains.length > 0 && (target === "site" || picked.length > 0);
  const submit = async () => {
    if (!valid) return;
    const ids = picked.map((p) => p.id);
    const input: LocationRuleInput = {
      locations: entries,
      target,
      content_ids: target === "content" ? ids : [],
      entity_ids: target === "entities" ? ids : [],
      domains,
      enabled,
      note: note.trim(),
    };
    if (await save.run(() => onSave(input))) onClose();
  };
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-xl" data-ckui="location-rule-dialog">
        <form
          className="grid gap-5"
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
          <DialogHeader>
            <DialogTitle>{t(rule ? "locationRules.editTitle" : "locationRules.newTitle")}</DialogTitle>
            <DialogDescription>{t("locationRules.dialogHint")}</DialogDescription>
          </DialogHeader>

          <fieldset className="grid gap-2">
            <legend className="mb-2 font-medium">{t("locationRules.where")}</legend>
            {entries.length > 0 && (
              <ul className="flex flex-wrap gap-1.5" aria-label={t("locationRules.where")}>
                {entries.map((e) => (
                  <li key={entryKey(e)}>
                    <Badge variant="secondary" className="gap-1 pr-1">
                      {entryName(e)}
                      <button
                        type="button"
                        className="rounded-sm p-0.5 hover:bg-background/60"
                        aria-label={t("locationRules.removePlace", { place: entryName(e) })}
                        onClick={() => setEntries(entries.filter((x) => entryKey(x) !== entryKey(e)))}
                      >
                        <HugeiconsIcon icon={Cancel01Icon} strokeWidth={2} className="size-3" />
                      </button>
                    </Badge>
                  </li>
                ))}
              </ul>
            )}
            <AddEntry onAdd={(e) => !entries.some((x) => entryKey(x) === entryKey(e)) && setEntries([...entries, e])} />
          </fieldset>

          <fieldset className="grid gap-2">
            <legend className="mb-2 font-medium">{t("locationRules.what")}</legend>
            <div className="flex">
            <Segmented
              label={t("locationRules.what")}
              options={(["site", "content", "entities"] as const).map((key) => ({
                key,
                label: t(key === "site" ? "locationRules.wholeSite" : key === "content" ? "locationRules.contents" : "locationRules.tags"),
                pressed: target === key,
                onSelect: () => {
                  if (key !== target) setPicked([]);
                  setTarget(key);
                },
              }))}
            />
            </div>
            {target === "site" ? (
              <p className="text-muted-foreground">{t("locationRules.siteHint")}</p>
            ) : (
              <>
                <p className="text-muted-foreground">{t(target === "content" ? "locationRules.contentsHint" : "locationRules.tagsHint")}</p>
                {picked.length > 0 && (
                  <ul className="flex flex-wrap gap-1.5">
                    {picked.map((p) => (
                      <li key={p.id}>
                        <Badge variant="secondary" className="max-w-64 gap-1 pr-1">
                          <span className="truncate">{p.label}</span>
                          <button
                            type="button"
                            className="rounded-sm p-0.5 hover:bg-background/60"
                            aria-label={t("locationRules.removeTarget", { name: p.label })}
                            onClick={() => setPicked(picked.filter((x) => x.id !== p.id))}
                          >
                            <HugeiconsIcon icon={Cancel01Icon} strokeWidth={2} className="size-3" />
                          </button>
                        </Badge>
                      </li>
                    ))}
                  </ul>
                )}
                <TargetPicker
                  key={target}
                  client={client}
                  contents={target === "content"}
                  exclude={picked.map((p) => p.id)}
                  label={t(target === "content" ? "locationRules.findContent" : "locationRules.findTag")}
                  onPick={(p) => setPicked([...picked, p])}
                />
              </>
            )}
          </fieldset>

          <fieldset className="grid gap-2">
            <legend className="mb-2 font-medium">{t("locationRules.domains")}</legend>
            <div className="flex flex-wrap gap-x-4 gap-y-2">
              {offered.map((d) => (
                <label key={d} className="flex items-center gap-2">
                  <Switch size="sm" checked={domains.includes(d)} onCheckedChange={(on) => setDomains(on ? [...domains, d] : domains.filter((x) => x !== d))} />
                  {d}
                </label>
              ))}
            </div>
          </fieldset>

          <div className="grid gap-1.5">
            <Label htmlFor={`${ids}-note`}>{t("locationRules.note")}</Label>
            <Textarea id={`${ids}-note`} rows={2} maxLength={2000} value={note} placeholder={t("locationRules.notePlaceholder")} onChange={(e) => setNote(e.target.value)} />
          </div>
          <label className="flex items-center gap-2">
            <Switch checked={enabled} onCheckedChange={setEnabled} />
            {t("locationRules.enabled")}
          </label>

          <ErrorLine error={save.error} />
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={!valid || save.pending}>
              {t(rule ? "locationRules.save" : "locationRules.create")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** A country code, a US state or an address range, added one at a time. */
function AddEntry({ onAdd }: { onAdd: (e: LocationEntry) => void }) {
  const { t } = useMessages();
  const ids = useId();
  const country = useCountryName();
  const [type, setType] = useState<EntryType>("subdivision");
  const [value, setValue] = useState("");
  const code = value.trim().toUpperCase();
  const ready = type === "country" ? /^[A-Z]{2}$/.test(code) && country(code) !== code : type === "subdivision" ? STATE_NAMES.has(value) : value.trim() !== "";
  const add = () => {
    if (!ready) return;
    onAdd(type === "country" ? { country: code } : type === "subdivision" ? { subdivision: value } : { network: value.trim() });
    setValue("");
  };
  return (
    <div className="grid gap-2">
      <div className="flex">
      <Segmented
        label={t("locationRules.placeType")}
        options={(["subdivision", "country", "network"] as const).map((key) => ({
          key,
          label: t(key === "subdivision" ? "locationRules.usState" : key === "country" ? "locationRules.country" : "locationRules.network"),
          pressed: type === key,
          onSelect: () => {
            setType(key);
            setValue("");
          },
        }))}
      />
      </div>
      <div className="flex flex-wrap items-end gap-2">
        {type === "subdivision" ? (
          <div className="grid min-w-48 flex-1 gap-1.5">
            <Label htmlFor={`${ids}-state`} className="sr-only">
              {t("locationRules.usState")}
            </Label>
            <Select value={value} onValueChange={(v) => setValue(String(v ?? ""))}>
              <SelectTrigger id={`${ids}-state`} className="w-full">
                <SelectValue>{(v: string) => (v ? STATE_NAMES.get(v) : t("locationRules.pickState"))}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {US_STATES.map(([code, name]) => (
                  <SelectItem key={code} value={`US-${code}`}>
                    {name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        ) : (
          <div className="grid min-w-48 flex-1 gap-1.5">
            <Label htmlFor={`${ids}-value`} className="sr-only">
              {t(type === "country" ? "locationRules.country" : "locationRules.network")}
            </Label>
            <Input
              id={`${ids}-value`}
              value={value}
              maxLength={type === "country" ? 2 : 64}
              placeholder={type === "country" ? t("locationRules.countryPlaceholder") : "203.0.113.0/24"}
              onChange={(e) => setValue(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  add();
                }
              }}
            />
          </div>
        )}
        <Button type="button" variant="outline" disabled={!ready} onClick={add}>
          {t("locationRules.addPlace")}
        </Button>
      </div>
      {type === "country" && code.length === 2 && <p className="text-muted-foreground">{country(code) !== code ? country(code) : t("locationRules.unknownCountry")}</p>}
    </div>
  );
}

/** A typeahead over the staff search: contents, or entities of any kind. */
function TargetPicker({ client, contents, exclude, label, onPick }: { client: ContentKitClient; contents: boolean; exclude: readonly string[]; label: string; onPick: (p: Picked) => void }) {
  const { t, language } = useMessages();
  const id = useId();
  const [text, setText] = useState("");
  const [found, setFound] = useState<Picked[]>([]);
  const [active, setActive] = useState(0);
  const q = text.trim();
  useEffect(() => {
    setActive(0);
    if (!q) {
      setFound([]);
      return;
    }
    const ctl = new AbortController();
    const timer = setTimeout(() => {
      client.discovery
        .adminSuggestions({ q, display_language: language, include_contents: contents, include_entities: !contents, limit: 10 }, ctl.signal)
        .then((r) =>
          setFound(
            contents
              ? r.contents.map((c) => ({ id: c.id, label: c.title || c.code }))
              : r.entities.map((g) => ({ id: g.id, label: `${g.name || g.slug} · ${g.kind}` })),
          ),
        )
        .catch(() => !ctl.signal.aborted && setFound([]));
    }, 200);
    return () => {
      clearTimeout(timer);
      ctl.abort();
    };
  }, [client, q, contents, language]);
  const results = found.filter((p) => !exclude.includes(p.id)).slice(0, 8);
  const pick = (p: Picked) => {
    setText("");
    onPick(p);
  };
  const open = results.length > 0;
  return (
    <div className="relative grid gap-1">
      <Input
        role="combobox"
        aria-label={label}
        aria-expanded={open}
        aria-controls={`${id}-list`}
        aria-activedescendant={open ? `${id}-${active}` : undefined}
        aria-autocomplete="list"
        autoComplete="off"
        value={text}
        placeholder={label}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (!open) return;
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setActive((a) => Math.min(results.length - 1, a + 1));
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setActive((a) => Math.max(0, a - 1));
          } else if (e.key === "Enter") {
            e.preventDefault();
            pick(results[active]!);
          } else if (e.key === "Escape") setText("");
        }}
      />
      {open && (
        <ul id={`${id}-list`} role="listbox" aria-label={label} className="absolute top-full z-10 mt-1 grid w-full overflow-hidden rounded-md border border-border bg-popover text-popover-foreground shadow-md">
          {results.map((p, i) => (
            <li
              key={p.id}
              id={`${id}-${i}`}
              role="option"
              aria-selected={i === active}
              className={cn("cursor-pointer truncate px-3 py-1.5", i === active && "bg-accent text-accent-foreground")}
              onMouseEnter={() => setActive(i)}
              onMouseDown={(e) => {
                e.preventDefault();
                pick(p);
              }}
            >
              {p.label}
            </li>
          ))}
        </ul>
      )}
      {q && !open && <p className="text-muted-foreground">{t("locationRules.noMatches")}</p>}
    </div>
  );
}

function HistoryDialog({ rule, client, onClose }: { rule: LocationRule; client: ContentKitClient; onClose: () => void }) {
  const m = useMessages();
  const { t } = m;
  const entryName = useEntryName();
  const [items, setItems] = useState<LocationRuleHistory[] | null>(null);
  const [error, setError] = useState<ContentKitError | null>(null);
  useEffect(() => {
    const ctl = new AbortController();
    client.moderation
      .locationRuleHistory(rule.id, { limit: 100 }, ctl.signal)
      .then((page) => setItems(page.items))
      .catch((e) => !ctl.signal.aborted && setError(e as ContentKitError));
    return () => ctl.abort();
  }, [client, rule.id]);
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-lg" data-ckui="location-rule-history">
        <DialogHeader>
          <DialogTitle>{t("locationRules.historyTitle")}</DialogTitle>
          <DialogDescription>{rule.locations.map(entryName).join(" · ")}</DialogDescription>
        </DialogHeader>
        <ErrorLine error={error} />
        {!items && !error ? (
          <Rows count={2} />
        ) : (
          <ol className="grid gap-3 text-sm">
            {(items ?? []).map((h) => {
              const was = h.snapshot as unknown as LocationRule;
              return (
                <li key={h.id} className="grid gap-0.5 border-l-2 border-border pl-3">
                  <p>
                    <span className="font-medium">{t(ACTIONS[h.action as keyof typeof ACTIONS] ?? ACTIONS.update)}</span>{" "}
                    <span className="text-muted-foreground">{t("locationRules.byAt", { actor: h.actor_name || h.actor, date: formatDate(m, h.at) })}</span>
                  </p>
                  {h.action !== "delete" && was?.locations && (
                    <p className="text-muted-foreground break-words">
                      {was.locations.map(entryName).join(" · ")} · {was.domains.join(", ")} · {t(was.enabled ? "locationRules.on" : "locationRules.off")}
                    </p>
                  )}
                </li>
              );
            })}
          </ol>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            {t("common.close")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
