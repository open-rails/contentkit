import type { CSSProperties } from "react";

/**
 * `light`/`dark` force a palette, `auto` follows the OS, `inherit` reads the
 * host's shadcn tokens (`--primary`, …) and its `.dark` class.
 */
export type ContentKitUiTheme = "light" | "dark" | "auto" | "inherit";

/** Raw CSS values applied as `--ckui-*` custom properties on every root. */
export interface ContentKitUiVariables {
  background?: string;
  foreground?: string;
  card?: string;
  cardForeground?: string;
  popover?: string;
  popoverForeground?: string;
  primary?: string;
  primaryForeground?: string;
  secondary?: string;
  secondaryForeground?: string;
  muted?: string;
  mutedForeground?: string;
  accent?: string;
  accentForeground?: string;
  destructive?: string;
  warning?: string;
  border?: string;
  input?: string;
  ring?: string;
  radius?: string;
  fontFamily?: string;
  /** The video player's played range, seek thumb and pressed toggles. Default white. */
  playerAccent?: string;
}

export interface ContentKitUiAppearance {
  theme?: ContentKitUiTheme;
  variables?: ContentKitUiVariables;
}

const kebab = (k: string) => k.replace(/[A-Z]/g, (c) => "-" + c.toLowerCase());

export function appearanceStyle(a: ContentKitUiAppearance | undefined): CSSProperties | undefined {
  if (!a?.variables) return undefined;
  const style: Record<string, string> = {};
  for (const [key, value] of Object.entries(a.variables)) {
    if (value) style[key === "fontFamily" ? "--ckui-font" : `--ckui-${kebab(key)}`] = value;
  }
  return style as CSSProperties;
}

export function appearanceTheme(a: ContentKitUiAppearance | undefined): ContentKitUiTheme {
  return a?.theme ?? "auto";
}
