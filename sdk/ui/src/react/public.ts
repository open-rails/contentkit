import { useCallback, useEffect, useMemo, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { ReadResult, RefBody } from "../client/generated/wire.js";
import type { PresetRule } from "../client/generated/wire.js";
import { defaultImage, isPattern, ruleDir, stableImage } from "../client/media/rules.js";
import type { PublicPreset } from "../client/public.js";
import { useOptionalContentKitClient, useReadScope } from "./context.js";
import { useKinds } from "./entities.js";
import { useMediaRead } from "./read.js";
import { keyOf, useContentScope, useResource } from "./use-resource.js";
import { resourcesFor } from "./resources.js";

export interface UsePresets {
  /** Every kind's public presets; empty until they load (or when the server has none). */
  presets: PresetRule[];
  loaded: boolean;
  error?: ContentKitError;
  /** Retry a failed read; successful public presets remain cached. */
  reload: () => void;
}

// Seed remounts synchronously with successful, immutable public metadata.
// Failures stay in the mounted resource and are never cached here.
const settled = new WeakMap<ContentKitClient, PresetRule[]>();

/** Public presets, shared across consumers; failed reads can be retried. */
export function usePresets(o: { client?: ContentKitClient | null } = {}): UsePresets {
  const client = useOptionalContentKitClient(o.client);
  const store = client ? resourcesFor(client) : null;
  const key = client ? keyOf("presets") : null;
  const initial = client ? settled.get(client) : undefined;
  const r = useResource<PresetRule[]>(store, key, { type: "presets" }, async () => {
    const presets = await client!.media.presets();
    settled.set(client!, presets);
    return presets;
  }, { initial });
  useEffect(() => {
    // A remount may join a failed slot before its deferred cleanup. Retry
    // once on mounting, rather than retrying continuously after each error.
    if (key && store?.snapshot(key).error) store.load(key, true);
  }, [key, store]);
  return { presets: r.data ?? [], loaded: r.loaded || !!initial || !client, error: r.error, reload: r.reload };
}

export interface PublicImageOptions {
  /** The host's listing of the item's image (its API's): shown without a read. */
  image?: PublicPreset | null;
  /** A read of the item the host already has. */
  read?: ReadResult | null;
  /** Overrides the provider's client. */
  client?: ContentKitClient | null;
}

export interface UsePublicImage {
  /** The item's current renditions; else the kind's default image (when the preset has one); null while unknown or when there is neither. */
  image: PublicPreset | null;
  /** The preset's rules (aspect, widths, min_width) once the presets load. */
  rule?: PresetRule;
  /** image is the kind's default, not the item's own. */
  isDefault: boolean;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
}

/**
 * An item's public image for a preset ("avatar", "cover", a version's
 * "poster"): the exact renditions from a read's `public` (or the host's
 * listing), else the kind's default image, else none. Reads refresh after
 * commits through the same client.
 */
export function usePublicImage(item: RefBody, preset: string, o: PublicImageOptions = {}): UsePublicImage {
  const { kind, id, version } = item;
  const client = useOptionalContentKitClient(o.client);
  const { presets, loaded, error: presetError, reload: reloadPresets } = usePresets({ client });
  const rule = presets.find((p) => p.kind === kind && p.name === preset);
  const listed = o.image !== undefined;
  // The read waits for the presets, so it lists only the preset's upload.
  const prefix = rule ? (isPattern(rule.from) ? ruleDir(rule.from) : rule.from) : undefined;
  const r = useMediaRead(item, { prefix, read: listed || !loaded ? null : o.read, client });
  const image = useMemo((): { image: PublicPreset | null; isDefault: boolean } => {
    const own = listed ? o.image : r.read?.public?.find((p) => p.preset === preset);
    if (own && own.renditions.length) return { image: { preset, renditions: own.renditions, aspect: ("aspect" in own && own.aspect) || rule?.aspect }, isDefault: false };
    if (!listed && !r.read) return { image: null, isDefault: false };
    const def = rule && defaultImage(rule, id, version);
    return def ? { image: def, isDefault: true } : { image: null, isDefault: false };
  }, [listed, o.image, r.read, preset, rule, id, version]);
  const reloadRead = r.reload;
  const reload = useCallback(() => { if (presetError) reloadPresets(); reloadRead(); }, [presetError, reloadPresets, reloadRead]);
  return { ...image, rule, loading: !loaded || r.loading, error: r.error ?? presetError, reload };
}

/**
 * A person's avatar. The signed-in viewer reads their current immutable
 * publication and follows upload/crop/removal events. Other accounts keep the
 * stable public URL without one API request per author in a listing.
 */
export function useAvatar(userId: string | null | undefined, o: { client?: ContentKitClient } = {}): PublicPreset | null {
  const { client, viewer } = useContentScope(o.client);
  const scope = useReadScope();
  const { kinds } = useKinds({ client });
  const rule = kinds?.avatars;
  const own = !!rule && !!userId && userId === viewer;
  const prefix = rule ? (isPattern(rule.from) ? ruleDir(rule.from) : rule.from) : undefined;
  const current = useMediaRead({ kind: rule?.kind ?? "", id: userId ?? "" }, { client, prefix, read: own ? undefined : null });
  const [last, setLast] = useState<{ client: ContentKitClient; scope: string; id: string; read: ReadResult } | null>(null);
  if (!own && last) setLast(null);
  if (own && current.read && (last?.read !== current.read || last.client !== client || last.scope !== scope || last.id !== userId)) {
    setLast({ client, scope, id: userId!, read: current.read });
  }
  const transient = current.error && (current.error.transient || current.error.status >= 500);
  if (own && current.error && !transient && last) setLast(null);
  const { refresh } = current;
  const read = current.read ?? (own && (current.loading || transient) && last?.client === client && last.scope === scope && last.id === userId ? last.read : null);


  // Another app can change the shared avatar while this tab is inactive.
  // The read store deduplicates refreshes from multiple consumers.
  useEffect(() => {
    if (!own) return;
    const visible = () => { if (document.visibilityState === "visible") refresh(); };
    window.addEventListener("focus", refresh);
    document.addEventListener("visibilitychange", visible);
    return () => {
      window.removeEventListener("focus", refresh);
      document.removeEventListener("visibilitychange", visible);
    };
  }, [own, refresh]);

  return useMemo(() => {
    if (!rule || !userId) return null;
    if (!own) return stableImage(rule, userId);
    const publication = read?.public?.find((p) => p.preset === rule.name);
    return publication?.renditions.length ? { preset: rule.name, aspect: rule.aspect, renditions: publication.renditions } : null;
  }, [rule, userId, own, read]);
}
