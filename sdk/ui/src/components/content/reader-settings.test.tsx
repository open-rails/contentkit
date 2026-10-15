// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { ReaderSettings } from "./reader-settings.js";

const languages = [{ value: "en", label: "English" }, { value: "es", label: "Español" }];

it("combines language and settings in one accessible trigger, including for guests", async () => {
  const fetcher = vi.fn();
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
  expect(fetcher).not.toHaveBeenCalled();
});

it("works without a content provider when only language is available", async () => {
  render(<ReaderSettings languages={languages} language="en" onLanguageChange={() => {}} />);
  await userEvent.setup().click(screen.getByRole("button", { name: "Reader settings" }));
  expect(await screen.findByRole("combobox", { name: "Language" })).toHaveTextContent("English");
  expect(screen.queryByText("Show Content:")).toBeNull();
});

it("saves signed-in filters immediately and retains them when reopened", async () => {
  let visible = false;
  const fetcher = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "PUT") visible = JSON.parse(String(init.body)).filters[0].visible;
    return Response.json({ filters: [{ group: { id: "filter", slug: "furry", name: "Furry" }, label: "Furry", visible, overridden: visible }], include_deleted: false });
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
  expect(JSON.parse(String(fetcher.mock.calls.find(([, init]) => init?.method === "PUT")?.[1]?.body))).toEqual({ filters: [{ group_id: "filter", visible: true }], include_deleted: false });
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await user.click(trigger);
  expect(await screen.findByRole("switch", { name: "Furry" })).toBeChecked();
});

it("omits the filter heading when the signed-in site has no configured filters", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async () => Response.json({ filters: [], include_deleted: false }) });
  render(<ContentKitProvider client={client} viewer="reader"><ReaderSettings languages={languages} language="en" onLanguageChange={() => {}} /></ContentKitProvider>);
  await userEvent.setup().click(screen.getByRole("button", { name: "Reader settings" }));
  await waitFor(() => expect(screen.queryByText("Show Content:")).toBeNull());
  expect(screen.getByRole("combobox", { name: "Language" })).toBeVisible();
  expect(screen.queryByRole("switch")).toBeNull();
});
