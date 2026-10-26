import { Add01Icon, Delete02Icon, ImageAdd01Icon, MoreHorizontalIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { PollState } from "../../client/content/polls.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import type { Poll, PollOption, PollUpdate } from "../../client/generated/wire.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import { useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { usePollEditor } from "../../react/polls.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "#ckui/ui/dropdown-menu";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { Switch } from "#ckui/ui/switch";
import { Textarea } from "#ckui/ui/textarea";
import { localInput } from "./bans.js";
import { SortableList } from "../sortable-list.js";
import { ConfirmDialog, contentError, ErrorLine, formatDate } from "./parts.js";

export interface PollEditorProps {
  /** The poll to edit; none creates one. */
  poll?: string | null;
  /** Languages offered for a new poll: codes, or codes with labels. Without them the language is typed. */
  languages?: readonly (string | { value: string; label: string })[];
  /** A new poll's language. Default the first offered, else "en". */
  defaultLanguage?: string;
  /** Most options. Default 10. */
  maxOptions?: number;
  onCreated?: (poll: Poll) => void;
  onSaved?: (poll: Poll) => void;
  onDeleted?: () => void;
  /** Leaves the editor; with it, the form offers Cancel. */
  onCancel?: () => void;
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

/** A poll's state as the staff list names it. */
export function pollState(p: Pick<Poll, "is_active" | "live_at" | "closes_at">, now = Date.now()): PollState {
  if (!p.is_active) return "inactive";
  if (Date.parse(p.live_at) > now) return "scheduled";
  if (p.closes_at && Date.parse(p.closes_at) <= now) return "closed";
  return "live";
}

const dot: Record<PollState, string> = { live: "bg-success", scheduled: "bg-sky-500", closed: "bg-muted-foreground/50", inactive: "ring-1 ring-inset ring-muted-foreground" };

/** A poll's state as a coloured dot and its name, like the content list's status. */
export function PollStateDot({ state, className }: { state: PollState; className?: string }) {
  const { t } = useMessages();
  return (
    <span className={cn("inline-flex items-center gap-2 whitespace-nowrap font-medium text-foreground", className)}>
      <span aria-hidden className={cn("size-2 shrink-0 rounded-full", dot[state])} />
      {t(`pollManager.${state}`)}
    </span>
  );
}

/**
 * Staff: creates or edits a poll (root:polls:write): question, language, type, when
 * it goes live and closes, visibility, a question image, and options with
 * images, reordered with the move buttons. A new poll is written at once with
 * its images; an existing one saves its fields with Save and each option
 * change as it is made. A live preview shows it as voters will.
 */
export function PollEditor(p: PollEditorProps) {
  const m = useMessages();
  const { t } = m;
  const ed = usePollEditor(p.poll ?? null, { client: p.client });
  const report = useErrorReporter(p.onError);
  const [error, setError] = useState<ContentKitError | null>(null);
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
        <Skeleton className="h-9 w-64" />
        <Skeleton className="h-40 w-full" />
      </div>,
    );
  }
  if (ed.error && !ed.poll) return root(<ErrorLine error={ed.error} />);
  const status = (
    <>
      {error && <ErrorLine>{contentError(m, error)}</ErrorLine>}
      {notice && !error && (
        <p role="status" className="text-sm text-muted-foreground">
          {notice}
        </p>
      )}
    </>
  );
  return root(
    <>
      {ed.poll ? (
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="grid gap-1">
            <h2 className="text-2xl font-semibold tracking-tight">{t("pollEditor.editTitle")}</h2>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-muted-foreground">
              <PollStateDot state={pollState(ed.poll)} />
              <span aria-hidden>·</span>
              <span className="tabular-nums">{m.plural("poll.votes", ed.poll.total_votes)}</span>
            </div>
          </div>
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button size="icon-sm" variant="ghost" aria-label={t("contentManager.moreActions", { title: ed.poll.question })} />}>
              <HugeiconsIcon icon={MoreHorizontalIcon} strokeWidth={2} />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="min-w-40">
              <DropdownMenuItem className="text-destructive focus:text-destructive" disabled={ed.saving} onClick={() => setDeleting(true)}>
                <HugeiconsIcon icon={Delete02Icon} strokeWidth={2} />
                {t("pollEditor.delete")}
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      ) : (
        <div className="grid gap-1">
          <h2 className="text-2xl font-semibold tracking-tight">{t("pollEditor.newTitle")}</h2>
          <p className="text-muted-foreground">{t("pollEditor.newIntro")}</p>
        </div>
      )}
      {ed.poll ? (
        <EditFields
          key={ed.poll.id}
          poll={ed.poll}
          saving={ed.saving}
          maxOptions={p.maxOptions ?? 10}
          editor={ed}
          status={status}
          onCancel={p.onCancel}
          onError={fail}
          onAttempt={() => setError(null)}
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
          maxOptions={p.maxOptions ?? 10}
          status={status}
          onCancel={p.onCancel}
          onAttempt={() => setError(null)}
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
      <ConfirmDialog
        open={deleting}
        onOpenChange={setDeleting}
        title={t("pollEditor.deleteTitle")}
        description={t("pollEditor.deleteBody")}
        confirm={t("pollEditor.delete")}
        typed={t("admin.deleteWord")}
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

// Cards on the left, the live preview pinned on the right from lg up.
const LAYOUT = "grid items-start gap-6 lg:grid-cols-[minmax(0,1fr)_19rem]";

function CreateFields(p: {
  saving: boolean;
  languages?: PollEditorProps["languages"];
  defaultLanguage?: string;
  maxOptions: number;
  status: ReactNode;
  onCancel?: () => void;
  onAttempt: () => void;
  onCreate: (input: Parameters<ReturnType<typeof usePollEditor>["create"]>[0], images: { question: File | null; options: (File | null)[] }) => Promise<void>;
}) {
  const { t } = useMessages();
  const ids = useId();
  const languageName = useLanguageName();
  const langs = useMemo(() => (p.languages ?? []).map((l) => (typeof l === "string" ? { value: l, label: languageName(l) } : l)), [p.languages, languageName]);
  const [question, setQuestion] = useState("");
  const [language, setLanguage] = useState(p.defaultLanguage ?? langs[0]?.value ?? "en");
  const [liveAt, setLiveAt] = useState(() => localInput(new Date()));
  const [closesAt, setClosesAt] = useState("");
  const [image, setImage] = useState<File | null>(null);
  const [options, setOptions] = useState<Draft[]>(() => [draft(), draft()]);
  const [tried, setTried] = useState(false);
  const questionRef = useRef<HTMLTextAreaElement>(null);
  const imageUrl = useObjectURL(image);
  const set = (key: string, patch: Partial<Draft>) => setOptions((os) => os.map((o) => (o.key === key ? { ...o, ...patch } : o)));
  const move = (from: number, to: number) =>
    setOptions((os) => {
      const next = [...os];
      next.splice(to, 0, ...next.splice(from, 1));
      return next;
    });
  const kept = options.filter((o) => o.label.trim() || o.image);
  // Shown after the first Create and gone as soon as the field is fixed.
  const questionError = tried && !question.trim() ? t("pollEditor.questionRequired") : null;
  const unlabelled = kept.some((o) => !o.label.trim());
  const tooFew = !unlabelled && kept.length < 2;
  const optionsError = !tried ? null : unlabelled ? t("pollEditor.optionRequired") : tooFew ? t("pollEditor.minOptions") : null;
  const submit = () => {
    p.onAttempt();
    setTried(true);
    if (!question.trim()) return questionRef.current?.focus();
    if (unlabelled || tooFew) return;
    void p.onCreate(
      {
        kind: "multiple_choice",
        question: question.trim(),
        language,
        live_at: toISO(liveAt),
        closes_at: toISO(closesAt),
        options: kept.map((o, position) => ({ label: o.label.trim(), position })),
      },
      { question: image, options: kept.map((o) => o.image) },
    );
  };
  return (
    <form
      className={LAYOUT}
      data-ckui="poll-create"
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      <div className="grid min-w-0 gap-5">
        <Card title={<Required>{t("pollEditor.question")}</Required>} hint={t("pollEditor.questionHint")} ckui="poll-question">
          <Field id={`${ids}-q`} label={t("pollEditor.question")} hideLabel error={questionError}>
            <Textarea
              ref={questionRef}
              id={`${ids}-q`}
              rows={2}
              autoFocus
              value={question}
              placeholder={t("pollEditor.questionPlaceholder")}
              aria-invalid={!!questionError || undefined}
              aria-describedby={questionError ? `${ids}-q-error` : undefined}
              onChange={(e) => setQuestion(e.target.value)}
            />
          </Field>
          <ImageField label={t("pollEditor.image")} url={imageUrl} onPick={setImage} onRemove={() => setImage(null)} />
        </Card>
        <Card title={t("pollEditor.options")} hint={t("pollEditor.optionsHint")} ckui="poll-options">
          <fieldset className="grid gap-2" aria-describedby={optionsError ? `${ids}-options-error` : undefined}>
            <legend className="sr-only">{t("pollEditor.options")}</legend>
            <SortableList
              items={options}
              id={(o) => o.key}
              name={(o) => o.label || t("pollEditor.option", { index: options.indexOf(o) + 1 })}
              onMove={move}
              label={t("pollEditor.options")}
              className="gap-2"
              row={() => ({ className: OPTION_ROW, "data-ckui": "poll-option" })}
            >
              {(o, handle, i) => (
                <OptionRow
                  handle={handle}
                  index={i}
                  label={o.label}
                  image={o.image}
                  invalid={!!optionsError && !o.label.trim() && (tooFew || !!o.image)}
                  onLabel={(label) => set(o.key, { label })}
                  onImage={(image) => set(o.key, { image })}
                  onRemove={options.length > 2 ? () => setOptions((os) => os.filter((x) => x.key !== o.key)) : undefined}
                />
              )}
            </SortableList>
            {options.length < p.maxOptions ? (
              <Button type="button" variant="outline" size="sm" className="justify-self-start" onClick={() => setOptions((os) => [...os, draft()])}>
                <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
                {t("pollEditor.addOption")}
              </Button>
            ) : (
              <p className="text-xs text-muted-foreground">{t("pollEditor.maxOptions", { max: p.maxOptions })}</p>
            )}
            {optionsError && (
              <FieldError id={`${ids}-options-error`} alert>
                {optionsError}
              </FieldError>
            )}
          </fieldset>
        </Card>
        <Card title={t("pollEditor.schedule")} hint={t("pollEditor.scheduleHint")} ckui="poll-schedule">
          <div className="grid gap-4 sm:grid-cols-3">
            <Field id={`${ids}-lang`} label={t("pollEditor.language")}>
              {langs.length ? (
                <Select value={language} onValueChange={(v) => setLanguage(String(v))}>
                  <SelectTrigger id={`${ids}-lang`} className="h-10 w-full">
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
                <Input id={`${ids}-lang`} className="h-10" value={language} onChange={(e) => setLanguage(e.target.value)} />
              )}
            </Field>
            <Field id={`${ids}-live`} label={t("pollEditor.liveAt")}>
              <Input id={`${ids}-live`} className="h-10" type="datetime-local" value={liveAt} onChange={(e) => setLiveAt(e.target.value)} />
            </Field>
            <Field id={`${ids}-closes`} label={t("pollEditor.closesAt")}>
              <Input id={`${ids}-closes`} className="h-10" type="datetime-local" value={closesAt} min={liveAt} onChange={(e) => setClosesAt(e.target.value)} />
            </Field>
          </div>
        </Card>
        {p.status}
        <div className="flex flex-wrap items-center justify-end gap-3">
          <p className="me-auto text-xs text-muted-foreground">
            <span className="text-destructive">*</span> {t("entityEditor.required")}
          </p>
          {p.onCancel && (
            <Button type="button" variant="outline" disabled={p.saving} onClick={p.onCancel}>
              {t("common.cancel")}
            </Button>
          )}
          <Button type="submit" disabled={p.saving}>
            <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
            {p.saving ? t("pollEditor.creating") : t("pollEditor.create")}
          </Button>
        </div>
      </div>
      <Preview
        question={question}
        imageUrl={imageUrl}
        state={pollState({ is_active: true, live_at: toISO(liveAt) ?? new Date().toISOString(), closes_at: toISO(closesAt) })}
        options={options.map((o, i) => ({ key: o.key, label: o.label.trim() || t("pollEditor.option", { index: i + 1 }), empty: !o.label.trim() }))}
        closesAt={toISO(closesAt)}
      />
    </form>
  );
}

function EditFields(p: {
  poll: Poll;
  saving: boolean;
  maxOptions: number;
  editor: ReturnType<typeof usePollEditor>;
  status: ReactNode;
  onCancel?: () => void;
  onError: (e: unknown) => void;
  onAttempt: () => void;
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
  const [tried, setTried] = useState(false);
  const questionRef = useRef<HTMLTextAreaElement>(null);
  const languageName = useLanguageName();
  const options = [...poll.options].sort((a, b) => a.position - b.position);
  const patch: PollUpdate = {};
  if (question.trim() !== poll.question) patch.question = question.trim();
  if (toISO(liveAt) !== new Date(localInput(new Date(poll.live_at))).toISOString()) patch.live_at = toISO(liveAt);
  if (closesAt && toISO(closesAt) !== (poll.closes_at && new Date(localInput(new Date(poll.closes_at))).toISOString())) patch.closes_at = toISO(closesAt);
  else if (!closesAt && poll.closes_at) patch.closes_at = null;
  if (active !== poll.is_active) patch.is_active = active;
  const dirty = Object.keys(patch).length > 0;
  const act = (fn: () => Promise<unknown>) => void fn().catch(p.onError);
  const questionError = tried && !question.trim() ? t("pollEditor.questionRequired") : null;
  const save = () => {
    p.onAttempt();
    setTried(true);
    if (!question.trim()) return questionRef.current?.focus();
    if (dirty) act(async () => p.onSaved(await editor.update(patch)));
  };
  return (
    <div className={LAYOUT} data-ckui="poll-edit">
      <div className="grid min-w-0 gap-5">
        <Card title={<Required>{t("pollEditor.question")}</Required>} hint={t("pollEditor.questionHint")} ckui="poll-question">
          <Field id={`${ids}-q`} label={t("pollEditor.question")} hideLabel error={questionError}>
            <Textarea
              ref={questionRef}
              id={`${ids}-q`}
              rows={2}
              value={question}
              aria-invalid={!!questionError || undefined}
              aria-describedby={questionError ? `${ids}-q-error` : undefined}
              onChange={(e) => setQuestion(e.target.value)}
            />
          </Field>
          <ImageField label={t("pollEditor.image")} url={poll.image_url} disabled={p.saving} onPick={(f) => act(() => editor.setImage(f))} onRemove={() => act(() => editor.setImage(null))} />
        </Card>
        <Card title={t("pollEditor.options")} hint={t("pollEditor.optionsLive")} ckui="poll-options">
          <fieldset className="grid gap-2">
            <legend className="sr-only">{t("pollEditor.options")}</legend>
            <SortableList
              items={options}
              id={(o) => o.id}
              name={(o) => o.label}
              disabled={p.saving}
              onMove={(from, to) => act(() => editor.moveOption(options[from]!.id, to))}
              label={t("pollEditor.options")}
              className="gap-2"
              row={() => ({ className: OPTION_ROW, "data-ckui": "poll-option" })}
            >
              {(o, handle, i) => (
                <OptionRow
                  handle={handle}
                  index={i}
                  label={o.label}
                  url={o.image_url}
                  votes={o.vote_count}
                  disabled={p.saving}
                  onCommit={(label) => label.trim() && label.trim() !== o.label && act(() => editor.renameOption(o.id, label.trim()))}
                  onImage={(f) => act(() => editor.setOptionImage(o.id, f))}
                  onRemove={options.length > 2 ? () => setRemoving(o) : undefined}
                />
              )}
            </SortableList>
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
                <Input className="h-10" value={added} placeholder={t("pollEditor.newOption")} aria-label={t("pollEditor.newOption")} onChange={(e) => setAdded(e.target.value)} />
                <Button type="submit" variant="outline" className="h-10" disabled={!added.trim() || p.saving}>
                  <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
                  {t("pollEditor.addOption")}
                </Button>
              </form>
            ) : (
              <p className="text-xs text-muted-foreground">{t("pollEditor.maxOptions", { max: p.maxOptions })}</p>
            )}
          </fieldset>
        </Card>
        <Card title={t("pollEditor.schedule")} hint={t("pollEditor.scheduleHint")} ckui="poll-schedule">
          <div className="grid gap-4 sm:grid-cols-3">
            {/* A poll's language is fixed once it exists. */}
            <div className="grid gap-1.5">
              <span className="text-sm font-medium">{t("pollEditor.language")}</span>
              <span className="flex h-10 items-center rounded-md border border-input bg-muted/30 px-3 text-muted-foreground">{languageName(poll.language)}</span>
            </div>
            <Field id={`${ids}-live`} label={t("pollEditor.liveAt")}>
              <Input id={`${ids}-live`} className="h-10" type="datetime-local" value={liveAt} onChange={(e) => setLiveAt(e.target.value)} />
            </Field>
            <Field id={`${ids}-closes`} label={t("pollEditor.closesAt")}>
              <Input id={`${ids}-closes`} className="h-10" type="datetime-local" value={closesAt} min={liveAt} onChange={(e) => setClosesAt(e.target.value)} />
            </Field>
          </div>
          <div className="flex items-start justify-between gap-4 rounded-md border border-border p-4">
            <div className="grid gap-1">
              <Label htmlFor={`${ids}-active`}>{t(active ? "pollEditor.active" : "pollEditor.inactive")}</Label>
              <p className="text-xs text-muted-foreground">{t("pollEditor.visibleHint")}</p>
            </div>
            <Switch id={`${ids}-active`} checked={active} onCheckedChange={setActive} />
          </div>
        </Card>
        {p.status}
        <div className="flex flex-wrap items-center justify-end gap-3">
          <p className="me-auto text-xs text-muted-foreground">
            <span className="text-destructive">*</span> {t("entityEditor.required")}
          </p>
          {p.onCancel && (
            <Button type="button" variant="outline" disabled={p.saving} onClick={p.onCancel}>
              {t("common.cancel")}
            </Button>
          )}
          <Button type="button" variant={dirty ? "default" : "outline"} disabled={!dirty || p.saving} onClick={save}>
            {p.saving ? t("common.saving") : t("common.save")}
          </Button>
        </div>
      </div>
      <Preview
        question={question.trim() || poll.question}
        imageUrl={poll.image_url}
        state={pollState({ is_active: active, live_at: toISO(liveAt) ?? poll.live_at, closes_at: toISO(closesAt) })}
        total={poll.total_votes}
        options={options.map((o) => ({ key: o.id, label: o.label, votes: o.vote_count, image: o.image_url }))}
        closesAt={toISO(closesAt)}
      />
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

/** The poll as voters will see it: the question and its options, with the results so far once there are votes. */
function Preview(p: {
  question: string;
  imageUrl?: string;
  state: PollState;
  total?: number;
  options: { key: string; label: string; empty?: boolean; votes?: number; image?: string }[];
  closesAt?: string;
}) {
  const m = useMessages();
  const { t } = m;
  const total = p.total ?? 0;
  const percent = new Intl.NumberFormat(m.language, { style: "percent", maximumFractionDigits: 0 });
  return (
    <aside className="hidden lg:sticky lg:top-[calc(var(--ckui-sticky-top,0px)+1rem)] lg:block" aria-label={t("pollEditor.preview")} data-ckui="poll-preview">
      <div className="overflow-hidden rounded-lg border border-border bg-muted/30">
        <div className="flex items-center justify-between gap-2 border-b border-border px-5 py-3">
          <p className="text-xs font-medium tracking-wide text-muted-foreground uppercase">{t("pollEditor.preview")}</p>
          <PollStateDot state={p.state} className="text-xs" />
        </div>
        <div className="grid gap-4 p-5">
          {p.imageUrl && <img src={p.imageUrl} alt="" className="aspect-video w-full rounded-md border border-border object-cover" />}
          <p className={cn("font-semibold break-words", !p.question.trim() && "text-muted-foreground")}>{p.question.trim() || t("pollEditor.questionPlaceholder")}</p>
          <ul className="grid gap-2">
            {p.options.map((o) => {
              const share = total ? (o.votes ?? 0) / total : 0;
              return (
                <li key={o.key} className="relative overflow-hidden rounded-md border border-border px-3 py-2">
                  {total > 0 && <span aria-hidden className="absolute inset-y-0 start-0 bg-primary/15" style={{ width: `${share * 100}%` }} />}
                  <span className="relative flex items-center gap-2">
                    {o.image && <img src={o.image} alt="" className="size-6 shrink-0 rounded object-cover" />}
                    <span className={cn("min-w-0 flex-1 truncate", o.empty && "text-muted-foreground")}>{o.label}</span>
                    {total > 0 && <span className="text-xs tabular-nums text-muted-foreground">{percent.format(share)}</span>}
                  </span>
                </li>
              );
            })}
          </ul>
          <p className="text-xs text-muted-foreground">
            {m.plural("poll.votes", total)}
            {p.closesAt && ` · ${t("poll.closes", { date: formatDate(m, p.closesAt) })}`}
          </p>
        </div>
      </div>
    </aside>
  );
}

/** A card with a heading, an optional one-line hint and an optional control beside them. */
function Card({ title, hint, ckui, aside, children }: { title: ReactNode; hint?: ReactNode; ckui: string; aside?: ReactNode; children: ReactNode }) {
  return (
    <section className="grid gap-4 rounded-lg border border-border bg-muted/30 p-5" data-ckui={ckui}>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="grid gap-0.5">
          <h3 className="text-sm font-medium">{title}</h3>
          {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
        </div>
        {aside}
      </div>
      {children}
    </section>
  );
}

const OPTION_ROW = "flex flex-wrap items-center gap-2 rounded-md border border-border bg-background/40 p-2";

/** One option's content: its drag handle, label, image, votes and remove. */
function OptionRow(p: {
  handle: ReactNode;
  index: number;
  label: string;
  /** A new poll's staged image. */
  image?: File | null;
  /** An existing option's image. */
  url?: string;
  votes?: number;
  disabled?: boolean;
  invalid?: boolean;
  /** A new poll's label as typed. */
  onLabel?: (label: string) => void;
  /** An existing option's label, on blur or Enter. */
  onCommit?: (label: string) => void;
  onImage: (file: File | null) => void;
  onRemove?: () => void;
}) {
  const m = useMessages();
  const { t } = m;
  const [label, setLabel] = useState(p.label);
  useEffect(() => setLabel(p.label), [p.label]);
  const staged = useObjectURL(p.image ?? null);
  const name = t("pollEditor.option", { index: p.index + 1 });
  return (
    <>
      {p.handle}
      <Input
        className="h-9 min-w-40 flex-1"
        value={label}
        aria-label={name}
        aria-invalid={p.invalid || undefined}
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
      {p.votes !== undefined && <span className="w-16 text-end text-xs tabular-nums text-muted-foreground">{m.plural("poll.votes", p.votes)}</span>}
      {p.onRemove && (
        <Button type="button" variant="ghost" size="icon-sm" aria-label={`${t("pollEditor.removeOption")}: ${name}`} disabled={p.disabled} onClick={p.onRemove}>
          <HugeiconsIcon icon={Delete02Icon} strokeWidth={2} />
        </Button>
      )}
    </>
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
  // A landscape slot: the picked image, or a dashed drop target that opens the picker.
  return (
    <div className="grid gap-1.5" data-ckui="poll-image">
      <span className="text-sm font-medium">
        {p.label}
        <span className="font-normal text-muted-foreground"> · {t("entityEditor.optional")}</span>
      </span>
      {pick}
      <div className="flex flex-wrap items-end gap-3">
        {p.url ? (
          <img src={p.url} alt="" className="aspect-video w-48 rounded-md border border-border object-cover" />
        ) : (
          <button
            type="button"
            disabled={p.disabled}
            onClick={() => input.current?.click()}
            className="grid aspect-video w-48 place-items-center gap-1 rounded-md border border-dashed border-border text-xs text-muted-foreground transition-colors outline-none hover:border-ring hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            <span className="grid justify-items-center gap-1">
              <HugeiconsIcon icon={ImageAdd01Icon} strokeWidth={2} className="size-5" />
              {t("pollEditor.uploadImage")}
            </span>
          </button>
        )}
        {p.url && (
          <div className="flex gap-2">
            <Button type="button" variant="outline" size="sm" disabled={p.disabled} onClick={() => input.current?.click()}>
              {t("pollEditor.replaceImage")}
            </Button>
            <Button type="button" variant="ghost" size="sm" disabled={p.disabled} onClick={p.onRemove}>
              {t("pollEditor.removeImage")}
            </Button>
          </div>
        )}
      </div>
    </div>
  );
}

/** A label with the red required mark after it. */
function Required({ children }: { children: ReactNode }) {
  return (
    <>
      {children}
      <span aria-hidden className="text-destructive">
        {" "}
        *
      </span>
    </>
  );
}

/** A labelled control; hideLabel when its card's heading already names it. */
function Field({ id, label, hideLabel, error, children }: { id: string; label: string; hideLabel?: boolean; error?: string | null; children: ReactNode }) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id} className={hideLabel ? "sr-only" : undefined}>
        {label}
      </Label>
      {children}
      {error && <FieldError id={`${id}-error`}>{error}</FieldError>}
    </div>
  );
}

function FieldError({ id, alert, children }: { id: string; alert?: boolean; children: ReactNode }) {
  return (
    <p id={id} role={alert ? "alert" : undefined} className="text-xs text-destructive">
      {children}
    </p>
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
