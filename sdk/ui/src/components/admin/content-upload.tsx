import { useRef, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { Content, ContentModel, EntityCard } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import { useFlowClient } from "../../react/admin.js";
import { useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { useKinds } from "../../react/entities.js";
import { Button } from "#ckui/ui/button";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Switch } from "#ckui/ui/switch";
import { RuleFields, cleanRule, defaultAccessRule } from "../content/editors.js";
import { AttributeFields } from "../entities/fields.js";
import { EntityPicker, type Labels } from "../entities/parts.js";
import { applyDraftDefaults, createDraft, uploadTarget, type BulkDraftDefaults } from "./bulk-drafts.js";
import { UploadQueueBody } from "./upload-queue.js";
import { AdminRoot, LoadFailed, Rows, type AdminFlowProps } from "./shell.js";

type Choice = string | { value: string; label: string };
const choices = (list: readonly Choice[], name: (code: string) => string) => list.map((c) => typeof c === "string" ? { value: c, label: name(c) } : c);

export interface ContentUploadProps extends AdminFlowProps {
  /** Configured content kind. Its model defines formats, languages and upload paths. */
  kind: string;
  languages?: readonly Choice[];
  labels?: Labels;
  /** Initial host defaults, editable before dropping files. Nothing is published. */
  defaults?: Omit<BulkDraftDefaults, "artists">;
  /** Open the created draft in the host's gallery/content editor. */
  onOpen?: (content: Content) => void;
}

/** Drop files into new drafts, with transfer bytes and durable processing progress. */
export function ContentUpload({ client: own, baseUrl, auth, guard, appearance, className, language, messages, onOpen, ...props }: ContentUploadProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return <AdminRoot flow="content-upload" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
    <ConfiguredUpload key={props.kind} {...props} client={client} open={onOpen} />
  </AdminRoot>;
}
function ConfiguredUpload(props: Omit<ContentUploadBodyProps, "model">) {
  const { kinds, error, reload } = useKinds({ client: props.client });
  const { t } = useMessages();
  if (error) return <LoadFailed error={error} message={t("contentManager.loadFailed")} retry={reload} />;
  if (!kinds) return <Rows count={2} />;
  if (!kinds.kinds[props.kind]) return null;
  return <ContentUploadBody {...props} model={kinds} />;
}

interface ContentUploadBodyProps {
  client: ContentKitClient;
  kind: string;
  model: ContentModel;
  languages?: readonly Choice[];
  labels?: Labels;
  defaults?: Omit<BulkDraftDefaults, "artists">;
  open?: (content: Content) => void;
  onError?: ContentKitErrorHandler;
}

export function ContentUploadBody({ client, kind, model, languages: offered, labels, defaults: initialDefaults, open, onError }: ContentUploadBodyProps) {
  const { t } = useMessages();
  const languageName = useLanguageName();
  const languages = choices(offered ?? model.languages, languageName);
  const spec = model.kinds[kind]!;
  const [defaults, setDefaults] = useState<BulkDraftDefaults>(initialDefaults ?? {});
  const [artists, setArtists] = useState<readonly EntityCard[]>([]);
  const drafts = useRef(new Map<string, Content>()).current;
  // Queued entries keep this callback and its drafts through retries.
  const created = new WeakMap<File, Awaited<ReturnType<typeof createDraft>>>();
  const report = useErrorReporter(onError);
  const createItem = async (file: File) => {
    const snapshot = { ...defaults, access: defaults.access ? cleanRule(defaults.access) : undefined, artists: artists.map((g) => g.id) };
    let draft = created.get(file);
    if (!draft) {
      draft = await createDraft(client, kind, snapshot.language ?? languages[0]?.value ?? model.languages[0] ?? "", file, t("readerEditor.untitled"));
      created.set(file, draft);
    }
    const content = await applyDraftDefaults(client, draft, model, snapshot);
    drafts.set(content.id, content);
    return draft.ref;
  };
  const openDraft = (ref: { id: string }) => {
    const known = drafts.get(ref.id);
    if (known) return open?.(known);
    // Resumed after a reload: the draft is no longer in memory.
    void client.contents.manage({ kind, ids: [ref.id], limit: 1 }).then(
      ([c]) => { if (c) open?.(c); },
      (e) => report(e, "upload"),
    );
  };
  const none = "__none__";
  return <>
    <fieldset className="grid gap-3">
      <legend className="mb-2 text-sm font-medium">{t("contentManager.bulkDefaults")}</legend>
      <div className="grid gap-3 sm:grid-cols-2">
        {spec.attributes?.format && <AttributeFields declared={{ format: { ...spec.attributes.format, required: false } }} form={{ format: defaults.format ?? "" }} onChange={(_, value) => setDefaults((d) => ({ ...d, format: String(value) }))} labels={labels} />}
        <div className="grid gap-1.5">
          <Label>{t("contentManager.language")}</Label>
          <Select value={defaults.language ?? none} onValueChange={(v) => setDefaults((d) => ({ ...d, language: v === none ? undefined : String(v) }))}>
            <SelectTrigger size="sm" aria-label={t("contentManager.language")}>
              <SelectValue>{(v: string) => v === none ? t("contentManager.bulkUnchanged") : languages.find((l) => l.value === v)?.label}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={none}>{t("contentManager.bulkUnchanged")}</SelectItem>
              {languages.map((l) => <SelectItem key={l.value} value={l.value}>{l.label}</SelectItem>)}
            </SelectContent>
          </Select>
        </div>
      </div>
      <label className="flex items-center gap-2 text-sm">
        <Switch checked={!!defaults.access} onCheckedChange={(on) => setDefaults((d) => ({ ...d, access: on ? defaultAccessRule(model, kind) : undefined }))} />
        {t("contentManager.bulkAccess")}
      </label>
      {defaults.access && <RuleFields value={defaults.access} onChange={(access) => setDefaults((d) => ({ ...d, access }))} model={model} kind={kind} labels={labels} client={client} />}
      {spec.entities?.includes("artist") && <div className="grid gap-1.5">
        <Label>{t("contentManager.bulkArtists")}</Label>
        {artists.map((g) => <div key={g.id} className="flex items-center gap-2 text-sm">
          <span>{g.name || g.slug}</span>
          <Button size="xs" variant="ghost" aria-label={t("memberships.remove", { name: g.name || g.slug })} onClick={() => setArtists((all) => all.filter((a) => a.id !== g.id))}>{t("common.remove")}</Button>
        </div>)}
        <EntityPicker management kind="artist" language={defaults.language} exclude={artists.map((g) => g.id)} label={t("contentManager.bulkArtists")} onPick={(g) => setArtists((all) => [...all, g])} client={client} />
      </div>}
    </fieldset>
    <UploadQueueBody client={client} createItem={createItem}
      target={(ref, file, signal) => uploadTarget(client, ref, file, signal)} accept={spec.presentation === "reader" ? ".zip,application/zip" : "video/*"}
      concurrency={2} storageKey={`ckui.bulk.${kind}`} onOpen={open ? openDraft : undefined} onError={onError} />
  </>;
}

