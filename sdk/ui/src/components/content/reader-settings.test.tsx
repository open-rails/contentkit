// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { ReaderSettings } from "./reader-settings.js";

const languages = [{ value: "en", label: "English" }, { value: "es", label: "Español" }];
afterEach(() => { localStorage.clear(); });

it("combines language and settings in one accessible trigger, including for guests", async () => {
  const fetcher = vi.fn(async () => Response.json({ applies_to: ["gallery", "blog"], filters: [], include_deleted: false }));
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  const changed = vi.fn();
  function Host() {
    const [language, setLanguage] = useState("en");
    return <ContentKitProvider client={client} viewer={null}><ReaderSettings languages={languages} language={language} onLanguageChange={(next) => { changed(next); setLanguage(next); }} /></ContentKitProvider>;
  }
  const user = userEvent.setup();
  render(<Host />);
  const trigger = screen.getByRole("button", { name: "Reader settings" });
  expect(trigger).toHaveTextContent("EN");
  expect(screen.queryByRole("dialog")).toBeNull();
  await user.click(trigger);
  const dialog = await screen.findByRole("dialog", { name: "Reader settings" });
  expect(within(dialog).queryByText("Show Content:")).toBeNull();
  expect(within(dialog).queryByText(/Sign in/)).toBeNull();
  await user.click(within(dialog).getByRole("combobox", { name: "Language" }));
  await user.click(await screen.findByRole("option", { name: "Español" }));
  expect(changed).toHaveBeenCalledExactlyOnceWith("es");
  expect(trigger).toHaveTextContent("ES");
  expect(screen.getByRole("combobox", { name: "Language" })).toHaveTextContent("Español");
  await user.click(within(dialog).getByRole("button", { name: "Close" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(trigger).toHaveFocus();
  expect(client.preferences.browser.snapshot().language).toBe("es");
  expect(fetcher.mock.calls.length).toBeGreaterThan(0);
});

it("works without a content provider when only language is available", async () => {
  render(<ReaderSettings languages={languages} language="en" onLanguageChange={() => {}} />);
  await userEvent.setup().click(screen.getByRole("button", { name: "Reader settings" }));
  expect(await screen.findByRole("combobox", { name: "Language" })).toHaveTextContent("English");
  expect(screen.queryByText("Show Content:")).toBeNull();
});

it("does not turn a localized URL into a saved language preference", () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/deeplink", fetch: async () => Response.json({ applies_to: ["gallery", "blog"], filters: [], include_deleted: false }) });
  const view = render(<ContentKitProvider client={client} viewer={null}><ReaderSettings languages={languages} language="es" onLanguageChange={() => {}} /></ContentKitProvider>);
  expect(client.preferences.browser.snapshot().language).toBeUndefined();
  view.rerender(<ContentKitProvider client={client} viewer={null}><ReaderSettings languages={languages} language="en" onLanguageChange={() => {}} /></ContentKitProvider>);
  expect(client.preferences.browser.snapshot().language).toBeUndefined();
});

it("shows the current page language while allowing an explicit return to the saved language", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/saved-language", fetch: async () => Response.json({ applies_to: ["gallery", "blog"], filters: [], include_deleted: false }) });
  client.preferences.browser.setLanguage("en");
  const changed = vi.fn();
  render(<ContentKitProvider client={client} viewer={null}><ReaderSettings languages={languages} language="es" onLanguageChange={changed} /></ContentKitProvider>);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Reader settings" }));
  const selector = await screen.findByRole("combobox", { name: "Language" });
  expect(selector).toHaveTextContent("Español");
  expect(changed).not.toHaveBeenCalled();
  await user.click(selector);
  await user.click(await screen.findByRole("option", { name: "English" }));
  await waitFor(() => expect(changed).toHaveBeenCalledExactlyOnceWith("en"));
  expect(client.preferences.browser.snapshot().language).toBe("en");
});

it("keeps failed registration inheritance visible and lets the reader retry it", async () => {
  let fail = true;
  const client = createContentKitClient({ baseUrl: "https://content.test/registration-ui", fetch: async (input) => {
    if (String(input).endsWith("/initialize") && fail) return Response.json({ error: "unavailable", code: "unavailable" }, { status: 503 });
    return Response.json({ applies_to: ["gallery", "blog"], filters: [{ entity: { id: "filter", slug: "furry", name: "Furry" }, default_visible: false, visible: false, overridden: false }], include_deleted: false });
  } });
  client.preferences.browser.setFilters([{ entity_id: "filter", visible: true }]);
  await client.preferences.inheritAfterRegistration("new-reader");
  const languagePreference = { load: async () => null, save: async () => {} };
  render(<ContentKitProvider client={client} viewer="new-reader" languagePreference={languagePreference}><ReaderSettings languages={languages} language="en" onLanguageChange={() => {}} /></ContentKitProvider>);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Reader settings" }));
  const retry = await screen.findByRole("button", { name: "Try again" });
  expect(client.preferences.snapshot().pending).toBe(true);
  fail = false;
  await user.click(retry);
  await waitFor(() => expect(client.preferences.snapshot().pending).toBe(false));
  expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
});

it("saves signed-in filters immediately and retains them when reopened", async () => {
  let visible = false;
  const fetcher = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "PATCH") visible = JSON.parse(String(init.body)).filters[0].visible;
    return Response.json({ applies_to: ["gallery", "blog"], filters: [{ entity: { id: "filter", slug: "furry", name: "Furry" }, default_visible: false, visible, overridden: visible }], include_deleted: false });
  });
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  render(<ContentKitProvider client={client} viewer="reader"><ReaderSettings languages={languages} language="en" onLanguageChange={() => {}} appearance={{ theme: "dark" }} /></ContentKitProvider>);
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Reader settings" });
  await user.click(trigger);
  const dialog = await screen.findByRole("dialog");
  expect(dialog.closest(".ckui")).toHaveAttribute("data-ckui-theme", "dark");
  expect(within(dialog).getByText("Show Content:")).toBeVisible();
  const toggle = await screen.findByRole("switch", { name: "Furry" });
  await user.click(toggle);
  await waitFor(() => expect(toggle).toBeChecked());
  expect(JSON.parse(String(fetcher.mock.calls.find(([, init]) => init?.method === "PATCH")?.[1]?.body))).toEqual({ filters: [{ entity_id: "filter", visible: true }] });
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await user.click(trigger);
  expect(await screen.findByRole("switch", { name: "Furry" })).toBeChecked();
});

it("omits the filter heading when the signed-in site has no configured filters", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async () => Response.json({ applies_to: ["gallery", "blog"], filters: [], include_deleted: false }) });
  render(<ContentKitProvider client={client} viewer="reader"><ReaderSettings languages={languages} language="en" onLanguageChange={() => {}} /></ContentKitProvider>);
  await userEvent.setup().click(screen.getByRole("button", { name: "Reader settings" }));
  await waitFor(() => expect(screen.queryByText("Show Content:")).toBeNull());
  expect(screen.getByRole("combobox", { name: "Language" })).toBeVisible();
  expect(screen.queryByRole("switch")).toBeNull();
});

it("offers only the language, without loading filters, when filters are off", async () => {
  const fetcher = vi.fn(async (_url: RequestInfo | URL) => Response.json({ applies_to: ["gallery", "blog"], filters: [{ entity: { id: "filter", slug: "furry", name: "Furry" }, default_visible: false, visible: false, overridden: false }], include_deleted: false }));
  const client = createContentKitClient({ baseUrl: "https://content.test/admin", fetch: fetcher });
  render(<ContentKitProvider client={client} viewer="admin"><ReaderSettings filters={false} languages={languages} language="en" onLanguageChange={() => {}} /></ContentKitProvider>);
  await userEvent.setup().click(screen.getByRole("button", { name: "Reader settings" }));
  expect(await screen.findByRole("combobox", { name: "Language" })).toBeVisible();
  expect(screen.queryByText("Show Content:")).toBeNull();
  expect(screen.queryByRole("switch")).toBeNull();
  expect(fetcher.mock.calls.some(([url]) => String(url).includes("content-filters"))).toBe(false);
});
