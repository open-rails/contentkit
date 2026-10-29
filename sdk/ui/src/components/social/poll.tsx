import { Tick02Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useContext, useState, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import type { Poll as PollData } from "../../client/generated/wire.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext, useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { useContentConfig } from "../../react/config.js";
import { usePoll } from "../../react/polls.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { ErrorLine, formatDate } from "./parts.js";

export interface PollProps {
  /** A poll id, or the poll the host already has; none shows the newest live poll (in language). */
  poll?: string | PollData;
  language?: string;
  /** When results show: once the visitor voted (default), or always. A closed poll always shows them. */
  results?: "voted" | "always";
  /** Asks a signed-out visitor to sign in where the server takes no anonymous votes (default the provider's). */
  onSignIn?: () => void;
  /** Shown when there is no live poll; default nothing. */
  empty?: ReactNode;
  /** Host action alongside the poll's date, totals and sign-in action. */
  footer?: ReactNode;
  /** Embedded omits the card surface and uses a larger heading on wider screens. */
  variant?: "card" | "embedded";
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** A predefined-choice poll with final votes, results and question/option images. */
export function Poll(p: PollProps) {
  const m = useMessages();
  const { t } = m;
  const ctx = useContext(ContentKitContext);
  const id = typeof p.poll === "string" ? p.poll : p.poll?.id;
  const r = usePoll(id ?? null, { initial: typeof p.poll === "object" ? p.poll : undefined, language: p.language, client: p.client });
  const report = useErrorReporter(p.onError);
  const [error, setError] = useState<ContentKitError | null>(null);
  const { config } = useContentConfig({ client: p.client });
  const signIn = p.onSignIn ?? ctx?.onSignIn;
  const signedOut = ctx?.viewer === null;
  // Signed out where the server takes no anonymous votes.
  const mustSignIn = signedOut && config?.anonymous.votes === false;
  const root = (children: ReactNode) => (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid min-w-0 content-start gap-3 text-sm text-card-foreground", p.variant !== "embedded" && "rounded-xl border border-border bg-card p-4", p.className)} data-ckui="poll">
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

  const fail = (e: unknown, op: "poll.vote") => {
    const err = toContentKitError(e);
    if (err.code === "unauthorized" && signIn) return signIn();
    setError(err);
    report(err, op);
  };
  const vote = (option: string) => {
    if (mustSignIn) return signIn?.();
    setError(null);
    r.vote(option).catch((e) => fail(e, "poll.vote"));
  };
  const header = (
    <>
      {poll.image_url && <img src={poll.image_url} alt="" className="aspect-video w-full rounded-lg object-cover" loading="lazy" />}
      <h3 className={cn("text-base leading-[1.4] font-semibold [overflow-wrap:anywhere]", p.variant === "embedded" ? "text-balance sm:text-lg" : "border-b border-border pb-2")} id={`poll-${poll.id}`}>
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
      <Choices
        poll={poll}
        results={p.results === "always" || poll.voted || poll.closed}
        canVote={!poll.voted && !poll.closed && !r.pending && (!mustSignIn || !!signIn)}
        onVote={vote}
      />
      <ErrorLine error={error} />
      <PollFooter
        poll={poll}
        results={p.results === "always" || poll.voted || poll.closed}
        signIn={!poll.voted && !poll.closed && mustSignIn ? (signIn ? (
          <Button size="sm" className="min-h-11 max-w-full justify-self-start whitespace-normal sm:min-h-8" onClick={signIn}>
            {t("poll.signIn")}
          </Button>
        ) : <p className="text-xs text-muted-foreground">{t("poll.signIn")}</p>) : undefined}
        action={p.footer}
      />
    </>,
  );
}

function Choices({ poll, results, canVote, onVote }: { poll: PollData; results: boolean; canVote: boolean; onVote: (option: string) => void }) {
  const m = useMessages();
  const { t } = m;
  const options = [...poll.options].sort((a, b) => a.position - b.position);
  const images = options.some((o) => o.image_url);
  const total = poll.total_votes || options.reduce((n, o) => n + o.vote_count, 0);
  // Bars scale to the leading option, which spans the full width.
  const lead = Math.max(1, ...options.map((o) => o.vote_count));
  return (
    <>
      {canVote && <p className="text-xs text-muted-foreground">{t("poll.choose")}</p>}
      <ul className={cn("grid gap-2 sm:gap-3", images && "grid-cols-2")} aria-labelledby={`poll-${poll.id}`}>
        {options.map((o) => {
          const mine = poll.my_option === o.id;
          const percent = total ? Math.round((o.vote_count / total) * 100) : 0;
          const choose = canVote ? () => onVote(o.id) : undefined;
          const meter = results && !choose ? { role: "meter", "aria-valuemin": 0, "aria-valuemax": 100, "aria-valuenow": percent, "aria-label": o.label } : {};
          const body = (
            <>
              {o.image_url && <img src={o.image_url} alt="" className="aspect-[4/3] w-full rounded-md bg-muted object-contain" loading="lazy" />}
              <span className="flex w-full flex-wrap items-baseline gap-x-2 gap-y-1">
                {mine && <HugeiconsIcon icon={Tick02Icon} strokeWidth={2.5} className="size-4 shrink-0 self-center text-primary" aria-label={t("poll.yourVote")} />}
                <span className="min-w-0 flex-1 text-start font-medium [overflow-wrap:anywhere]">{o.label}</span>
                {results && (
                  <span className="ms-auto shrink-0 text-xs tabular-nums text-muted-foreground">
                    <span className="font-semibold text-foreground">{percent}%</span> · {m.plural("poll.votes", o.vote_count)}
                  </span>
                )}
              </span>
              {results && (
                <span aria-hidden className="block h-2 w-full overflow-hidden rounded-full bg-muted">
                  <span className={cn("block h-full rounded-full transition-[width] duration-500 motion-reduce:transition-none", mine ? "bg-primary" : "bg-foreground/40")} style={{ width: `${(o.vote_count / lead) * 100}%` }} />
                </span>
              )}
            </>
          );
          return (
            <li key={o.id} data-option={o.id} data-mine={mine || undefined} {...meter}>
              {choose ? (
                <button type="button" className="grid min-h-11 w-full gap-1.5 rounded-md border border-border p-2 text-start hover:border-primary/60 hover:bg-muted/60 focus-visible:outline-2 focus-visible:outline-ring" onClick={choose}>
                  {body}
                </button>
              ) : (
                <div className="grid gap-1.5 p-1.5">{body}</div>
              )}
            </li>
          );
        })}
      </ul>
      {poll.voted && !poll.closed && <p className="text-xs text-muted-foreground">{t("poll.final")}</p>}
    </>
  );
}

/** Keep host navigation and voting actions in the poll's own layout. */
function PollFooter({ poll, results, signIn, action }: { poll: PollData; results: boolean; signIn?: ReactNode; action?: ReactNode }) {
  const m = useMessages();
  const total = poll.total_votes || poll.options.reduce((n, option) => n + option.vote_count, 0);
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-2 border-t border-border pt-2" data-ckui="poll-footer">
      <p className={cn("flex min-w-0 flex-wrap justify-between gap-x-3 gap-y-1 text-xs tabular-nums text-muted-foreground", signIn || !action ? "w-full" : "flex-1")}>
        <time dateTime={poll.live_at}>{formatDate(m, poll.live_at, false)}</time>
        {results && <span>{m.plural("poll.votes", total)}</span>}
      </p>
      {signIn}
      {action && <div className="ms-auto shrink-0">{action}</div>}
    </div>
  );
}
