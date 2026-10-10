import { ArrowDown01Icon, ArrowUp01Icon, Delete02Icon, ImageAdd01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import type { Poll, PollKind, PollOption, PollUpdate } from "../../client/generated/wire.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { useMessages } from "../../i18n/context.js";
import { useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { usePollEditor } from "../../react/polls.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { Switch } from "#ckui/ui/switch";
import { Textarea } from "#ckui/ui/textarea";
import { localInput } from "./bans.js";
import { ConfirmDialog, contentError, ErrorLine } from "./parts.js";

export interface PollEditorProps {
  /** The poll to edit; none creates one. */
  poll?: string | null;
  /** Languages offered for a new poll: codes, or codes with labels. Without them the language is typed. */
  languages?: readonly (string | { value: string; label: string })[];
  /** A new poll's language. Default the first offered, else "en". */
  defaultLanguage?: string;
  /** Offer free-text polls (the host registered an answer classifier). Default false. */
  freeText?: boolean;
  /** Most options. Default 10. */
  maxOptions?: number;
  onCreated?: (poll: Poll) => void;
  onSaved?: (poll: Poll) => void;
  onDeleted?: () => void;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

interface Draft {
  key: string;
  label: string;
  image: File | null;
}

const toISO = (local: string) => (local ? new Date(local).toISOString() : undefined);
let seq = 0;
const draft = (): Draft => ({ key: `o${++seq}`, label: "", image: null });

/**
 * Staff: creates or edits a poll (PollWrite): question, language, type, when
 * it goes live and closes, visibility, a question image, and options with
 * images, reordered with the move buttons. A new poll is written at once with
 * its images; an existing one saves its fields with Save and each option
 * change as it is made.
 */
export function PollEditor(p: PollEditorProps) {
  const m = useMessages();
  const { t } = m;
  const ed = usePollEditor(p.poll ?? null, { client: p.client });
  const report = useErrorReporter(p.onError);
  const [error, setError] = useState<ContentKitError | string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const fail = (e: unknown) => {
    const err = toContentKitError(e);
    setError(err);
    report(err, "poll.save");
  };
  const root = (children: ReactNode) => (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-5 text-sm", p.className)} data-ckui="poll-editor">
      {children}
    </ContentKitUiRoot>
  );
  if (ed.loading && !ed.poll) {
    return root(
      <div className="grid gap-3" aria-busy>
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-24 w-full" />
      </div>,
    );
  }
  if (ed.error && !ed.poll) return root(<ErrorLine error={ed.error} />);
  return root(
    <>
      <h2 className="text-base font-semibold">{t(ed.poll ? "pollEditor.editTitle" : "pollEditor.newTitle")}</h2>
      {ed.poll ? (
        <EditFields
          key={ed.poll.id}
          poll={ed.poll}
          saving={ed.saving}
          maxOptions={p.maxOptions ?? 10}
          editor={ed}
          onError={fail}
          onInvalid={(msg) => setError(msg)}
          onSaved={(poll) => {
            setError(null);
            setNotice(t("pollEditor.saved"));
            p.onSaved?.(poll);
          }}
        />
      ) : (
        <CreateFields
          saving={ed.saving}
          languages={p.languages}
          defaultLanguage={p.defaultLanguage}
          freeText={!!p.freeText}
          maxOptions={p.maxOptions ?? 10}
          onInvalid={(msg) => setError(msg)}
          onCreate={async (input, images) => {
            setError(null);
            try {
              const { poll, imageErrors } = await ed.create(input, images);
              setNotice(imageErrors.length ? t("pollEditor.imageFailed") : t("pollEditor.created"));
              for (const e of imageErrors) report(e, "poll.save");
              p.onCreated?.(poll);
            } catch (e) {
              fail(e);
            }
          }}
        />
      )}
      {error && <ErrorLine>{typeof error === "string" ? error : contentError(m, error)}</ErrorLine>}
      {notice && !error && (
        <p role="status" className="text-sm text-muted-foreground">
          {notice}
        </p>
      )}
      {ed.poll && (
        <div className="border-t border-border pt-4">
          <Button variant="destructive" onClick={() => setDeleting(true)} disabled={ed.saving}>
            {t("pollEditor.delete")}
          </Button>
        </div>
      )}
      <ConfirmDialog
        open={deleting}
        onOpenChange={setDeleting}
        title={t("pollEditor.deleteTitle")}
        description={t("pollEditor.deleteBody")}
        confirm={t("pollEditor.delete")}
        onConfirm={() =>
          void ed.remove().then(
            () => p.onDeleted?.(),
            (e) => fail(e),
          )
        }
      />
    </>,
  );
}

function CreateFields(p: {
  saving: boolean;
  languages?: PollEditorProps["languages"];
  defaultLanguage?: string;
  freeText: boolean;
  maxOptions: number;
  onInvalid: (message: string) => void;
  onCreate: (input: Parameters<ReturnType<typeof usePollEditor>["create"]>[0], images: { question: File | null; options: (File | null)[] }) => Promise<void>;
}) {
  const { t } = useMessages();
  const ids = useId();
  const langs = useMemo(() => (p.languages ?? []).map((l) => (typeof l === "string" ? { value: l, label: l } : l)), [p.languages]);
  const [question, setQuestion] = useState("");
  const [language, setLanguage] = useState(p.defaultLanguage ?? langs[0]?.value ?? "en");
  const [kind, setKind] = useState<PollKind>("multiple_choice");
  const [liveAt, setLiveAt] = useState(() => localInput(new Date()));
  const [closesAt, setClosesAt] = useState("");
  const [image, setImage] = useState<File | null>(null);
  const [options, setOptions] = useState<Draft[]>(() => [draft(), draft()]);
  const set = (key: string, patch: Partial<Draft>) => setOptions((os) => os.map((o) => (o.key === key ? { ...o, ...patch } : o)));
  const move = (from: number, to: number) =>
    setOptions((os) => {
      const next = [...os];
      next.splice(to, 0, ...next.splice(from, 1));
      return next;
    });
  const submit = () => {
    if (!question.trim()) return p.onInvalid(t("pollEditor.questionRequired"));
    const kept = options.filter((o) => o.label.trim() || o.image);
    if (kind === "multiple_choice") {
      if (kept.some((o) => !o.label.trim())) return p.onInvalid(t("pollEditor.optionRequired"));
      if (kept.length < 2) return p.onInvalid(t("pollEditor.minOptions"));
    }
    const choice = kind === "multiple_choice";
    void p.onCreate(
      {
        kind,
        question: question.trim(),
        language,
        live_at: toISO(liveAt),
        closes_at: toISO(closesAt),
        options: choice ? kept.map((o, position) => ({ label: o.label.trim(), position })) : undefined,
      },
      { question: image, options: choice ? kept.map((o) => o.image) : [] },
    );
  };
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      <Field id={`${ids}-q`} label={t("pollEditor.question")}>
        <Textarea id={`${ids}-q`} rows={2} value={question} placeholder={t("pollEditor.questionPlaceholder")} onChange={(e) => setQuestion(e.target.value)} />
      </Field>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${ids}-lang`} label={t("pollEditor.language")}>
          {langs.length ? (
            <Select value={language} onValueChange={(v) => setLanguage(String(v))}>
              <SelectTrigger id={`${ids}-lang`} className="w-full">
                <SelectValue>{(v: string) => langs.find((l) => l.value === v)?.label ?? v}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {langs.map((l) => (
                  <SelectItem key={l.value} value={l.value}>
                    {l.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : (
            <Input id={`${ids}-lang`} value={language} onChange={(e) => setLanguage(e.target.value)} />
          )}
        </Field>
        {p.freeText && (
          <fieldset className="grid gap-1.5">
            <legend className="mb-1.5 text-sm font-medium">{t("pollEditor.kind")}</legend>
            <div className="flex flex-wrap gap-2">
              {(["multiple_choice", "free_text"] as const).map((k) => (
                <Button key={k} type="button" size="sm" variant={kind === k ? "default" : "outline"} aria-pressed={kind === k} onClick={() => setKind(k)}>
                  {t(k === "free_text" ? "pollEditor.freeText" : "pollEditor.multipleChoice")}
                </Button>
              ))}
            </div>
          </fieldset>
        )}
        <Field id={`${ids}-live`} label={t("pollEditor.liveAt")}>
          <Input id={`${ids}-live`} type="datetime-local" value={liveAt} onChange={(e) => setLiveAt(e.target.value)} />
        </Field>
        <Field id={`${ids}-closes`} label={t("pollEditor.closesAt")}>
          <Input id={`${ids}-closes`} type="datetime-local" value={closesAt} min={liveAt} onChange={(e) => setClosesAt(e.target.value)} />
        </Field>
      </div>
      <ImageField label={t("pollEditor.image")} url={useObjectURL(image)} onPick={setImage} onRemove={() => setImage(null)} />
      {kind === "free_text" ? (
        <p className="text-muted-foreground">{t("pollEditor.freeTextHelp")}</p>
      ) : (
        <fieldset className="grid gap-2">
          <legend className="mb-1.5 text-sm font-medium">{t("pollEditor.options")}</legend>
          <ol className="grid gap-2">
            {options.map((o, i) => (
              <OptionRow
                key={o.key}
                index={i}
                count={options.length}
                label={o.label}
                image={o.image}
                onLabel={(label) => set(o.key, { label })}
                onImage={(image) => set(o.key, { image })}
                onMove={(to) => move(i, to)}
                onRemove={options.length > 2 ? () => setOptions((os) => os.filter((x) => x.key !== o.key)) : undefined}
              />
            ))}
          </ol>
          {options.length < p.maxOptions ? (
            <Button type="button" variant="outline" size="sm" className="justify-self-start" onClick={() => setOptions((os) => [...os, draft()])}>
              {t("pollEditor.addOption")}
            </Button>
          ) : (
            <p className="text-xs text-muted-foreground">{t("pollEditor.maxOptions", { max: p.maxOptions })}</p>
          )}
        </fieldset>
      )}
      <Button type="submit" className="justify-self-start" disabled={p.saving}>
        {p.saving ? t("pollEditor.creating") : t("pollEditor.create")}
      </Button>
    </form>
  );
}

function EditFields(p: {
  poll: Poll;
  saving: boolean;
  maxOptions: number;
  editor: ReturnType<typeof usePollEditor>;
  onError: (e: unknown) => void;
  onInvalid: (message: string) => void;
  onSaved: (poll: Poll) => void;
}) {
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const { poll, editor } = p;
  const [question, setQuestion] = useState(poll.question);
  const [liveAt, setLiveAt] = useState(localInput(new Date(poll.live_at)));
  const [closesAt, setClosesAt] = useState(poll.closes_at ? localInput(new Date(poll.closes_at)) : "");
  const [active, setActive] = useState(poll.is_active);
  const [added, setAdded] = useState("");
  const [removing, setRemoving] = useState<PollOption | null>(null);
  const options = [...poll.options].sort((a, b) => a.position - b.position);
  const patch: PollUpdate = {};
  if (question.trim() !== poll.question) patch.question = question.trim();
  if (toISO(liveAt) !== new Date(localInput(new Date(poll.live_at))).toISOString()) patch.live_at = toISO(liveAt);
  if (closesAt && toISO(closesAt) !== (poll.closes_at && new Date(localInput(new Date(poll.closes_at))).toISOString())) patch.closes_at = toISO(closesAt);
  if (active !== poll.is_active) patch.is_active = active;
  const dirty = Object.keys(patch).length > 0;
  const act = (fn: () => Promise<unknown>) => void fn().catch(p.onError);
  return (
    <div className="grid gap-4">
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (!question.trim()) return p.onInvalid(t("pollEditor.questionRequired"));
          if (dirty) act(async () => p.onSaved(await editor.update(patch)));
        }}
      >
        <Field id={`${ids}-q`} label={t("pollEditor.question")}>
          <Textarea id={`${ids}-q`} rows={2} value={question} onChange={(e) => setQuestion(e.target.value)} />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={`${ids}-live`} label={t("pollEditor.liveAt")}>
            <Input id={`${ids}-live`} type="datetime-local" value={liveAt} onChange={(e) => setLiveAt(e.target.value)} />
          </Field>
          <Field id={`${ids}-closes`} label={t("pollEditor.closesAt")}>
            <Input id={`${ids}-closes`} type="datetime-local" value={closesAt} min={liveAt} onChange={(e) => setClosesAt(e.target.value)} />
          </Field>
        </div>
        <div className="flex items-center gap-3">
          <Switch id={`${ids}-active`} checked={active} onCheckedChange={setActive} />
          <Label htmlFor={`${ids}-active`}>{t(active ? "pollEditor.active" : "pollEditor.inactive")}</Label>
        </div>
        <Button type="submit" className="justify-self-start" disabled={!dirty || p.saving}>
          {p.saving ? t("common.saving") : t("common.save")}
        </Button>
      </form>
      <ImageField label={t("pollEditor.image")} url={poll.image_url} disabled={p.saving} onPick={(f) => act(() => editor.setImage(f))} onRemove={() => act(() => editor.setImage(null))} />
      {poll.kind === "free_text" ? (
        <p className="text-muted-foreground">
          {t("pollEditor.freeTextHelp")} {m.plural("poll.answers", poll.answer_count ?? 0)}
        </p>
      ) : (
        <fieldset className="grid gap-2">
          <legend className="mb-1.5 text-sm font-medium">{t("pollEditor.options")}</legend>
          <ol className="grid gap-2">
            {options.map((o, i) => (
              <OptionRow
                key={o.id}
                index={i}
                count={options.length}
                label={o.label}
                url={o.image_url}
                votes={o.vote_count}
                disabled={p.saving}
                onCommit={(label) => label.trim() && label.trim() !== o.label && act(() => editor.renameOption(o.id, label.trim()))}
                onImage={(f) => act(() => editor.setOptionImage(o.id, f))}
                onMove={(to) => act(() => editor.moveOption(o.id, to))}
                onRemove={options.length > 2 ? () => setRemoving(o) : undefined}
              />
            ))}
          </ol>
          {options.length < p.maxOptions ? (
            <form
              className="flex gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                if (!added.trim()) return;
                act(async () => {
                  await editor.addOption(added.trim());
                  setAdded("");
                });
              }}
            >
              <Input value={added} placeholder={t("pollEditor.newOption")} aria-label={t("pollEditor.newOption")} onChange={(e) => setAdded(e.target.value)} />
              <Button type="submit" variant="outline" disabled={!added.trim() || p.saving}>
                {t("pollEditor.addOption")}
              </Button>
            </form>
          ) : (
            <p className="text-xs text-muted-foreground">{t("pollEditor.maxOptions", { max: p.maxOptions })}</p>
          )}
        </fieldset>
      )}
      <ConfirmDialog
        open={!!removing}
        onOpenChange={(o) => !o && setRemoving(null)}
        title={t("pollEditor.removeOptionTitle")}
        description={t("pollEditor.removeOptionBody")}
        confirm={t("pollEditor.removeOption")}
        onConfirm={() => removing && act(() => editor.removeOption(removing.id))}
      />
    </div>
  );
}

/** One option: move buttons, its label, its image, its votes and remove. */
function OptionRow(p: {
  index: number;
  count: number;
  label: string;
  /** A new poll's staged image. */
  image?: File | null;
  /** An existing option's image. */
  url?: string;
  votes?: number;
  disabled?: boolean;
  /** A new poll's label as typed. */
  onLabel?: (label: string) => void;
  /** An existing option's label, on blur or Enter. */
  onCommit?: (label: string) => void;
  onImage: (file: File | null) => void;
  onMove: (to: number) => void;
  onRemove?: () => void;
}) {
  const m = useMessages();
  const { t } = m;
  const [label, setLabel] = useState(p.label);
  useEffect(() => setLabel(p.label), [p.label]);
  const staged = useObjectURL(p.image ?? null);
  const name = t("pollEditor.option", { index: p.index + 1 });
  return (
    <li className="flex flex-wrap items-center gap-2 rounded-lg border border-border p-2" data-ckui="poll-option">
      <div className="flex flex-col">
        <Button type="button" variant="ghost" size="icon-xs" aria-label={`${t("pollEditor.moveUp")}: ${name}`} disabled={p.disabled || p.index === 0} onClick={() => p.onMove(p.index - 1)}>
          <HugeiconsIcon icon={ArrowUp01Icon} strokeWidth={2} />
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="icon-xs"
          aria-label={`${t("pollEditor.moveDown")}: ${name}`}
          disabled={p.disabled || p.index === p.count - 1}
          onClick={() => p.onMove(p.index + 1)}
        >
          <HugeiconsIcon icon={ArrowDown01Icon} strokeWidth={2} />
        </Button>
      </div>
      <Input
        className="min-w-40 flex-1"
        value={label}
        aria-label={name}
        placeholder={name}
        disabled={p.disabled && !p.onLabel}
        onChange={(e) => {
          setLabel(e.target.value);
          p.onLabel?.(e.target.value);
        }}
        onBlur={() => p.onCommit?.(label)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && p.onCommit) {
            e.preventDefault();
            p.onCommit(label);
          }
        }}
      />
      <ImageField compact label={t("pollEditor.optionImage", { index: p.index + 1 })} url={p.url ?? staged} disabled={p.disabled} onPick={p.onImage} onRemove={() => p.onImage(null)} />
      {p.votes !== undefined && <span className="text-xs tabular-nums text-muted-foreground">{m.plural("poll.votes", p.votes)}</span>}
      {p.onRemove && (
        <Button type="button" variant="ghost" size="icon-sm" aria-label={`${t("pollEditor.removeOption")}: ${name}`} disabled={p.disabled} onClick={p.onRemove}>
          <HugeiconsIcon icon={Delete02Icon} strokeWidth={2} />
        </Button>
      )}
    </li>
  );
}

function ImageField(p: { label: string; url?: string | null; compact?: boolean; disabled?: boolean; onPick: (file: File) => void; onRemove: () => void }) {
  const { t } = useMessages();
  const input = useRef<HTMLInputElement>(null);
  const pick = (
    <input
      ref={input}
      type="file"
      accept="image/*"
      hidden
      aria-label={p.label}
      onChange={(e) => {
        const f = e.target.files?.[0];
        e.target.value = "";
        if (f) p.onPick(f);
      }}
    />
  );
  if (p.compact) {
    return (
      <div className="flex items-center gap-1" data-ckui="poll-image">
        {pick}
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="overflow-hidden p-0"
          aria-label={`${p.url ? t("pollEditor.replaceImage") : t("pollEditor.uploadImage")}: ${p.label}`}
          disabled={p.disabled}
          onClick={() => input.current?.click()}
        >
          {p.url ? <img src={p.url} alt="" className="size-full object-cover" /> : <HugeiconsIcon icon={ImageAdd01Icon} strokeWidth={2} />}
        </Button>
        {p.url && (
          <Button type="button" variant="ghost" size="xs" disabled={p.disabled} onClick={p.onRemove}>
            {t("pollEditor.removeImage")}
          </Button>
        )}
      </div>
    );
  }
  return (
    <div className="grid gap-1.5" data-ckui="poll-image">
      <span className="text-sm font-medium">{p.label}</span>
      {pick}
      {p.url && <img src={p.url} alt="" className="aspect-video w-full max-w-sm rounded-lg border border-border object-cover" />}
      <div className="flex gap-2">
        <Button type="button" variant="outline" size="sm" disabled={p.disabled} onClick={() => input.current?.click()}>
          <HugeiconsIcon icon={ImageAdd01Icon} strokeWidth={2} data-icon="inline-start" />
          {p.url ? t("pollEditor.replaceImage") : t("pollEditor.uploadImage")}
        </Button>
        {p.url && (
          <Button type="button" variant="ghost" size="sm" disabled={p.disabled} onClick={p.onRemove}>
            {t("pollEditor.removeImage")}
          </Button>
        )}
      </div>
    </div>
  );
}

function Field({ id, label, children }: { id: string; label: string; children: ReactNode }) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  );
}

/** An object URL for a staged file, revoked when it changes. */
function useObjectURL(file: File | null): string | undefined {
  const [url, setUrl] = useState<string>();
  useEffect(() => {
    if (!file) return setUrl(undefined);
    const u = URL.createObjectURL(file);
    setUrl(u);
    return () => URL.revokeObjectURL(u);
  }, [file]);
  return url;
}
