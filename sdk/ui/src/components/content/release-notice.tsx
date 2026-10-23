import type { Release } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { formatDate } from "../social/parts.js";

/** Additional audiences and their release dates, independent of billing prices. */
export function ReleaseNotice({ releases }: { releases?: Release[] }) {
  const m = useMessages();
  return <>{releases?.map((release, i) => <p key={i} className="text-muted-foreground">{m.t("contents.releaseFrom", {
    audience: release.level === "tier" ? release.tiers?.join(", ") ?? "" : m.t(`contentEditor.level_${release.level}`),
    date: formatDate(m, release.at, false),
  })}</p>)}</>;
}
