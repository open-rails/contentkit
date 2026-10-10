import { ContentKitUiProvider, type ContentKitUiTheme } from "@openrails/contentkit-ui";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { AbrDemo, GalleryDemo, PlayerDemo, WatchDemo } from "./gallery";

const q = new URLSearchParams(location.search);
const theme = (q.get("theme") ?? "light") as ContentKitUiTheme;
const dark = theme === "dark";
document.documentElement.style.colorScheme = dark ? "dark" : "light";
document.body.style.cssText = `margin:0;font-family:Inter,ui-sans-serif,system-ui,sans-serif;background:${dark ? "#09090b" : "#fafafa"};color:${dark ? "#fafafa" : "#09090b"}`;

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ContentKitUiProvider appearance={{ theme }}>{{ player: <PlayerDemo />, abr: <AbrDemo />, watch: <WatchDemo /> }[q.get("page") ?? ""] ?? <GalleryDemo />}</ContentKitUiProvider>
  </StrictMode>,
);
