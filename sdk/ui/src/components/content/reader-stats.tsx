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
  /** Display names of content kinds by identifier. */
  labels?: Labels;
  client?: ContentKitClient;
  appearance?: ContentKitUiAppearance;
  className?: string;
}

/** The signed-in reader's activity: contents finished per kind, their comments and the reactions those received. Nothing signed out. */
export function ReaderStats({ labels, client, appearance, className }: ReaderStatsProps) {
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
  const stats: [string, number][] = [
    ...Object.entries(s.data.completed).map(([kind, n]): [string, number] => [t("myStats.completed", { kind: labelOf(labels, kind) }), n]),
    [t("myStats.comments"), s.data.comments],
    [t("myStats.likes"), s.data.comment_likes],
    [t("myStats.dislikes"), s.data.comment_dislikes],
  ];
  return root(
    <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4">
      {stats.map(([label, n]) => (
        <div key={label} className="grid gap-0.5 rounded-lg border border-border p-3" data-ckui="reader-stat">
          <dt className="text-xs text-muted-foreground">{label}</dt>
          <dd className="text-lg font-semibold tabular-nums">{n}</dd>
        </div>
      ))}
    </dl>,
  );
}
