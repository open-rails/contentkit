import React, { useEffect, useState } from "../../../sdk/ui/node_modules/react/index.js";
import { createRoot } from "../../../sdk/ui/node_modules/react-dom/client.js";
import { createContentKitClient } from "../../../sdk/ui/dist/client.js";
import { ContentKitProvider, useBeacons, useReadingEngagement } from "../../../sdk/ui/dist/react.js";
import { ContentGrid, SearchBox, SearchResults } from "../../../sdk/ui/dist/index.js";
import "../../../sdk/ui/dist/styles.css";

const fixture = await fetch("/fixture.json").then((r) => r.json());
const nativeFetch = globalThis.fetch.bind(globalThis);
const accepted = [];
globalThis.fetch = async (...args) => {
  const response = await nativeFetch(...args);
  if (String(args[0]).includes("/engagement") && args[1]?.body && response.ok) {
    accepted.push(...JSON.parse(args[1].body).snapshots);
  }
  return response;
};
const client = createContentKitClient({ baseUrl: location.origin + "/content", token: () => fixture.token });
let navigate;
let flushReader = async () => {};
let flushBeacons = async () => {};
let stopReading = () => {};
const h = React.createElement;
const route = () => location.pathname + location.search;

function Reader({ content, version }) {
  const [body, setBody] = useState(null);
  const [reading, setReading] = useState(true);
  stopReading = () => setReading(false);
  const engagement = useReadingEngagement(content, version, { client, enabled: body !== null && reading, kind: "article" });
  flushReader = engagement.flush;
  useEffect(() => {
    let current = true;
    client.contents.version(content, version).then((value) => { if (current) setBody(value.body); });
    return () => { current = false; };
  }, [content, version]);
  return h("main", { style: { padding: "24px", maxWidth: "720px" } },
    h("a", { href: "/results", onClick: (event) => { event.preventDefault(); navigate("/results"); } }, "Back to results"),
    body === null ? h("p", null, "Loading reader") : h("article", {
      id: "measured-body", ref: engagement.text, tabIndex: 0,
      "data-content": content, "data-version": version,
      style: { minHeight: "160px", width: "640px", fontSize: "20px", padding: "24px", background: "#eee" },
    }, body));
}

function BeaconFlush() {
  const beacons = useBeacons({ client });
  flushBeacons = () => beacons.flush();
  return null;
}

function App() {
  const [path, setPath] = useState(route());
  navigate = (to, options) => {
    history[options?.replace ? "replaceState" : "pushState"]({}, "", to);
    setPath(route());
  };
  useEffect(() => {
    const changed = () => setPath(route());
    addEventListener("popstate", changed);
    return () => removeEventListener("popstate", changed);
  }, []);
  const pathname = new URL(path, location.origin).pathname;
  const reader = pathname.match(/^\/read\/([^/]+)\/([^/]+)$/);
  return h(ContentKitProvider, { client, viewer: fixture.user, language: "en", navigate },
    h(BeaconFlush), reader ? h(Reader, { key: reader[1], content: reader[1], version: reader[2] }) : pathname === "/browse" ?
      h("section", { id: "results", style: { padding: "24px", width: "960px" } }, h(ContentGrid, {
        client, query: { kind: "article", limit: 2, sort: "newest" }, surface: "browse",
        href: (item) => `/read/${item.id}/${item.version.id}`, showCounts: false, owner: false,
      })) : h("main", { style: { padding: "24px", width: "960px" } },
        h("section", { id: "suggestions", style: { marginBottom: "24px" } }, h(SearchBox, {
          client, kind: "article", recent: false, placeholder: "Find attributed content", presentation: "inline",
          href: (item) => `/read/${item.id}/${item.version.id}`,
          onSearch: () => {},
        })),
        h("section", { id: "results" }, h(SearchResults, {
          client, query: { q: fixture.query, kind: "article", limit: 2, sort: "relevance" },
          pagination: "pages", page: 2, pageSize: 2,
          href: (item) => `/read/${item.id}/${item.version.id}`,
          showCounts: false, owner: false,
        }))));
}
createRoot(document.getElementById("root")).render(h(App));
window.attribution = {
  fixture, client, accepted,
  navigate: (...args) => navigate(...args),
  stopReading: () => stopReading(),
  flush: async () => { await flushReader(); await flushBeacons(); },
  replay: async () => {
    const last = accepted.at(-1);
    if (!last) throw Error("no accepted measured snapshot to replay");
    await client.discovery.engagement([last]);
    await client.discovery.engagement([last]);
    return last;
  },
};
