import { cn } from "cn";
import type { ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import { useMessages } from "../../i18n/context.js";
import { useMyStats } from "../../react/stats.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Skeleton } from "#ckui/ui/skeleton";
import { labelOf, type Labels } from "../groups/parts.js";
import { contentError } from "../social/parts.js";

export interface ReaderStatsProps {
  /** Display names of content kinds and access tiers by identifier. */
  labels?: Labels;
  /** Complete labels for finished-content counts, e.g. {gallery: "Galleries read"}. */
  completedLabels?: Labels;
  /** Content kinds to show, including an unavailable placeholder. Default: returned kinds. */
  kinds?: readonly string[];
  /** Access tiers to show, including an unavailable placeholder. Default: returned tiers. */
  tiers?: readonly string[];
  /** Include received dislikes. Defaults to true. */
  showDislikes?: boolean;
  client?: ContentKitClient;
  appearance?: ContentKitUiAppearance;
  className?: string;
}

/** The signed-in reader's activity and elapsed tier access. Nothing signed out. */
export function ReaderStats({ labels, completedLabels, kinds, tiers, showDislikes = true, client, appearance, className }: ReaderStatsProps) {
  const m = useMessages();
  const { t } = m;
  const s = useMyStats({ client });
  const root = (children: ReactNode) => (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-3 text-sm", className)} data-ckui="reader-stats">
      {children}
    </ContentKitUiRoot>
  );
  if (s.error) return root(<p role="alert" className="text-destructive">{contentError(m, s.error)}</p>);
  if (!s.data) return s.loading ? root(<Skeleton className="h-16 w-full" />) : null;
  const data = s.data;
  const number = (value: number | undefined) => value === undefined ? "—" : new Intl.NumberFormat(m.language).format(value);
  const daySeconds = 24 * 60 * 60;
  const monthSeconds = 30 * daySeconds;
  const tenure = (seconds: number | undefined) => {
    if (seconds === undefined) return "—";
    if (seconds > 0 && seconds < daySeconds) return t("myStats.lessThanDay");
    const unit = seconds < monthSeconds ? "day" : "month";
    const count = Math.floor(seconds / (unit === "day" ? daySeconds : monthSeconds));
    return new Intl.NumberFormat(m.language, { style: "unit", unit, unitDisplay: "long" }).format(count);
  };
  const showKinds = kinds ?? Object.keys(data.completed);
  const showTiers = tiers ?? Object.keys(data.access_seconds ?? {});
  const unavailable = showKinds.some((kind) => data.completed[kind] === undefined) || showTiers.some((tier) => data.access_seconds?.[tier] === undefined);
  const stats: [string, string, string?][] = [
    ...showTiers.map((tier): [string, string, string] => [t("myStats.accessFor", { tier: labelOf(labels, tier) }), tenure(data.access_seconds?.[tier]), t("myStats.accessDescription")]),
    ...showKinds.map((kind): [string, string] => [completedLabels?.[kind] ?? t("myStats.completed", { kind: labelOf(labels, kind) }), number(data.completed[kind])]),
    [t("myStats.comments"), number(data.comments)],
    [t("myStats.likes"), number(data.comment_likes)],
    ...(showDislikes ? [[t("myStats.dislikes"), number(data.comment_dislikes)] as [string, string]] : []),
  ];
  return root(
    <>
      <dl className={cn("grid grid-cols-2 gap-3", stats.length === 4 ? "lg:grid-cols-4" : "sm:grid-cols-3 lg:grid-cols-5")}>
        {stats.map(([label, value, description]) => (
          <div key={label} className="grid gap-1 rounded-lg border border-border p-4" data-ckui="reader-stat" title={description}>
            <dt className="text-xs text-muted-foreground">{label}</dt>
            <dd className="text-xl font-semibold tabular-nums">{value}</dd>
          </div>
        ))}
      </dl>
      {unavailable ? <p className="text-xs text-muted-foreground" role="status">{t("myStats.unavailable")}</p> : null}
    </>,
  );
}
