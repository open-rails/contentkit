import { createRoot } from "react-dom/client";
import { ContentKitUiProvider, NotificationBell, NotificationInbox } from "@openrails/contentkit-ui";
import { ContentKitProvider } from "@openrails/contentkit-ui/react";
import type { NotificationItem } from "@openrails/contentkit-ui/client";
import { client, theme, viewer, q } from "./session.js";

const renderItem = (item: NotificationItem) => {
  const data = item.data as { title?: string; message?: string };
  return <div><strong>{data.title ?? "A new update"}</strong>{data.message && <p style={{ margin: "8px 0 0", lineHeight: 1.6 }}>{data.message}</p>}</div>;
};

createRoot(document.getElementById("root")!).render(
  <ContentKitUiProvider appearance={{ theme }} language={q.get("language") ?? "en"}>
    <ContentKitProvider client={client} viewer={viewer}>
      <main style={{ maxWidth: 700, margin: "40px auto", padding: "0 20px" }}>
        <header style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 32 }}>
          <span style={{ fontSize: 20, fontWeight: 600 }}>Your workspace</span>
          <NotificationBell renderItem={renderItem} pageSize={2} unreadRefreshInterval={false} />
        </header>
        <NotificationInbox renderItem={renderItem} pageSize={2} />
      </main>
    </ContentKitProvider>
  </ContentKitUiProvider>,
);
