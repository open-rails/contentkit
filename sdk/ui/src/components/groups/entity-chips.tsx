import { Cancel01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useId, useMemo, useRef, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { fromLocalInput, toLocalInput } from "../../client/group-fields.js";
import type { GroupCard, GroupKind, Membership } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useContentKitClient } from "../../react/context.js";
import { useTypeahead } from "../../react/discovery.js";
import { ErrorLine } from "../social/parts.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { labelOf, type Labels } from "./parts.js";

export interface EntityChipsProps {
  /** The entity kind these links join. */
  kind: string;
  label: string;
  /** This kind's memberships, in order; callbacks index into this list. */
  memberships: readonly Membership[];
  cards: ReadonlyMap<string, GroupCard>;
  spec: GroupKind;
  labels?: Labels;
  versions?: readonly VersionChoice[];
  versionLabel: (id: string) => string;
  language?: string;
  /** Staff lookup, including internal entities. */
  management?: boolean;
  client?: ContentKitClient;
  disabled?: boolean;
  onAdd: (card: GroupCard) => void;
  onChange: (index: number, patch: Partial<Membership>) => void;
  onRemove: (index: number) => void;
}

const fold = (s: string) =>
  s
    .normalize("NFKD")
    .replace(/\p{M}/gu, "")
    .toLocaleLowerCase()
    .replace(/\s+/g, " ")
    .trim();

/** Splits typed or pasted text on commas and newlines into trimmed tokens. */
export function splitTokens(text: string): string[] {
  return text
    .split(/[,\n]/)
    .map((s) => s.trim())
    .filter(Boolean);
}

/** An exact name or slug match wins; a lone result is an alias match. */
export function resolveToken(token: string, found: readonly GroupCard[], kind: string): GroupCard | undefined {
  const own = found.filter((g) => g.kind === kind);
  const key = fold(token);
  return own.find((g) => fold(g.name) === key) ?? own.find((g) => fold(g.slug) === key) ?? (own.length === 1 ? own[0] : undefined);
}

/**
 * A content's links to one entity kind as chips: type or paste names
 * separated by commas, remove with × or Backspace. A chip with roles,
 * versions, positions or a proposal opens those details. Names without a
 * match stay marked and are not saved.
 */
export function EntityChips(p: EntityChipsProps) {
  const { t } = useMessages();
  const id = useId();
  const client = useContentKitClient(p.client);
  const input = useRef<HTMLInputElement>(null);
  const [text, setText] = useState("");
  const [pending, setPending] = useState<string[]>([]);
  const [missing, setMissing] = useState<string[]>([]);
  const [open, setOpen] = useState<number | null>(null);
  const [active, setActive] = useState(0);
  const fragment = text.trim();
  const found = useTypeahead(fragment, { entity_kinds: [p.kind], display_language: p.language, limit: 8, client: p.client, management: p.management });
  const name = (m: Membership) => {
    const card = p.cards.get(m.group_id ?? "");
    return card ? card.name || card.slug : (m.group_id ?? "");
  };
  // A whole-content link without a role is the one duplicate to skip.
  const plain = useMemo(() => new Set(p.memberships.filter((m) => !m.version_id && !m.role).map((m) => m.group_id)), [p.memberships]);
  const suggestions = useMemo(() => found.entities.filter((g) => g.kind === p.kind && !plain.has(g.id)).slice(0, 6), [found.entities, p.kind, plain]);
  const listOpen = fragment !== "" && suggestions.length > 0;
  useEffect(() => setActive(0), [fragment]);
  const detailed = (m: Membership) => !!p.spec.version_level || !!p.spec.roles?.length || !!p.spec.ordered || m.state === "proposed" || !!m.active_from || !!m.active_until;
  const add = (card: GroupCard) => {
    if (plain.has(card.id)) return false;
    p.onAdd(card);
    return true;
  };
  const lookup = (token: string) =>
    p.management
      ? client.entities.manage({ q: token, kind: p.kind, language: p.language, limit: 10 }).then((page) => page.entities)
      : client.discovery.typeahead({ q: token, entity_kinds: [p.kind], display_language: p.language, limit: 10 }).then((r) => r.entities);
  const commit = (tokens: string[]) => {
    const known = new Set([...p.memberships.map((m) => fold(name(m))), ...pending.map(fold), ...missing.map(fold)]);
    const fresh: string[] = [];
    for (const token of tokens) {
      if (known.has(fold(token))) continue;
      known.add(fold(token));
      fresh.push(token);
    }
    if (!fresh.length) return;
    setPending((all) => [...all, ...fresh]);
    for (const token of fresh) {
      lookup(token).then(
        (cards) => {
          const card = resolveToken(token, cards, p.kind);
          if (card) add(card);
          else setMissing((all) => [...all, token]);
        },
        () => setMissing((all) => [...all, token]),
      ).finally(() => setPending((all) => all.filter((x) => x !== token)));
    }
  };
  const type = (value: string) => {
    if (!/[,\n]/.test(value)) return setText(value);
    const parts = value.split(/[,\n]/);
    setText(parts.pop() ?? "");
    commit(parts.map((s) => s.trim()).filter(Boolean));
  };
  // One picked name with roles, versions or positions opens its details.
  const pick = (card: GroupCard) => {
    setText("");
    if (add(card) && (p.spec.version_level || p.spec.roles?.length || p.spec.ordered)) setOpen(p.memberships.length);
  };
  const removeLast = () => {
    if (missing.length) return setMissing((all) => all.slice(0, -1));
    if (p.memberships.length) p.onRemove(p.memberships.length - 1);
  };
  const detail = open !== null ? p.memberships[open] : undefined;
  return (
    <div className="grid min-w-0 gap-1.5" data-ckui="entity-chips" data-group-kind={p.kind}>
      <label htmlFor={`${id}-input`} className="font-semibold">
        {p.label}
      </label>
      <div
        className={cn(
          "relative flex min-h-9 flex-wrap items-center gap-1.5 rounded-md border border-input bg-transparent px-2 py-1.5 focus-within:border-ring focus-within:ring-[3px] focus-within:ring-ring/50",
          p.disabled && "pointer-events-none opacity-50",
        )}
        onClick={() => input.current?.focus()}
      >
        <ul className="contents" aria-label={p.label}>
          {p.memberships.map((m, i) => {
            const n = name(m);
            const extra = [m.role && labelOf(p.labels, m.role), m.version_id && p.versionLabel(m.version_id), m.position != null && `#${m.position}`].filter(Boolean).join(" · ");
            return (
              <li key={`${m.group_id}:${i}`} className={cn("inline-flex max-w-full items-center gap-1 rounded-md bg-muted py-0.5 ps-2 pe-1 text-sm", m.state === "proposed" && "border border-dashed border-primary", open === i && "ring-2 ring-ring")} data-ckui="entity-chip" data-group={p.cards.get(m.group_id ?? "")?.slug}>
                {detailed(m) ? (
                  <button type="button" className="min-w-0 truncate text-start hover:underline" aria-expanded={open === i} aria-label={t("memberships.chipDetails", { name: n })} onClick={(e) => (e.stopPropagation(), setOpen(open === i ? null : i))}>
                    {n}
                    {extra && <span className="text-muted-foreground"> · {extra}</span>}
                    {m.state === "proposed" && <span className="text-primary"> · {t("memberships.proposed")}</span>}
                  </button>
                ) : (
                  <span className="min-w-0 truncate">{n}</span>
                )}
                <button type="button" className="rounded-sm p-0.5 text-muted-foreground hover:bg-background hover:text-foreground" aria-label={t("memberships.remove", { name: n })} onClick={(e) => (e.stopPropagation(), open === i && setOpen(null), p.onRemove(i))}>
                  <HugeiconsIcon icon={Cancel01Icon} className="size-3" strokeWidth={2.5} />
                </button>
              </li>
            );
          })}
          {pending.map((token) => (
            <li key={`pending:${token}`} className="inline-flex items-center rounded-md bg-muted px-2 py-0.5 text-sm opacity-60" aria-busy>
              {token}
            </li>
          ))}
          {missing.map((token, i) => (
            <li key={`missing:${token}`} className="inline-flex items-center gap-1 rounded-md border border-dashed border-destructive py-0.5 ps-2 pe-1 text-sm text-destructive" data-ckui="entity-chip-missing">
              <span className="line-through">{token}</span>
              <button type="button" className="rounded-sm p-0.5 hover:bg-destructive/15" aria-label={t("memberships.remove", { name: token })} onClick={(e) => (e.stopPropagation(), setMissing((all) => all.filter((_, j) => j !== i)))}>
                <HugeiconsIcon icon={Cancel01Icon} className="size-3" strokeWidth={2.5} />
              </button>
            </li>
          ))}
        </ul>
        <input
          ref={input}
          id={`${id}-input`}
          role="combobox"
          aria-label={t("memberships.add", { kind: p.label })}
          aria-expanded={listOpen}
          aria-controls={`${id}-list`}
          aria-activedescendant={listOpen ? `${id}-${active}` : undefined}
          aria-autocomplete="list"
          autoComplete="off"
          disabled={p.disabled}
          className="min-w-32 flex-1 bg-transparent text-sm outline-none placeholder:text-muted-foreground"
          placeholder={p.memberships.length || missing.length ? "" : t("memberships.chipsPlaceholder")}
          value={text}
          onChange={(e) => type(e.target.value)}
          onBlur={() => {
            if (fragment) commit([fragment]);
            setText("");
          }}
          onKeyDown={(e) => {
            if (e.key === "Backspace" && text === "") {
              e.preventDefault();
              removeLast();
            } else if (e.key === "ArrowDown" && listOpen) {
              e.preventDefault();
              setActive((a) => Math.min(suggestions.length - 1, a + 1));
            } else if (e.key === "ArrowUp" && listOpen) {
              e.preventDefault();
              setActive((a) => Math.max(0, a - 1));
            } else if (e.key === "Enter") {
              e.preventDefault();
              if (listOpen) pick(suggestions[active]!);
              else if (fragment) {
                commit([fragment]);
                setText("");
              }
            } else if (e.key === "Escape") setText("");
          }}
        />
        {listOpen && (
          <ul id={`${id}-list`} role="listbox" aria-label={p.label} className="absolute start-0 top-full z-10 mt-1 grid w-full overflow-hidden rounded-md border border-border bg-popover text-popover-foreground shadow-md">
            {suggestions.map((g, i) => (
              <li
                key={g.id}
                id={`${id}-${i}`}
                role="option"
                aria-selected={i === active}
                className={cn("flex cursor-pointer items-baseline justify-between gap-2 px-3 py-1.5 text-sm", i === active && "bg-accent text-accent-foreground")}
                onMouseEnter={() => setActive(i)}
                onMouseDown={(e) => {
                  e.preventDefault();
                  pick(g);
                }}
              >
                <span className="min-w-0 truncate">{g.name || g.slug}</span>
                <span className="text-xs text-muted-foreground">{g.slug}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
      {missing.length > 0 && (
        <p className="text-xs text-destructive" role="status">
          {t("memberships.notFound", { names: missing.join(", ") })}
        </p>
      )}
      <ErrorLine error={found.error} />
      {detail && open !== null && (
        <ul>
          <MembershipRow
            m={detail}
            card={p.cards.get(detail.group_id ?? "")}
            spec={p.spec}
            labels={p.labels}
            versions={p.versions}
            versionLabel={p.versionLabel}
            onChange={(patch) => p.onChange(open, patch)}
            onRemove={() => {
              setOpen(null);
              p.onRemove(open);
            }}
          />
        </ul>
      )}
    </div>
  );
}

const NONE = "__none__";

/** A version a membership may target. */
export interface VersionChoice {
  id: string;
  label: string;
}

export function MembershipRow({
  m: x,
  card,
  spec,
  labels,
  versions,
  versionLabel,
  onChange,
  onRemove,
}: {
  m: Membership;
  card?: GroupCard;
  spec: GroupKind;
  labels?: Labels;
  versions?: readonly VersionChoice[];
  versionLabel: (id: string) => string;
  onChange: (patch: Partial<Membership>) => void;
  onRemove: () => void;
}) {
  const { t } = useMessages();
  const id = useId();
  const name = card ? card.name || card.slug : (x.group_id ?? "");
  const versionIds = [...new Set([...(versions ?? []).map((v) => v.id), ...(x.version_id ? [x.version_id] : [])])];
  return (
    <li className="flex flex-wrap items-center gap-2 rounded-md border border-border p-2" data-ckui="membership" data-group={card?.slug} data-state={x.state ?? "active"}>
      <span className="min-w-0 flex-1 font-medium break-words">{name}</span>
      {x.state === "proposed" && (
        <>
          <Badge variant="secondary">{t("memberships.proposed")}</Badge>
          <Button type="button" size="xs" variant="outline" onClick={() => onChange({ state: "active" })}>
            {t("memberships.accept")}
          </Button>
        </>
      )}
      {spec.version_level && versionIds.length > 0 && (
        <Select value={x.version_id ?? NONE} onValueChange={(v) => onChange({ version_id: v === NONE ? undefined : String(v) })}>
          <SelectTrigger size="sm" aria-label={t("memberships.versionOf", { name })}>
            <SelectValue>{(v: string) => (v === NONE ? t("memberships.wholeContent") : versionLabel(v))}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={NONE}>{t("memberships.wholeContent")}</SelectItem>
            {versionIds.map((v) => (
              <SelectItem key={v} value={v}>
                {versionLabel(v)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
      {!!spec.roles?.length && (
        <Select value={x.role || NONE} onValueChange={(v) => onChange({ role: v === NONE ? undefined : String(v) })}>
          <SelectTrigger size="sm" aria-label={t("memberships.roleOf", { name })}>
            <SelectValue>{(v: string) => (v === NONE ? t("memberships.noRole") : labelOf(labels, v))}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={NONE}>{t("memberships.noRole")}</SelectItem>
            {spec.roles.map((r) => (
              <SelectItem key={r} value={r}>
                {labelOf(labels, r)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
      {spec.ordered && (
        <div className="flex items-center gap-1">
          <Label htmlFor={`${id}-pos`} className="text-xs text-muted-foreground">
            {t("memberships.position")}
          </Label>
          <Input
            id={`${id}-pos`}
            className="h-8 w-20"
            type="number"
            min={0}
            step={1}
            value={x.position ?? ""}
            placeholder={t("memberships.last")}
            onChange={(e) => onChange({ position: e.target.value === "" ? undefined : Math.max(0, Math.trunc(Number(e.target.value))) })}
          />
        </div>
      )}
      {spec.windows && <WindowFields from={x.active_from} until={x.active_until} name={name} onChange={onChange} />}
      <Button type="button" size="xs" variant="ghost" aria-label={t("memberships.remove", { name })} onClick={onRemove}>
        {t("common.remove")}
      </Button>
    </li>
  );
}

export function WindowFields({ from, until, name, onChange }: { from?: string; until?: string; name: string; onChange: (patch: Partial<Membership>) => void }) {
  const { t } = useMessages();
  const id = useId();
  return (
    <div className="flex flex-wrap items-center gap-1" data-ckui="membership-window">
      <Label htmlFor={`${id}-from`} className="text-xs text-muted-foreground">
        {t("memberships.from")}
      </Label>
      <Input
        id={`${id}-from`}
        className="h-8 w-48"
        type="datetime-local"
        aria-label={t("memberships.fromOf", { name })}
        value={toLocalInput(from)}
        onChange={(e) => onChange({ active_from: fromLocalInput(e.target.value) })}
      />
      <Label htmlFor={`${id}-until`} className="text-xs text-muted-foreground">
        {t("memberships.until")}
      </Label>
      <Input
        id={`${id}-until`}
        className="h-8 w-48"
        type="datetime-local"
        aria-label={t("memberships.untilOf", { name })}
        value={toLocalInput(until)}
        onChange={(e) => onChange({ active_until: fromLocalInput(e.target.value) })}
      />
    </div>
  );
}
