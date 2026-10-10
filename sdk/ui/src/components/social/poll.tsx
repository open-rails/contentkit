import { Tick02Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useContext, useId, useState, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import type { Poll as PollData } from "../../client/generated/wire.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext, useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { usePoll } from "../../react/polls.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { Textarea } from "#ckui/ui/textarea";
import { ErrorLine, formatDate } from "./parts.js";

export interface PollProps {
  /** A poll id, or the poll the host already has; none shows the newest live poll (in language). */
  poll?: string | PollData;
  language?: string;
  /** When results show: once the visitor voted (default), or always. A closed poll always shows them. */
  results?: "voted" | "always";
  /** Asks a signed-out visitor to sign in (default the provider's); without one they vote anonymously. */
  onSignIn?: () => void;
  /** Shown when there is no live poll; default nothing. */
  empty?: ReactNode;
  /** Longest free-text answer. Default 280. */
  maxLength?: number;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/**
 * A poll: vote (at once, final) and results scaled to the leading option, or a
 * free-text answer and its groups. Question and option images, the closing
 * date and the closed state come from the poll.
 */
export function Poll(p: PollProps) {
  const m = useMessages();
  const { t } = m;
  const ctx = useContext(ContentKitContext);
  const id = typeof p.poll === "string" ? p.poll : p.poll?.id;
  const r = usePoll(id ?? null, { initial: typeof p.poll === "object" ? p.poll : undefined, language: p.language, client: p.client });
  const report = useErrorReporter(p.onError);
  const [error, setError] = useState<ContentKitError | null>(null);
  const signIn = p.onSignIn ?? ctx?.onSignIn;
  const signedOut = ctx?.viewer === null;
  const root = (children: ReactNode) => (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-3 rounded-xl border border-border bg-card p-4 text-sm text-card-foreground", p.className)} data-ckui="poll">
      {children}
    </ContentKitUiRoot>
  );
  if (r.loading && !r.poll) {
    return root(
      <div className="grid gap-2" aria-busy>
        <Skeleton className="h-5 w-2/3" />
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
      </div>,
    );
  }
  if (r.error && !r.poll) {
    return root(
      <div className="grid justify-items-start gap-2">
        <ErrorLine>{t("poll.loadFailed")}</ErrorLine>
        <Button size="sm" variant="outline" onClick={r.reload}>
          {t("common.retry")}
        </Button>
      </div>,
    );
  }
  const poll = r.poll;
  if (!poll) return p.empty ? <>{p.empty}</> : null;

  const fail = (e: unknown, op: "poll.vote" | "poll.answer") => {
    const err = toContentKitError(e);
    if (err.code === "unauthorized" && signIn) return signIn();
    setError(err);
    report(err, op);
  };
  const vote = (option: string) => {
    if (signedOut && signIn) return signIn();
    setError(null);
    r.vote(option).catch((e) => fail(e, "poll.vote"));
  };
  const header = (
    <>
      {poll.image_url && <img src={poll.image_url} alt="" className="aspect-video w-full rounded-lg object-cover" loading="lazy" />}
      <h3 className="text-base font-semibold" id={`poll-${poll.id}`}>
        {poll.question}
      </h3>
      {poll.closed ? (
        <p className="text-muted-foreground">{t("poll.closed")}</p>
      ) : (
        poll.closes_at && <p className="text-muted-foreground">{t("poll.closes", { date: formatDate(m, poll.closes_at) })}</p>
      )}
    </>
  );
  return root(
    <>
      {header}
      {poll.kind === "free_text" ? (
        <FreeText poll={poll} pending={r.pending} maxLength={p.maxLength ?? 280} results={p.results} onAnswer={(text) =>
            r.answer(text).then(
              () => true,
              (e) => (fail(e, "poll.answer"), false),
            )
          }
        />
      ) : (
        <Choices poll={poll} results={p.results === "always" || poll.voted || poll.closed} canVote={!poll.voted && !poll.closed && !r.pending} onVote={vote} />
      )}
      <ErrorLine error={error} />
      {!poll.voted && !poll.closed && signedOut && signIn && poll.kind !== "free_text" && <p className="text-xs text-muted-foreground">{t("poll.signIn")}</p>}
    </>,
  );
}

function Choices({ poll, results, canVote, onVote }: { poll: PollData; results: boolean; canVote: boolean; onVote: (option: string) => void }) {
  const m = useMessages();
  const { t } = m;
  const options = [...poll.options].sort((a, b) => a.position - b.position);
  const images = options.some((o) => o.image_url);
  const total = poll.total_votes || options.reduce((n, o) => n + o.vote_count, 0);
  const lead = Math.max(1, ...options.map((o) => o.vote_count));
  return (
    <>
      <ul className={cn("grid gap-2", images && "grid-cols-2")} aria-labelledby={`poll-${poll.id}`}>
        {options.map((o) => {
          const mine = poll.my_option === o.id;
          const percent = total ? Math.round((o.vote_count / total) * 100) : 0;
          const body = (
            <>
              {o.image_url && <img src={o.image_url} alt="" className="aspect-[4/3] w-full rounded-md bg-muted object-contain" loading="lazy" />}
              <span className="relative flex w-full items-center gap-2">
                {results && (
                  <span
                    aria-hidden
                    className={cn("absolute inset-y-0 start-0 rounded-md transition-[width] duration-500 motion-reduce:transition-none", mine ? "bg-primary/25" : "bg-muted")}
                    style={{ width: `${(o.vote_count / lead) * 100}%` }}
                  />
                )}
                <span className="relative flex min-w-0 flex-1 items-center gap-2 px-2 py-1.5">
                  {mine && <HugeiconsIcon icon={Tick02Icon} strokeWidth={2.5} className="size-4 shrink-0 text-primary" aria-label={t("poll.yourVote")} />}
                  <span className="min-w-0 flex-1 break-words text-start">{o.label}</span>
                  {results && <span className="shrink-0 tabular-nums font-medium">{percent}%</span>}
                </span>
              </span>
              {results && <span className="sr-only">{m.plural("poll.votes", o.vote_count)}</span>}
            </>
          );
          return (
            <li key={o.id} data-option={o.id} data-mine={mine || undefined}>
              {canVote ? (
                <Button variant="outline" className="h-auto w-full flex-col items-stretch gap-1.5 p-1 whitespace-normal" onClick={() => onVote(o.id)}>
                  {body}
                </Button>
              ) : (
                <div
                  className="grid gap-1.5 rounded-md border border-border p-1"
                  role="meter"
                  aria-valuemin={0}
                  aria-valuemax={100}
                  aria-valuenow={percent}
                  aria-label={o.label}
                >
                  {body}
                </div>
              )}
            </li>
          );
        })}
      </ul>
      {results && <p className="text-xs text-muted-foreground">{m.plural("poll.votes", total)}</p>}
    </>
  );
}

function FreeText({ poll, pending, maxLength, results, onAnswer }: { poll: PollData; pending: boolean; maxLength: number; results?: "voted" | "always"; onAnswer: (text: string) => Promise<boolean> }) {
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const mine = poll.my_answer;
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState(mine?.text ?? "");
  const open = !poll.closed;
  const groups = [...(poll.groups ?? [])].sort((a, b) => b.count - a.count);
  const lead = Math.max(1, ...groups.map((g) => g.count));
  const showGroups = groups.length > 0 && (!!mine || poll.closed || results === "always");
  return (
    <>
      {open && (!mine || editing) ? (
        <form
          className="grid gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (!text.trim()) return;
            void onAnswer(text.trim()).then((ok) => ok && setEditing(false));
          }}
        >
          <label htmlFor={`${ids}-answer`} className="sr-only">
            {t("poll.answerPlaceholder")}
          </label>
          <Textarea id={`${ids}-answer`} rows={2} maxLength={maxLength} placeholder={t("poll.answerPlaceholder")} value={text} disabled={pending} onChange={(e) => setText(e.target.value)} />
          <div className="flex justify-end gap-2">
            {editing && (
              <Button type="button" variant="ghost" size="sm" onClick={() => setEditing(false)}>
                {t("common.cancel")}
              </Button>
            )}
            <Button type="submit" size="sm" disabled={pending || !text.trim()}>
              {t(mine ? "poll.updateAnswer" : "poll.answer")}
            </Button>
          </div>
        </form>
      ) : (
        mine && (
          <div className="grid gap-1 rounded-md bg-muted p-3">
            <p className="text-xs text-muted-foreground">{t("poll.yourAnswer")}</p>
            <p className="break-words whitespace-pre-wrap">{mine.text}</p>
            {open && (
              <Button
                variant="link"
                size="xs"
                className="justify-self-start px-0"
                onClick={() => {
                  setText(mine.text);
                  setEditing(true);
                }}
              >
                {t("poll.editAnswer")}
              </Button>
            )}
          </div>
        )
      )}
      {showGroups && (
        <>
          <ul className="grid gap-2" aria-labelledby={`poll-${poll.id}`}>
            {groups.map((g) => (
              <li key={g.id} className="relative flex items-center gap-2 rounded-md px-2 py-1.5">
                <span aria-hidden className="absolute inset-y-0 start-0 rounded-md bg-muted" style={{ width: `${(g.count / lead) * 100}%` }} />
                <span className="relative min-w-0 flex-1 break-words">{g.label}</span>
                <span className="relative tabular-nums text-muted-foreground">{m.plural("poll.answers", g.count)}</span>
              </li>
            ))}
          </ul>
          {poll.answer_count !== undefined && <p className="text-xs text-muted-foreground">{m.plural("poll.answers", poll.answer_count)}</p>}
        </>
      )}
    </>
  );
}
