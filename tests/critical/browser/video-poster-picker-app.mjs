import React from "../../../sdk/ui/node_modules/react/index.js";
import { createRoot } from "../../../sdk/ui/node_modules/react-dom/client.js";
import { VideoPosterPicker, ContentKitUiRoot } from "../../../sdk/ui/dist/index.js";
import { ContentKitProvider } from "../../../sdk/ui/dist/react/provider.js";
import { createContentKitClient } from "../../../sdk/ui/dist/client.js";
import "../../../sdk/ui/dist/styles.css";
const root = createRoot(document.querySelector("#root"));
// The shipped picker, as a host mounts it: its saves and failures are recorded.
window.posterPicker = {
  open(fixture, props) {
    const client = createContentKitClient({ baseUrl: "/content", token: () => fixture.token });
    window.picker = { saved: null, closed: false, errors: [] };
    root.render(React.createElement(ContentKitProvider, { client, onError: (error, info) => window.picker.errors.push(`${info.operation}: ${error.code} ${error.message}`) },
      React.createElement(ContentKitUiRoot, { className: "p-6" }, React.createElement(VideoPosterPicker, {
        key: props.item.id, open: true, item: props.item, frames: props.frames,
        onOpenChange: (open) => { window.picker.closed = !open; },
        onChange: (poster) => { window.picker.saved = poster; },
      }))));
  },
};
