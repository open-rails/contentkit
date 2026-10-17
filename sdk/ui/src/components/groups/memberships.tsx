import { cn } from "cn";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentMemberships, GroupCard, GroupKind, Membership } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import type { ContentKitErrorHandler } from "../../react/context.js";
import { useContentGroups, useKinds } from "../../react/groups.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { contentError, ErrorLine, useAction } from "../social/parts.js";
import { EntityChips, type VersionChoice } from "./entity-chips.js";
import { labelOf, type Labels } from "./parts.js";

export type { VersionChoice } from "./entity-chips.js";

export interface MembershipEditorProps {
  /** The content whose memberships are edited, its versions' included. */
  content: string;
  /** Its kind: the group kinds it joins. */
  contentKind: string;
  /** Staff lookup, including internal classifications. */
  management?: boolean;
  /** Its versions, for version-level memberships (voice actors, version-only tags). */
  versions?: readonly VersionChoice[];
  /** Only these group kinds; default every kind its kind joins. */
  groupKinds?: readonly string[];
  labels?: Labels;
  /** Group names in this language; default the provider's. */
  language?: string;
  onSaved?: (saved: ContentMemberships) => void;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

type Draft = Membership & { key: number };

const strip = ({ key: _key, ...m }: Draft): Membership => m;
const same = (a: Membership[], b: Membership[]) => JSON.stringify(a) === JSON.stringify(b);

/**
 * Staff: a content's groups, per group kind: add and remove groups, and per
 * membership the version it targets, its role, its position in an ordered
 * group, its window and acceptance of a proposed one. Saved in one write per
 * content for the kinds shown.
 */
export function MembershipEditor(p: MembershipEditorProps) {
  const m = useMessages();
  const { t } = m;
  const { kinds, loading: kindsLoading } = useKinds({ client: p.client });
  const cg = useContentGroups(p.content, { language: p.language, client: p.client });
  const groupKinds = useMemo(() => p.groupKinds ?? kinds?.kinds[p.contentKind]?.entities ?? [], [p.groupKinds, kinds, p.contentKind]);
  const [picked, setPicked] = useState<ReadonlyMap<string, GroupCard>>(new Map());
  const cards = useMemo(() => new Map([...cg.entities, ...picked]), [cg.entities, picked]);
  const seq = useRef(0);
  const initial = useMemo(
    () => cg.memberships.filter((x) => groupKinds.includes(cg.entities.get(x.group_id ?? "")?.kind ?? "")).map((x) => ({ ...x, content_id: undefined })),
    [cg.memberships, cg.entities, groupKinds],
  );
  const [draft, setDraft] = useState<Draft[]>([]);
  const [created, setCreated] = useState<Record<string, string[]>>({});
  // A scoped kind's new names wait for exactly one linked or new scope entity.
  const scopeMissing = (kind: string) => {
    const by = kinds?.entity_kinds?.[kind]?.scoped_by;
    if (!by) return undefined;
    const linked = new Set(draft.filter((x) => cards.get(x.group_id ?? "")?.kind === by).map((x) => x.group_id));
    return linked.size + (created[by]?.length ?? 0) === 1 ? undefined : labelOf(p.labels, by);
  };
  const newNames = Object.entries(created).flatMap(([kind, names]) => (scopeMissing(kind) ? [] : names.map((name) => ({ kind, name, language: p.language }))));
  const dirty = !same(draft.map(strip), initial) || newNames.length > 0;
  const dirtyRef = useRef(dirty);
  dirtyRef.current = dirty;
  useEffect(() => {
    if (!dirtyRef.current || !draft.length) setDraft(initial.map((x) => ({ ...x, key: ++seq.current })));
    // Server data replaces an untouched draft only.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initial]);
  const save = useAction("membership.save", p.onError);
  const [saved, setSaved] = useState(false);
  const change = (key: number, patch: Partial<Membership>) => {
    setSaved(false);
    setDraft((d) => d.map((x) => (x.key === key ? { ...x, ...patch } : x)));
  };
  const root = (children: ReactNode) => (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="membership-editor">
      {children}
    </ContentKitUiRoot>
  );
  if (kindsLoading || (cg.loading && !cg.memberships.length)) return root(<Skeleton className="h-32 w-full" />);
  if (cg.error && !cg.memberships.length)
    return root(
      <div role="alert" className="grid justify-items-start gap-2">
        <p className="text-destructive">{contentError(m, cg.error)}</p>
        <Button size="sm" variant="outline" onClick={cg.reload}>
          {t("common.retry")}
        </Button>
      </div>,
    );
  const versionLabel = (id: string) => p.versions?.find((v) => v.id === id)?.label ?? id.slice(0, 8);
  return root(
    <form
      className="grid gap-5"
      onSubmit={(e) => {
        e.preventDefault();
        setSaved(false);
        void save.run(() => cg.set(draft.map(strip), [...groupKinds], newNames)).then((out) => {
          if (!out) return;
          setSaved(true);
          setCreated((all) => Object.fromEntries(Object.entries(all).filter(([kind]) => scopeMissing(kind))));
          setDraft(out.memberships.filter((x) => groupKinds.includes(out.entities.find((g) => g.id === x.group_id)?.kind ?? "")).map((x) => ({ ...x, content_id: undefined, key: ++seq.current })));
          p.onSaved?.(out);
        });
      }}
    >
      {groupKinds.map((kind) => {
        const spec: GroupKind = kinds?.entity_kinds?.[kind] ?? {};
        const rows = draft.filter((x) => cards.get(x.group_id ?? "")?.kind === kind);
        const label = labelOf(p.labels, kind);
        return (
          <div key={kind} data-ckui="membership-kind" data-group-kind={kind}>
            <EntityChips
              kind={kind}
              label={label}
              memberships={rows}
              cards={cards}
              spec={spec}
              labels={p.labels}
              versions={p.versions}
              versionLabel={versionLabel}
              language={p.language}
              management={p.management}
              client={p.client}
              created={created[kind] ?? []}
              needsScope={scopeMissing(kind)}
              onCreatedChange={(names) => {
                setSaved(false);
                setCreated((all) => ({ ...all, [kind]: names }));
              }}
              onChange={(i, patch) => change(rows[i]!.key, patch)}
              onRemove={(i) => {
                setSaved(false);
                setDraft((d) => d.filter((y) => y.key !== rows[i]!.key));
              }}
              onAdd={(g) => {
                setSaved(false);
                setPicked((all) => new Map(all).set(g.id, g));
                setDraft((d) => [...d, { group_id: g.id, key: ++seq.current }]);
              }}
            />
          </div>
        );
      })}
      {!groupKinds.length && <p className="text-muted-foreground">{t("memberships.noKinds")}</p>}
      <ErrorLine error={save.error} />
      <div className="flex items-center gap-3">
        <Button type="submit" size="sm" disabled={save.pending || !dirty}>
          {save.pending ? t("common.saving") : t("common.save")}
        </Button>
        {dirty && (
          <Button
            type="button"
            size="sm"
            variant="ghost"
            disabled={save.pending}
            onClick={() => {
              setCreated({});
              setDraft(initial.map((x) => ({ ...x, key: ++seq.current })));
            }}
          >
            {t("memberships.discard")}
          </Button>
        )}
        {saved && !dirty && (
          <span role="status" className="text-muted-foreground">
            {t("memberships.saved")}
          </span>
        )}
      </div>
    </form>,
  );
}
