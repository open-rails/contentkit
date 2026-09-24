import type { Plugin } from "vite"

import { isolateCss } from "./isolate.ts"

const STYLE_ELEMENT_ID = "openrails-contentkit-upload-styles"

// Installing from the entry lets hosts import components alone; the emitted
// stylesheet stays available for SSR or manual loading.
export function renderStyleInstaller(css: string): string {
  return `
const __ckuiCss = ${JSON.stringify(css)};
if (typeof document !== "undefined") {
  let __ckuiStyle = document.getElementById(${JSON.stringify(STYLE_ELEMENT_ID)});
  if (!__ckuiStyle) {
    __ckuiStyle = document.createElement("style");
    __ckuiStyle.id = ${JSON.stringify(STYLE_ELEMENT_ID)};
    (document.head || document.documentElement).appendChild(__ckuiStyle);
  }
  if (__ckuiStyle.textContent !== __ckuiCss) __ckuiStyle.textContent = __ckuiCss;
}
`
}

/** Isolates the emitted stylesheet and installs it from the given entries. */
export function ckuiCssPlugin(options: { entries: string[] }): Plugin {
  return {
    name: "openrails-contentkit-upload-css",
    enforce: "post",
    async generateBundle(_options, bundle) {
      const stylesheet = Object.values(bundle).find(
        (item) => item.type === "asset" && item.fileName.endsWith(".css")
      )
      if (!stylesheet || stylesheet.type !== "asset") {
        throw new Error("contentkit-upload build did not emit a stylesheet")
      }
      const css = await isolateCss(String(stylesheet.source))
      stylesheet.source = css

      for (const item of Object.values(bundle)) {
        if (
          item.type === "chunk" &&
          item.isEntry &&
          options.entries.includes(item.name)
        ) {
          item.code = `${renderStyleInstaller(css)}\n${item.code}`
        }
      }
    },
  }
}
