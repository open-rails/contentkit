import { cn } from "cn";
import { createContext, useContext, type ComponentProps, type CSSProperties } from "react";
import { appearanceStyle, appearanceTheme, type UploadUiAppearance, type UploadUiTheme } from "./appearance.js";

export const AppearanceContext = createContext<UploadUiAppearance | undefined>(undefined);

export interface ScopeProps {
  className: string;
  "data-ckui-theme": UploadUiTheme;
  style?: CSSProperties;
}

/** Props that make an element a styling root; portals carry them too. */
export function useScopeProps(appearance?: UploadUiAppearance): ScopeProps {
  const ctx = useContext(AppearanceContext);
  const a = appearance ?? ctx;
  return { className: "ckui", "data-ckui-theme": appearanceTheme(a), style: appearanceStyle(a) };
}

/** Every in-page surface renders inside one of these. */
export function UploadUiRoot({ className, style, appearance, ...props }: ComponentProps<"div"> & { appearance?: UploadUiAppearance }) {
  const scope = useScopeProps(appearance);
  return <div data-ckui-theme={scope["data-ckui-theme"]} className={cn(scope.className, className)} style={{ ...scope.style, ...style }} {...props} />;
}
