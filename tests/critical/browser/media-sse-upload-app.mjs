import React from "../../../sdk/ui/node_modules/react/index.js";
import { createRoot } from "../../../sdk/ui/node_modules/react-dom/client.js";
import { ContentUpload, ContentEditor, ContentKitUiRoot } from "../../../sdk/ui/dist/index.js";
import { ContentKitProvider } from "../../../sdk/ui/dist/react/provider.js";
import { createContentKitClient } from "../../../sdk/ui/dist/client.js";
import "../../../sdk/ui/dist/styles.css";
let credential;
window.mediaAcceptance = {
  rotateCredential(token) { credential = token; },
  initialize(fixture) {
    credential = fixture.token;
    const client = createContentKitClient({ baseUrl: "/content", token: () => credential });
    window.mediaClient = client;
    const id = new URL(location.href).searchParams.get("id");
    const body = id ? React.createElement(ContentEditor, { client, kind: "gallery", id, languages: ["en"] }) :
      React.createElement(ContentUpload, { client, kind: "gallery", defaults: { language: "en" }, onOpen: (content) => { location.href = `/media-sse-upload.html?id=${encodeURIComponent(content.id)}`; } });
    createRoot(document.querySelector("#root")).render(React.createElement(ContentKitProvider, { client, viewer: fixture.viewer }, React.createElement(ContentKitUiRoot, { className: "p-6" }, body)));
  },
};
