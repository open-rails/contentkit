import { useEffect, useMemo, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { ReadResult } from "../client/generated/wire.js";
import { defaultImage, isPattern, ruleDir, type PresetRule } from "../client/media/rules.js";
import type { PublicPreset } from "../client/public.js";
import { useOptionalContentKitClient } from "./context.js";
import { useMediaRead } from "./read.js";

export interface UsePresets {
  /** Every kind's public presets; empty until they load (or when the server has none). */
  presets: PresetRule[];
  loaded: boolean;
  error?: ContentKitError;
}

const settled = new WeakMap<ContentKitClient, { presets: PresetRule[]; error?: ContentKitError }>();

/** The public presets (`GET /media/presets`), fetched once per client. */
export function usePresets(o: { client?: ContentKitClient | null } = {}): UsePresets {
  const client = useOptionalContentKitClient(o.client);
  const [, rerender] = useState(0);
  const done = client ? settled.get(client) : undefined;
  useEffect(() => {
    if (!client || settled.has(client)) return;
    let live = true;
    client.media.presets().then(
      (presets) => settled.set(client, { presets }),
      (error: ContentKitError) => settled.set(client, { presets: [], error }),
    ).finally(() => live && rerender((n) => n + 1));
    return () => {
      live = false;
    };
  }, [client]);
  return { presets: done?.presets ?? [], loaded: !!done || !client, error: done?.error };
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
  /** The item's current renditions; the kind's default image when it has none; null while unknown. */
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
 * An item's public image for a preset ("avatar", "cover"): the exact
 * renditions from a read's `public` (or the host's listing), else the kind's
 * default image. Reads refresh after commits through the same client.
 */
export function usePublicImage(kind: string, id: string, preset: string, o: PublicImageOptions = {}): UsePublicImage {
  const client = useOptionalContentKitClient(o.client);
  const { presets, loaded } = usePresets({ client });
  const rule = presets.find((p) => p.kind === kind && p.name === preset);
  const listed = o.image !== undefined;
  // The read waits for the presets, so it lists only the preset's upload.
  const prefix = rule ? (isPattern(rule.from) ? ruleDir(rule.from) : rule.from) : undefined;
  const r = useMediaRead({ kind, id }, { prefix, read: listed || !loaded ? null : o.read, client });
  const image = useMemo((): { image: PublicPreset | null; isDefault: boolean } => {
    const own = listed ? o.image : r.read?.public?.find((p) => p.preset === preset);
    if (own && own.renditions.length) return { image: { preset, renditions: own.renditions, aspect: ("aspect" in own && own.aspect) || rule?.aspect }, isDefault: false };
    if (!listed && !r.read) return { image: null, isDefault: false };
    return rule ? { image: defaultImage(rule, id), isDefault: true } : { image: null, isDefault: false };
  }, [listed, o.image, r.read, preset, rule, id]);
  return { ...image, rule, loading: !loaded || r.loading, error: r.error, reload: r.reload };
}
