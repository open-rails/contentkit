import { useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentKitError } from "../../client/errors.js";
import type { Period } from "../../client/discovery.js";
import { formatDuration } from "../../client/gallery.js";
import type { TopQuery } from "../../client/stats.js";
import type { RankedContent } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useFlowClient } from "../../react/admin.js";
import { useActivity, useTopContents } from "../../react/stats.js";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { AdminRoot, LoadFailed, NoAccess, Rows, type AdminFlowProps } from "./shell.js";

type Choice = string | { value: string; label: string };
type Metric = NonNullable<TopQuery["metric"]>;

export interface StatsDashboardProps extends AdminFlowProps {
  /** Content kinds offered: codes, or codes with labels; the first shows first. */
  kinds: readonly Choice[];
  /** A ranked item's link; default its path. */
  contentHref?: (content: RankedContent) => string | undefined;
  /** Rows in the ranking. Default 10. */
  limit?: number;
}

const choices = (list: readonly Choice[]) => list.map((c) => (typeof c === "string" ? { value: c, label: c } : c));
const DAYS = [7, 30, 90] as const;
const METRICS: readonly Metric[] = ["readers", "comments"];
const PERIODS: readonly Period[] = ["7d", "30d", "90d", "365d", "all"];
const refused = (e?: ContentKitError) => e?.code === "forbidden" || e?.code === "unauthorized";

/**
 * Staff statistics from measured attention: daily distinct people and focused
 * active time, plus content ranked by distinct engaged people or comments.
 * Daily people are never summed into a misleading period-unique total.
 * Comments remain available without the signal plane.
 */
export function StatsDashboard({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: StatsDashboardProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="stats-dashboard" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Dashboard {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<StatsDashboardProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };

function Dashboard({ kinds, contentHref, limit = 10, client }: Inner) {
  const { t } = useMessages();
  const options = choices(kinds);
  const [kind, setKind] = useState(options[0]?.value ?? "");
  const [days, setDays] = useState<number>(7);
  const [metric, setMetric] = useState<Metric>("readers");
  const [period, setPeriod] = useState<Period>("30d");
  const activity = useActivity(kind ? { kind, days } : null, { client });
  const top = useTopContents(kind ? { kind, metric, period, limit } : null, { client });
  if (refused(activity.error) || refused(top.error)) return <NoAccess>{t("stats.noAccess")}</NoAccess>;
  const rows = activity.data?.days ?? [];
  const most = Math.max(1, ...rows.map((d) => d.readers));
  const totalActiveMS = rows.reduce((n, d) => n + d.active_ms, 0);
  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold">{t("stats.title")}</h2>
        {options.length > 1 && <Pick label={t("stats.kind")} value={kind} onChange={setKind} options={options} />}
      </div>

      <section className="grid gap-3" data-ckui="stats-activity">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h3 className="font-medium">{t("stats.activity")}</h3>
          <Pick
            label={t("stats.days")}
            value={String(days)}
            onChange={(v) => setDays(Number(v))}
            options={DAYS.map((d) => ({ value: String(d), label: t("stats.lastDays", { count: d }) }))}
          />
        </div>
        {activity.unavailable ? (
          <p className="text-muted-foreground" role="status">
            {t("stats.unavailable")}
          </p>
        ) : activity.error ? (
          <LoadFailed error={activity.error} message={t("stats.loadFailed")} retry={activity.reload} />
        ) : activity.loading && !activity.data ? (
          <Rows count={2} />
        ) : !rows.length ? (
          <p className="text-muted-foreground">{t("stats.noActivity")}</p>
        ) : (
          <table className="w-full text-left tabular-nums" aria-busy={activity.loading}>
            <thead className="text-xs text-muted-foreground">
              <tr>
                <th className="py-1 font-medium">{t("stats.date")}</th>
                <th className="py-1 font-medium">{t("stats.readers")}</th>
                <th className="py-1 text-end font-medium">{t("stats.users")}</th>
                <th className="py-1 text-end font-medium">{t("stats.activeTime")}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((d) => (
                <tr key={d.date} className="border-t border-border" data-ckui="stats-day">
                  <td className="py-1.5 pe-3 whitespace-nowrap">{d.date}</td>
                  <td className="w-1/2 py-1.5 pe-3">
                    <span className="flex items-center gap-2">
                      <span aria-hidden className="h-2 rounded-full bg-primary" style={{ width: `${Math.max(2, (d.readers / most) * 100)}%` }} />
                      {d.readers}
                    </span>
                  </td>
                  <td className="py-1.5 text-end">{d.users}</td>
                  <td className="py-1.5 text-end">{formatDuration(Math.floor(d.active_ms / 1000))}</td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr className="border-t border-border font-medium">
                <th scope="row" colSpan={3} className="py-1.5 font-medium">
                  {t("stats.totalActiveTime")}
                </th>
                <td className="py-1.5 text-end" data-ckui="stats-active-total">
                  {formatDuration(Math.floor(totalActiveMS / 1000))}
                </td>
              </tr>
            </tfoot>
          </table>
        )}
      </section>

      <section className="grid gap-3" data-ckui="stats-top">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h3 className="font-medium">{t("stats.top")}</h3>
          <div className="flex flex-wrap gap-2">
            <Pick label={t("stats.metric")} value={metric} onChange={(v) => setMetric(v as Metric)} options={METRICS.map((x) => ({ value: x, label: t(`stats.metric_${x}`) }))} />
            <Pick label={t("stats.period")} value={period} onChange={(v) => setPeriod(v as Period)} options={PERIODS.map((x) => ({ value: x, label: t(`stats.period_${x}`) }))} />
          </div>
        </div>
        {top.unavailable ? (
          <p className="text-muted-foreground" role="status">
            {t("stats.unavailable")}
          </p>
        ) : top.error ? (
          <LoadFailed error={top.error} message={t("stats.loadFailed")} retry={top.reload} />
        ) : top.loading && !top.data ? (
          <Rows count={3} />
        ) : !top.data?.contents.length ? (
          <p className="text-muted-foreground">{t("stats.noTop")}</p>
        ) : (
          <ol className="grid divide-y divide-border rounded-lg border border-border" aria-busy={top.loading}>
            {top.data.contents.map((c, i) => {
              const href = contentHref ? contentHref(c) : c.path;
              return (
                <li key={c.id} className="flex items-center gap-3 p-2.5" data-ckui="stats-ranked">
                  <span className="w-6 text-end text-muted-foreground tabular-nums">{i + 1}</span>
                  {href ? (
                    <a className="min-w-0 flex-1 break-words hover:underline" href={href}>
                      {c.title}
                    </a>
                  ) : (
                    <span className="min-w-0 flex-1 break-words">{c.title}</span>
                  )}
                  <span className="tabular-nums" data-ckui="stats-value">
                    {c.value}
                  </span>
                </li>
              );
            })}
          </ol>
        )}
      </section>
    </>
  );
}

function Pick({ label, value, onChange, options }: { label: string; value: string; onChange: (v: string) => void; options: { value: string; label: string }[] }) {
  return (
    <Select value={value} onValueChange={(v) => onChange(String(v))}>
      <SelectTrigger size="sm" aria-label={label}>
        <SelectValue>{(v: string) => options.find((o) => o.value === v)?.label ?? v}</SelectValue>
      </SelectTrigger>
      <SelectContent>
        {options.map((o) => (
          <SelectItem key={o.value} value={o.value}>
            {o.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
