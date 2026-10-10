import { ContentKitUiProvider } from "@openrails/contentkit-ui";
import { ContentKitProvider } from "@openrails/contentkit-ui/react";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { AbrDemo, GalleryDemo, PlayerDemo, WatchDemo } from "./gallery";
import { client, q, theme } from "./session";

const page = q.get("page");
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ContentKitProvider client={client}>
      <ContentKitUiProvider appearance={{ theme }}>{{ player: <PlayerDemo />, abr: <AbrDemo />, watch: <WatchDemo /> }[page ?? ""] ?? <GalleryDemo />}</ContentKitUiProvider>
    </ContentKitProvider>
  </StrictMode>,
);
