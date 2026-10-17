// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import { ContentKitProvider } from "../../react/index.js";
import { PagePicker } from "./page-picker.js";

const pages = [{ name: "001", url: "https://media.test/t/001.webp" }, { name: "002" }, { name: "003", url: "https://media.test/t/003.webp" }];

function Harness({ onChange }: { onChange: (v: string) => void }) {
  const [value, setValue] = useState("001");
  return (
    <PagePicker
      label="Chapter 1 starts"
      pages={pages}
      value={value}
      extras={[{ value: "anchor:intro", label: "Anchor intro" }]}
      onChange={(v) => {
        setValue(v);
        onChange(v);
      }}
    />
  );
}

it("PagePicker shows the current page and picks another by thumbnail or an extra", async () => {
  const onChange = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://content.test", fetch: vi.fn() });
  render(
    <ContentKitProvider client={client}>
      <Harness onChange={onChange} />
    </ContentKitProvider>,
  );
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Chapter 1 starts" });
  expect(trigger).toHaveTextContent("Page 1");
  await user.click(trigger);
  expect(screen.getByRole("button", { name: "Page 1: 001" })).toHaveAttribute("aria-pressed", "true");
  // A page without a signed thumbnail still shows and picks by number.
  expect(screen.getByRole("button", { name: "Page 2: 002" })).toHaveTextContent("2");
  await user.click(screen.getByRole("button", { name: "Page 3: 003" }));
  expect(onChange).toHaveBeenLastCalledWith("003");
  expect(screen.getByRole("button", { name: "Chapter 1 starts" })).toHaveTextContent("Page 3");
  await user.click(screen.getByRole("button", { name: "Chapter 1 starts" }));
  await user.click(screen.getByRole("button", { name: "Anchor intro" }));
  expect(onChange).toHaveBeenLastCalledWith("anchor:intro");
  expect(screen.getByRole("button", { name: "Chapter 1 starts" })).toHaveTextContent("Anchor intro");
});
