import type { CSSProperties } from "react";

/**
 * `light`/`dark` force a palette, `auto` follows the OS, `inherit` reads the
 * host's shadcn tokens (`--primary`, …) and its `.dark` class.
 */
export type UploadUiTheme = "light" | "dark" | "auto" | "inherit";

/** Raw CSS values applied as `--ckui-*` custom properties on every root. */
export interface UploadUiVariables {
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
}

export interface UploadUiAppearance {
  theme?: UploadUiTheme;
  variables?: UploadUiVariables;
}

const kebab = (k: string) => k.replace(/[A-Z]/g, (c) => "-" + c.toLowerCase());

export function appearanceStyle(a: UploadUiAppearance | undefined): CSSProperties | undefined {
  if (!a?.variables) return undefined;
  const style: Record<string, string> = {};
  for (const [key, value] of Object.entries(a.variables)) {
    if (value) style[key === "fontFamily" ? "--ckui-font" : `--ckui-${kebab(key)}`] = value;
  }
  return style as CSSProperties;
}

export function appearanceTheme(a: UploadUiAppearance | undefined): UploadUiTheme {
  return a?.theme ?? "auto";
}
