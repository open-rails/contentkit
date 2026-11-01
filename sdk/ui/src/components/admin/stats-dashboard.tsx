import { useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentKitError } from "../../client/errors.js";
import { formatDuration } from "../../client/gallery.js";
import type { ActivityDay } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useFlowClient } from "../../react/admin.js";
import { useActivity } from "../../react/stats.js";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "#ckui/ui/select";
import {
  AdminRoot,
  LoadFailed,
  NoAccess,
  Rows,
  type AdminFlowProps,
} from "./shell.js";

type Choice = string | { value: string; label: string };

export interface StatsDashboardProps extends AdminFlowProps {
  /** Content kinds offered: codes, or codes with labels; the first shows first. */
  kinds: readonly Choice[];
  /** Host-owned labels and colors for the server's configured cohort IDs. */
  series?: readonly ActivitySeries[];
  /** UTC days to show, today included; defaults to ten. */
  days?: number;
}

const choices = (list: readonly Choice[]) =>
  list.map((c) => (typeof c === "string" ? { value: c, label: c } : c));
const refused = (e?: ContentKitError) =>
  e?.code === "forbidden" || e?.code === "unauthorized";

export interface ActivitySeries {
  id: string;
  label: string;
  /** Any CSS color, including a host theme variable. */
  color?: string;
}

const COLORS = ["#3987e5", "#e98042", "#21a87e", "#aa79da", "#d66088"];
const value = (day: ActivityDay | undefined, id: string) =>
  day?.cohorts?.[id] ?? 0;

/**
 * Staff statistics from measured attention: distinct people per UTC day who
 * read or played at least one item, by application-defined cohort.
 * People are distinct within a day only, so days are never summed into a
 * period-unique total.
 */
export function StatsDashboard({
  client: own,
  baseUrl,
  auth,
  guard,
  appearance,
  className,
  language,
  messages,
  ...p
}: StatsDashboardProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot
      flow="stats-dashboard"
      appearance={appearance}
      className={className}
      language={language}
      messages={messages}
      guard={guard}
    >
      <Dashboard {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<
  StatsDashboardProps,
  | "client"
  | "baseUrl"
  | "auth"
  | "guard"
  | "appearance"
  | "className"
  | "language"
  | "messages"
> & { client: ContentKitClient };

function Dashboard({
  kinds,
  series: configured = [],
  days = 10,
  client,
}: Inner) {
  const { t } = useMessages();
  const options = choices(kinds);
  const [kind, setKind] = useState(options[0]?.value ?? "");
  const activity = useActivity(kind ? { kind, days } : null, { client });
  if (refused(activity.error))
    return <NoAccess>{t("stats.noAccess")}</NoAccess>;
  // Newest first from the server; the chart reads oldest to newest.
  const rows = [...(activity.data?.days ?? [])].reverse();
  const ids = activity.data?.cohorts ?? [];
  const ordered: ActivitySeries[] = [
    ...configured.filter((s) => ids.includes(s.id)),
    ...ids
      .filter((id) => !configured.some((s) => s.id === id))
      .map((id) => ({ id, label: id })),
  ];
  const series = ordered.map((s, i) => ({
    ...s,
    color: s.color ?? COLORS[i % COLORS.length]!,
  }));
  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold">{t("stats.title")}</h2>
        {options.length > 1 && (
          <Pick
            label={t("stats.kind")}
            value={kind}
            onChange={setKind}
            options={options}
          />
        )}
      </div>

      <section className="grid gap-3" data-ckui="stats-activity">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="grid gap-0.5">
            <h3 className="font-medium">{t("stats.activity")}</h3>
            <p className="text-xs text-muted-foreground">
              {t("stats.visitorsNote")}
            </p>
          </div>
          <span className="text-xs text-muted-foreground">
            {t("stats.lastDays", { count: days })}
          </span>
        </div>
        {activity.unavailable ? (
          <p className="text-muted-foreground" role="status">
            {t("stats.unavailable")}
          </p>
        ) : activity.error ? (
          <LoadFailed
            error={activity.error}
            message={t("stats.loadFailed")}
            retry={activity.reload}
          />
        ) : activity.loading && !activity.data ? (
          <Rows count={3} />
        ) : (
          <div className="grid gap-4" aria-busy={activity.loading}>
            <Today day={rows.at(-1)} series={series} />
            <Chart rows={rows} series={series} />
            <DayTable rows={rows} series={series} />
          </div>
        )}
      </section>
    </>
  );
}

function Today({
  day,
  series,
}: {
  day?: ActivityDay;
  series: readonly ActivitySeries[];
}) {
  const { t } = useMessages();
  const tiles = [
    {
      id: "__total",
      label: t("stats.visitors"),
      value: day?.readers ?? 0,
      color: undefined,
    },
    ...series.map((s) => ({
      id: s.id,
      label: s.label,
      value: value(day, s.id),
      color: s.color,
    })),
  ];
  return (
    <div className="grid gap-2" data-ckui="stats-today">
      <p className="text-xs font-medium text-muted-foreground">
        {t("stats.today")}
        {day && day.active_ms > 0 && (
          <span className="font-normal">
            {" · "}
            {t("stats.activeTime")}{" "}
            {formatDuration(Math.floor(day.active_ms / 1000))}
          </span>
        )}
      </p>
      <dl
        className="grid gap-2"
        style={{ gridTemplateColumns: "repeat(auto-fit, minmax(8rem, 1fr))" }}
      >
        {tiles.map((tile) => (
          <div
            key={tile.id}
            className="grid gap-0.5 rounded-lg border border-border p-3"
          >
            <dt className="flex items-center gap-1.5 text-xs text-muted-foreground">
              {tile.color && (
                <span
                  aria-hidden
                  className="size-2.5 rounded-sm"
                  style={{ backgroundColor: tile.color }}
                />
              )}
              {tile.label}
            </dt>
            <dd className="text-xl font-semibold tabular-nums">{tile.value}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}

function Chart({
  rows,
  series,
}: {
  rows: ActivityDay[];
  series: readonly ActivitySeries[];
}) {
  const m = useMessages();
  const { t } = m;
  const [active, setActive] = useState<number | null>(null);
  const most = Math.max(
    0,
    ...rows.flatMap((d) =>
      series.length ? series.map((s) => value(d, s.id)) : [d.readers],
    ),
  );
  const scale = most || 1;
  const every = Math.ceil(rows.length / 10);
  const short = new Intl.DateTimeFormat(m.language, {
    month: "short",
    day: "numeric",
    timeZone: "UTC",
  });
  const long = new Intl.DateTimeFormat(m.language, {
    dateStyle: "medium",
    timeZone: "UTC",
  });
  const at = (date: string) => new Date(`${date}T00:00:00Z`);
  const shown = active === null ? undefined : rows[active];
  const lines = series.length
    ? series
    : [{ id: "", label: t("stats.visitors"), color: COLORS[0] }];
  // SVG scales horizontally; strokes retain their visible size.
  const x = (i: number) =>
    rows.length > 1 ? 12 + (i / (rows.length - 1)) * 976 : 500;
  const y = (n: number) => 190 - (n / scale) * 178;
  const count = (d: ActivityDay, id: string) => (id ? value(d, id) : d.readers);
  return (
    // One shrinkable column: an auto track grew to the date labels' combined
    // width and pushed narrow pages sideways.
    <figure className="grid grid-cols-[minmax(0,1fr)] gap-2" data-ckui="stats-chart">
      <figcaption className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
        {lines.map((s) => (
          <span key={s.id} className="flex items-center gap-1.5">
            <span
              aria-hidden
              className="size-2.5 rounded-sm"
              style={{ backgroundColor: s.color }}
            />
            {s.label}
          </span>
        ))}
        <span className="ms-auto tabular-nums">
          {t("stats.peak", { count: most })}
        </span>
      </figcaption>
      <div className="relative h-52" onMouseLeave={() => setActive(null)}>
        <svg
          viewBox="0 0 1000 200"
          preserveAspectRatio="none"
          className="absolute inset-0 size-full overflow-visible"
          aria-hidden
        >
          {[12, 101, 190].map((pos) => (
            <line
              key={pos}
              x1="12"
              x2="988"
              y1={pos}
              y2={pos}
              stroke="currentColor"
              className="text-border"
              strokeDasharray="4 4"
              vectorEffect="non-scaling-stroke"
            />
          ))}
          {lines.map((s) => (
            <polyline
              key={s.id}
              data-ckui="stats-line"
              data-series={s.id}
              points={rows
                .map((d, i) => `${x(i)},${y(count(d, s.id))}`)
                .join(" ")}
              fill="none"
              stroke={s.color}
              strokeWidth="2.5"
              strokeLinecap="round"
              strokeLinejoin="round"
              vectorEffect="non-scaling-stroke"
            />
          ))}
        </svg>
        <ol className="absolute inset-0 flex">
          {rows.map((d, i) => (
            <li
              key={d.date}
              tabIndex={0}
              data-ckui="stats-day"
              aria-label={`${long.format(at(d.date))}: ${t("stats.visitors")} ${d.readers}${series.map((s) => `, ${s.label} ${value(d, s.id)}`).join("")}`}
              className="h-full min-w-0 flex-1 cursor-default rounded-sm outline-none hover:bg-muted/20 focus-visible:bg-muted/20 focus-visible:ring-1 focus-visible:ring-ring"
              onMouseEnter={() => setActive(i)}
              onFocus={() => setActive(i)}
              onBlur={() => setActive(null)}
            />
          ))}
        </ol>
        {shown && active !== null && (
          <div
            role="tooltip"
            className="pointer-events-none absolute -top-2 z-10 grid min-w-36 -translate-y-full gap-1 rounded-md border border-border bg-popover p-2 text-xs text-popover-foreground shadow-md"
            style={{
              left: `clamp(0%, ${((active + 0.5) / rows.length) * 100}% - 4.5rem, calc(100% - 9rem))`,
            }}
          >
            <p className="font-medium">{long.format(at(shown.date))}</p>
            <p className="flex justify-between gap-3 tabular-nums">
              <span>{t("stats.visitors")}</span>
              <span className="font-semibold">{shown.readers}</span>
            </p>
            {series.map((s) => (
              <p
                key={s.id}
                className="flex items-center justify-between gap-3 tabular-nums text-muted-foreground"
              >
                <span className="flex items-center gap-1.5">
                  <span
                    aria-hidden
                    className="size-2 rounded-sm"
                    style={{ backgroundColor: s.color }}
                  />
                  {s.label}
                </span>
                <span className="text-popover-foreground">
                  {value(shown, s.id)}
                </span>
              </p>
            ))}
          </div>
        )}
      </div>
      <ol
        aria-hidden
        className="flex gap-1 text-[11px] text-muted-foreground tabular-nums"
      >
        {rows.map((d, i) => (
          <li
            key={d.date}
            className="min-w-0 flex-1 overflow-visible text-center whitespace-nowrap"
          >
            {(rows.length - 1 - i) % every === 0
              ? short.format(at(d.date))
              : ""}
          </li>
        ))}
      </ol>
      {most === 0 && (
        <p className="text-muted-foreground">{t("stats.noActivity")}</p>
      )}
    </figure>
  );
}

function DayTable({
  rows,
  series,
}: {
  rows: ActivityDay[];
  series: readonly ActivitySeries[];
}) {
  const { t } = useMessages();
  return (
    <details className="text-sm">
      <summary className="cursor-pointer text-xs text-muted-foreground">
        {t("stats.showTable")}
      </summary>
      <table className="mt-2 w-full text-left tabular-nums">
        <thead className="text-xs text-muted-foreground">
          <tr>
            <th className="py-1 font-medium">{t("stats.date")}</th>
            <th className="py-1 text-end font-medium">{t("stats.visitors")}</th>
            {series.map((s) => (
              <th key={s.id} className="py-1 text-end font-medium">
                {s.label}
              </th>
            ))}
            <th className="py-1 text-end font-medium">
              {t("stats.activeTime")}
            </th>
          </tr>
        </thead>
        <tbody>
          {[...rows].reverse().map((d) => (
            <tr key={d.date} className="border-t border-border">
              <td className="py-1.5 pe-3 whitespace-nowrap">{d.date}</td>
              <td className="py-1.5 text-end">{d.readers}</td>
              {series.map((s) => (
                <td key={s.id} className="py-1.5 text-end">
                  {value(d, s.id)}
                </td>
              ))}
              <td className="py-1.5 text-end">
                {formatDuration(Math.floor(d.active_ms / 1000))}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </details>
  );
}

function Pick({
  label,
  value,
  onChange,
  options,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  options: { value: string; label: string }[];
}) {
  return (
    <Select value={value} onValueChange={(v) => onChange(String(v))}>
      <SelectTrigger size="sm" aria-label={label}>
        <SelectValue>
          {(v: string) => options.find((o) => o.value === v)?.label ?? v}
        </SelectValue>
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
