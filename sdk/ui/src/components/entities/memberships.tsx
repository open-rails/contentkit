import { cn } from "cn";
import { useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentMemberships, EntityCard, EntityKind, Membership } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import type { ContentKitErrorHandler } from "../../react/context.js";
import { useContentEntities, useKinds } from "../../react/entities.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { SaveState } from "../content/editors.js";
import { contentError, ErrorLine, useAction } from "../social/parts.js";
import { EntityChips, type VersionChoice } from "./entity-chips.js";
import { labelOf, type Labels } from "./parts.js";
import { EditorSaveContext, useEditorSaveTask } from "../content/editor-save.js";

export type { VersionChoice } from "./entity-chips.js";

export interface MembershipEditorProps {
  /** The content whose memberships are edited, its versions' included. */
  content: string;
  /** Its kind: the entity kinds it joins. */
  contentKind: string;
  /** Staff lookup, including internal classifications. */
  management?: boolean;
  /** Its versions, for version-level memberships (voice actors, version-only tags). */
  versions?: readonly VersionChoice[];
  /** Only these entity kinds; default every kind its kind joins. */
  entityKinds?: readonly string[];
  labels?: Labels;
  /** Entity names in this language; default the provider's. */
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
 * Staff: a content's entities, per entity kind: add and remove entities, and per
 * membership the version it targets, its role, its position in an ordered
 * entity, its window and acceptance of a proposed one. Saved in one write per
 * content for the kinds shown.
 */
export function MembershipEditor(p: MembershipEditorProps) {
  const m = useMessages();
  const { t } = m;
  const { kinds, loading: kindsLoading } = useKinds({ client: p.client });
  const cg = useContentEntities(p.content, { language: p.language, client: p.client });
  const entityKinds = useMemo(() => p.entityKinds ?? kinds?.kinds[p.contentKind]?.entities ?? [], [p.entityKinds, kinds, p.contentKind]);
  const [picked, setPicked] = useState<ReadonlyMap<string, EntityCard>>(new Map());
  const cards = useMemo(() => new Map([...cg.entities, ...picked]), [cg.entities, picked]);
  const seq = useRef(0);
  const initial = useMemo(
    () => cg.memberships.filter((x) => entityKinds.includes(cg.entities.get(x.group_id ?? "")?.kind ?? "")).map((x) => ({ ...x, content_id: undefined })),
    [cg.memberships, cg.entities, entityKinds],
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
  const coordinated = useContext(EditorSaveContext) !== null;
  const submit = async () => {
    setSaved(false);
    const out = await save.run(() => cg.set(draft.map(strip), [...entityKinds], newNames));
    if (!out) return false;
    setSaved(true);
    setCreated((all) => Object.fromEntries(Object.entries(all).filter(([kind]) => scopeMissing(kind))));
    setDraft(out.memberships.filter((x) => entityKinds.includes(out.entities.find((g) => g.id === x.group_id)?.kind ?? "")).map((x) => ({ ...x, content_id: undefined, key: ++seq.current })));
    p.onSaved?.(out);
    return true;
  };
  useEditorSaveTask(`memberships:${p.content}`, { label: t("contentAdmin.entities"), dirty, pending: save.pending, valid: true, save: submit, order: 20 });
  const change = (key: number, patch: Partial<Membership>) => {
    setSaved(false);
    setDraft((d) => d.map((x) => (x.key === key ? { ...x, ...patch } : x)));
  };
  const root = (children: ReactNode) => (
    <ContentKitUiRoot
      appearance={p.appearance}
      className={cn("rounded-lg border border-border bg-muted/30 p-4 grid gap-4 text-sm", p.className)}
      data-ckui="membership-editor"
    >
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2">
          <h3 className="text-sm font-medium">
            {t("contentAdmin.entities")}
          </h3>
          <span className="text-sm text-muted-foreground tabular-nums">
            {draft.length}
          </span>
        </div>
        <SaveState dirty={dirty} saved={saved} />
      </div>
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
        if (!coordinated) void submit();
      }}
    >
      {entityKinds.map((kind) => {
        const spec: EntityKind = kinds?.entity_kinds?.[kind] ?? {};
        const rows = draft.filter((x) => cards.get(x.group_id ?? "")?.kind === kind);
        const label = labelOf(p.labels, kind);
        return (
          <div key={kind} data-ckui="membership-kind" data-entity-kind={kind}>
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
      {!entityKinds.length && <p className="text-muted-foreground">{t("memberships.noKinds")}</p>}
      <ErrorLine error={save.error} />
      <div className="flex justify-end gap-2 border-t border-border pt-4">
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
        {!coordinated && (
          <Button
            type="submit"
            size="sm"
            variant={dirty ? "default" : "outline"}
            disabled={save.pending || !dirty}
          >
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
        )}
      </div>
    </form>,
  );
}
