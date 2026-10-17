import { cn } from "cn";
import { useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentKitError } from "../../client/errors.js";
import { formatDuration } from "../../client/gallery.js";
import type { ActivityDay } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useFlowClient } from "../../react/admin.js";
import { useActivity } from "../../react/stats.js";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { AdminRoot, LoadFailed, NoAccess, Rows, type AdminFlowProps } from "./shell.js";

type Choice = string | { value: string; label: string };

export interface StatsDashboardProps extends AdminFlowProps {
  /** Content kinds offered: codes, or codes with labels; the first shows first. */
  kinds: readonly Choice[];
}

const choices = (list: readonly Choice[]) => list.map((c) => (typeof c === "string" ? { value: c, label: c } : c));
const DAYS = [10, 30, 90] as const;
const refused = (e?: ContentKitError) => e?.code === "forbidden" || e?.code === "unauthorized";

// Validated categorical slots (light / dark steps), stacked bottom to top.
const AUDIENCES = [
  { key: "anon", fill: "bg-[#2a78d6] dark:bg-[#3987e5]" },
  { key: "free", fill: "bg-[#eb6834] dark:bg-[#d95926]" },
  { key: "premium", fill: "bg-[#1baf7a] dark:bg-[#199e70]" },
] as const;

/**
 * Staff statistics from measured attention: distinct people per UTC day who
 * read or played at least one item, by audience (anonymous, free, premium).
 * People are distinct within a day only, so days are never summed into a
 * period-unique total.
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

function Dashboard({ kinds, client }: Inner) {
  const { t } = useMessages();
  const options = choices(kinds);
  const [kind, setKind] = useState(options[0]?.value ?? "");
  const [days, setDays] = useState<number>(DAYS[0]);
  const activity = useActivity(kind ? { kind, days } : null, { client });
  if (refused(activity.error)) return <NoAccess>{t("stats.noAccess")}</NoAccess>;
  // Newest first from the server; the chart reads oldest to newest.
  const rows = [...(activity.data?.days ?? [])].reverse();
  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold">{t("stats.title")}</h2>
        {options.length > 1 && <Pick label={t("stats.kind")} value={kind} onChange={setKind} options={options} />}
      </div>

      <section className="grid gap-3" data-ckui="stats-activity">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="grid gap-0.5">
            <h3 className="font-medium">{t("stats.activity")}</h3>
            <p className="text-xs text-muted-foreground">{t("stats.visitorsNote")}</p>
          </div>
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
          <Rows count={3} />
        ) : (
          <div className="grid gap-4" aria-busy={activity.loading}>
            <Today day={rows.at(-1)} />
            <Chart rows={rows} />
            <DayTable rows={rows} />
          </div>
        )}
      </section>
    </>
  );
}

function Today({ day }: { day?: ActivityDay }) {
  const { t } = useMessages();
  const tiles = [
    { label: t("stats.visitors"), value: day?.readers ?? 0, fill: undefined },
    ...AUDIENCES.map((a) => ({ label: t(`stats.${a.key}`), value: day?.[a.key] ?? 0, fill: a.fill })),
  ];
  return (
    <div className="grid gap-2" data-ckui="stats-today">
      <p className="text-xs font-medium text-muted-foreground">
        {t("stats.today")}
        {day && day.active_ms > 0 && (
          <span className="font-normal">
            {" · "}
            {t("stats.activeTime")} {formatDuration(Math.floor(day.active_ms / 1000))}
          </span>
        )}
      </p>
      <dl className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        {tiles.map((tile) => (
          <div key={tile.label} className="grid gap-0.5 rounded-lg border border-border p-3">
            <dt className="flex items-center gap-1.5 text-xs text-muted-foreground">
              {tile.fill && <span aria-hidden className={cn("size-2.5 rounded-sm", tile.fill)} />}
              {tile.label}
            </dt>
            <dd className="text-xl font-semibold tabular-nums">{tile.value}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}

function Chart({ rows }: { rows: ActivityDay[] }) {
  const m = useMessages();
  const { t } = m;
  const [active, setActive] = useState<number | null>(null);
  const most = Math.max(0, ...rows.map((d) => d.readers));
  const scale = most || 1;
  const every = Math.ceil(rows.length / 10);
  const short = new Intl.DateTimeFormat(m.language, { month: "short", day: "numeric", timeZone: "UTC" });
  const long = new Intl.DateTimeFormat(m.language, { dateStyle: "medium", timeZone: "UTC" });
  const at = (date: string) => new Date(`${date}T00:00:00Z`);
  const shown = active === null ? undefined : rows[active];
  return (
    <figure className="grid gap-2" data-ckui="stats-chart">
      <figcaption className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
        {AUDIENCES.map((a) => (
          <span key={a.key} className="flex items-center gap-1.5">
            <span aria-hidden className={cn("size-2.5 rounded-sm", a.fill)} />
            {t(`stats.${a.key}`)}
          </span>
        ))}
        <span className="ms-auto tabular-nums">{t("stats.peak", { count: most })}</span>
      </figcaption>
      <div className="relative">
        <div className="pointer-events-none absolute inset-x-0 top-0 border-t border-dashed border-border" aria-hidden />
        <div className="pointer-events-none absolute inset-x-0 top-1/2 border-t border-dashed border-border/60" aria-hidden />
        <ol className="relative flex h-48 items-end gap-1 border-b border-border" onMouseLeave={() => setActive(null)}>
          {rows.map((d, i) => (
            <li
              key={d.date}
              tabIndex={0}
              data-ckui="stats-day"
              aria-label={`${long.format(at(d.date))}: ${t("stats.visitors")} ${d.readers}, ${t("stats.anon")} ${d.anon}, ${t("stats.free")} ${d.free}, ${t("stats.premium")} ${d.premium}`}
              className="flex h-full min-w-0 flex-1 cursor-default flex-col justify-end rounded-sm outline-none hover:bg-muted/40 focus-visible:bg-muted/40 focus-visible:ring-1 focus-visible:ring-ring"
              onMouseEnter={() => setActive(i)}
              onFocus={() => setActive(i)}
              onBlur={() => setActive(null)}
            >
              {/* 2px surface gaps between segments; the top segment carries the 4px rounded end. */}
              <span className="mx-auto flex w-full max-w-10 flex-col-reverse gap-[2px]" style={{ height: `${(d.readers / scale) * 100}%` }}>
                {AUDIENCES.map((a, j) => {
                  const value = d[a.key];
                  if (!value) return null;
                  const top = AUDIENCES.slice(j + 1).every((b) => !d[b.key]);
                  return <span key={a.key} className={cn("min-h-[2px] w-full", a.fill, top && "rounded-t-[4px]")} style={{ flexGrow: value, flexBasis: 0 }} />;
                })}
              </span>
            </li>
          ))}
        </ol>
        {shown && active !== null && (
          <div
            role="tooltip"
            className="pointer-events-none absolute -top-2 z-10 grid min-w-36 -translate-y-full gap-1 rounded-md border border-border bg-popover p-2 text-xs text-popover-foreground shadow-md"
            style={{ left: `clamp(0%, ${((active + 0.5) / rows.length) * 100}% - 4.5rem, calc(100% - 9rem))` }}
          >
            <p className="font-medium">{long.format(at(shown.date))}</p>
            <p className="flex justify-between gap-3 tabular-nums">
              <span>{t("stats.visitors")}</span>
              <span className="font-semibold">{shown.readers}</span>
            </p>
            {[...AUDIENCES].reverse().map((a) => (
              <p key={a.key} className="flex items-center justify-between gap-3 tabular-nums text-muted-foreground">
                <span className="flex items-center gap-1.5">
                  <span aria-hidden className={cn("size-2 rounded-sm", a.fill)} />
                  {t(`stats.${a.key}`)}
                </span>
                <span className="text-popover-foreground">{shown[a.key]}</span>
              </p>
            ))}
          </div>
        )}
      </div>
      <ol aria-hidden className="flex gap-1 text-[11px] text-muted-foreground tabular-nums">
        {rows.map((d, i) => (
          <li key={d.date} className="min-w-0 flex-1 overflow-visible text-center whitespace-nowrap">
            {(rows.length - 1 - i) % every === 0 ? short.format(at(d.date)) : ""}
          </li>
        ))}
      </ol>
      {most === 0 && <p className="text-muted-foreground">{t("stats.noActivity")}</p>}
    </figure>
  );
}

function DayTable({ rows }: { rows: ActivityDay[] }) {
  const { t } = useMessages();
  return (
    <details className="text-sm">
      <summary className="cursor-pointer text-xs text-muted-foreground">{t("stats.showTable")}</summary>
      <table className="mt-2 w-full text-left tabular-nums">
        <thead className="text-xs text-muted-foreground">
          <tr>
            <th className="py-1 font-medium">{t("stats.date")}</th>
            <th className="py-1 text-end font-medium">{t("stats.visitors")}</th>
            <th className="py-1 text-end font-medium">{t("stats.anon")}</th>
            <th className="py-1 text-end font-medium">{t("stats.free")}</th>
            <th className="py-1 text-end font-medium">{t("stats.premium")}</th>
            <th className="py-1 text-end font-medium">{t("stats.activeTime")}</th>
          </tr>
        </thead>
        <tbody>
          {[...rows].reverse().map((d) => (
            <tr key={d.date} className="border-t border-border">
              <td className="py-1.5 pe-3 whitespace-nowrap">{d.date}</td>
              <td className="py-1.5 text-end">{d.readers}</td>
              <td className="py-1.5 text-end">{d.anon}</td>
              <td className="py-1.5 text-end">{d.free}</td>
              <td className="py-1.5 text-end">{d.premium}</td>
              <td className="py-1.5 text-end">{formatDuration(Math.floor(d.active_ms / 1000))}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </details>
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
