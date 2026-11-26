import { Cancel01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useId, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { FeedbackReason, FeedbackTarget } from "../../client/feedback.js";
import type { FeedbackEntity } from "../../client/generated/wire.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { useMessages } from "../../i18n/context.js";
import { useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { useReactionFeedback, type UseReactionFeedback } from "../../react/reaction-feedback.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Label } from "#ckui/ui/label";
import { Switch } from "#ckui/ui/switch";
import { Textarea } from "#ckui/ui/textarea";

const MAX_TEXT = 280;

export interface ReactionFeedbackPromptProps {
  item: FeedbackTarget;
  /** Ask after each like, favorite or dislike of the item made through the client. Default true. */
  follow?: boolean;
  /** A prompt state the host drives itself (useReactionFeedback); the component then asks nothing. */
  state?: UseReactionFeedback;
  /** Where rule violations are reported: a dislike prompt links it when given. */
  reportHref?: string;
  onReport?: () => void;
  /** The reader's content filter settings, linked beside the hide toggle. */
  filtersHref?: string;
  /** Follows an artist or series; without it no follow button shows. */
  onFollow?: (entity: FeedbackEntity) => void;
  /** A tag's page to browse; default its path. */
  entityHref?: (entity: FeedbackEntity) => string | undefined;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** One answer chip: a reason, and for "not_into" the tag it names. */
interface Choice {
  reason: FeedbackReason;
  tag?: FeedbackEntity;
}

/**
 * "What did you like?" or "What didn't you like?" after a reaction; the
 * server decides whether to ask. "Not into ‹tag›" on a tag the filters can
 * hide offers to hide it, never preselected. Renders nothing without a prompt.
 */
export function ReactionFeedbackPrompt({ item, follow = true, state, reportHref, onReport, filtersHref, onFollow, entityHref, client, onError, className, appearance }: ReactionFeedbackPromptProps) {
  const m = useMessages();
  const { t } = m;
  const own = useReactionFeedback(item, { client, follow: follow && !state });
  const f = state ?? own;
  const p = f.prompt;
  const report = useErrorReporter(onError);
  const ids = useId();
  const [choice, setChoice] = useState<Choice>();
  const [entity, setEntity] = useState<string>();
  const [text, setText] = useState("");
  const [hide, setHide] = useState(false);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    setChoice(undefined);
    setEntity(undefined);
    setText("");
    setHide(false);
    setFailed(false);
  }, [p?.content_id, p?.offered_at]);
  if (!p) return null;

  const dislike = p.polarity === "dislike";
  const reason = choice?.reason;
  const choices = p.reasons.flatMap((r): Choice[] => (r === "not_into" ? p.tags.map((tag) => ({ reason: r, tag })) : [{ reason: r }]));
  const same = (c: Choice) => c.reason === reason && c.tag?.id === choice?.tag?.id;
  // A like names a tag, artist or series; a dislike's tag comes with its chip.
  const entities = dislike ? [] : reason === "tag" ? p.tags : reason === "artist" ? p.artists : reason === "series" ? p.series : [];
  const named = choice?.tag?.id ?? entity;
  const ready = !!choice && (entities.length === 0 || (reason !== "tag" && entities.length === 1) || !!entity);
  const hideable = choice?.tag?.hideable ? choice.tag : undefined;
  const done = !!p.answer;
  const submit = async () => {
    if (!choice) return;
    setFailed(false);
    try {
      await f.answer({ reason: choice.reason, entity_id: named, text: choice.reason === "other" && text.trim() ? text.trim() : undefined, hide: hideable ? hide : undefined });
    } catch (e) {
      setFailed(true);
      report(e, "feedback.save");
    }
  };
  const label = (c: Choice) => (c.tag ? t("feedback.reasons.not_into", { tag: c.tag.name }) : t(`feedback.reasons.${c.reason as Exclude<FeedbackReason, "not_into">}`));
  const followUp = p.follow_up;
  const browse = followUp?.action === "browse" ? (entityHref ? entityHref(followUp.entity) : followUp.entity.path) : undefined;

  return (
    <ContentKitUiRoot appearance={appearance} className={cn("relative rounded-lg border border-border bg-card p-3 text-sm text-card-foreground", className)} data-ckui="reaction-feedback">
      <section aria-labelledby={`${ids}-title`} aria-live="polite" className="flex flex-col gap-3">
        <div className="flex items-start justify-between gap-2">
          <h3 id={`${ids}-title`} className="font-medium">
            {done ? t("feedback.thanks") : t(dislike ? "feedback.dislikeTitle" : "feedback.likeTitle")}
          </h3>
          <Button variant="ghost" size="icon-xs" aria-label={t("common.close")} onClick={f.dismiss}>
            <HugeiconsIcon icon={Cancel01Icon} strokeWidth={2} />
          </Button>
        </div>
        {done ? (
          followUp &&
          (followUp.action === "follow" ? onFollow : browse) && (
            <div>
              {followUp.action === "follow" ? (
                <Button variant="outline" size="sm" onClick={() => onFollow?.(followUp.entity)}>
                  {t("feedback.follow", { name: followUp.entity.name })}
                </Button>
              ) : (
                <a href={browse} className="text-primary underline-offset-4 hover:underline">
                  {t("feedback.browse", { name: followUp.entity.name })}
                </a>
              )}
            </div>
          )
        ) : (
          <>
            <div role="group" aria-labelledby={`${ids}-title`} className="flex flex-wrap gap-1.5">
              {choices.map((c) => (
                <Button
                  key={c.reason + (c.tag?.id ?? "")}
                  variant={same(c) ? "secondary" : "outline"}
                  size="sm"
                  aria-pressed={same(c)}
                  title={c.reason === "misleading" ? t("feedback.misleadingHint") : undefined}
                  onClick={() => {
                    setChoice(same(c) ? undefined : c);
                    setEntity(undefined);
                    setHide(false);
                  }}
                >
                  {label(c)}
                </Button>
              ))}
            </div>
            {entities.length > (reason === "tag" ? 0 : 1) && (
              <div role="group" aria-label={t(reason === "tag" ? "feedback.pickTag" : reason === "artist" ? "feedback.pickArtist" : "feedback.pickSeries")} className="flex flex-wrap gap-1.5">
                {entities.map((e) => (
                  <Button key={e.id} variant={entity === e.id ? "secondary" : "ghost"} size="xs" aria-pressed={entity === e.id} onClick={() => setEntity(entity === e.id ? undefined : e.id)}>
                    {e.name}
                  </Button>
                ))}
              </div>
            )}
            {reason === "other" && (
              <div className="flex flex-col gap-1">
                <Textarea aria-label={t("feedback.otherPlaceholder")} placeholder={t("feedback.otherPlaceholder")} maxLength={MAX_TEXT} rows={2} value={text} onChange={(e) => setText(e.target.value)} />
                <span className="self-end text-xs text-muted-foreground">{m.plural("comments.charsLeft", MAX_TEXT - [...text].length)}</span>
              </div>
            )}
            {hideable && (
              <div className="flex flex-wrap items-center gap-2">
                <Switch id={`${ids}-hide`} checked={hide} onCheckedChange={setHide} />
                <Label htmlFor={`${ids}-hide`}>{t("feedback.hide", { tag: hideable.name })}</Label>
                {filtersHref && (
                  <a href={filtersHref} className="text-xs text-muted-foreground underline-offset-4 hover:underline">
                    {t("feedback.filterSettings")}
                  </a>
                )}
              </div>
            )}
            <div className="flex flex-wrap items-center justify-between gap-2">
              {dislike && (reportHref || onReport) ? (
                <a
                  href={reportHref ?? "#"}
                  onClick={
                    onReport
                      ? (e) => {
                          e.preventDefault();
                          onReport();
                        }
                      : undefined
                  }
                  className="text-xs text-muted-foreground underline-offset-4 hover:underline"
                >
                  {t("feedback.report")}
                </a>
              ) : (
                <span />
              )}
              <div className="flex gap-1.5">
                <Button variant="ghost" size="sm" onClick={f.dismiss}>
                  {t("feedback.skip")}
                </Button>
                <Button size="sm" disabled={!ready || f.pending} onClick={() => void submit()}>
                  {t(f.pending ? "feedback.sending" : "feedback.send")}
                </Button>
              </div>
            </div>
            {failed && (
              <span role="alert" className="text-xs text-destructive">
                {t("feedback.failed")}
              </span>
            )}
          </>
        )}
      </section>
    </ContentKitUiRoot>
  );
}
