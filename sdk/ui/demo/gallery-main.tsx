import { UploadUiProvider, type UploadUiTheme } from "@openrails/contentkit-upload/ui";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { AbrDemo, GalleryDemo, PlayerDemo } from "./gallery";

const q = new URLSearchParams(location.search);
const theme = (q.get("theme") ?? "light") as UploadUiTheme;
const dark = theme === "dark";
document.documentElement.style.colorScheme = dark ? "dark" : "light";
document.body.style.cssText = `margin:0;font-family:Inter,ui-sans-serif,system-ui,sans-serif;background:${dark ? "#09090b" : "#fafafa"};color:${dark ? "#fafafa" : "#09090b"}`;

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <UploadUiProvider appearance={{ theme }}>{q.get("page") === "player" ? <PlayerDemo /> : q.get("page") === "abr" ? <AbrDemo /> : <GalleryDemo />}</UploadUiProvider>
  </StrictMode>,
);
