import React from "../../../sdk/ui/node_modules/react/index.js";
import { createRoot } from "../../../sdk/ui/node_modules/react-dom/client.js";
import { createContentKitClient } from "../../../sdk/ui/dist/client.js";
import { ContentKitProvider } from "../../../sdk/ui/dist/react/provider.js";
import { ContentGrid, PopularContentGrid, PostList } from "../../../sdk/ui/dist/index.js";

const client = createContentKitClient({ baseUrl: "/content" });
const root = createRoot(document.querySelector("#root"));
const query = { kind: "article", summary: false };
function render(period) {
  root.render(React.createElement(ContentKitProvider, { client, viewer: null },
    React.createElement(PopularContentGrid, {
      query, period, loadMedia: false, showFilteredSummary: false,
      renderCard: card => React.createElement("span", { "data-item": card.id }, card.title),
    })));
}
function show(element) {
  root.render(React.createElement(ContentKitProvider, { client, viewer: null }, element));
}
window.popular = {
  mount: () => render(undefined),
  paged: () => show(React.createElement(ContentGrid, {
    query: { kind: "article", sort: "newest" }, pagination: "pages", pageSize: 1, locked: "collapse", loadMedia: false, showFilteredSummary: false,
    renderCard: card => React.createElement("span", { "data-item": card.id }, card.title),
  })),
  posts: trending => show(React.createElement(PostList, { kind: "article", trending })),
  range: (from, until) => render({ from, until }),
  async cutoff(from, until) {
    try {
      await client.discovery.list({ kind: "article", sort: "popular", activity_from: from, activity_until: until });
      throw Error("out-of-coverage range unexpectedly accepted");
    } catch (error) {
      if (error.status !== 400) throw error;
      return { status: error.status, details: error.details };
    }
  },
};
