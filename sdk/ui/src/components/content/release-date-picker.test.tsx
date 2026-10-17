// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { isReleaseDate, localReleaseDate, ReleaseDatePicker } from "./release-date-picker.js";

it("validates real past calendar dates without converting them to UTC days", () => {
  expect(localReleaseDate(new Date(2026, 0, 2, 0, 30))).toBe("2026-01-02");
  for (const date of ["", "2000-02-29", "0001-01-01", "2026-10-16"]) expect(isReleaseDate(date, "2026-10-16")).toBe(true);
  for (const date of ["1900-02-29", "2026-02-30", "2026-10-17", "2026-1-02", "2026-01", "0000-01-01", "nonsense"]) expect(isReleaseDate(date, "2026-10-16")).toBe(false);
});

it("jumps directly between years/months and commits only the selected calendar day", async () => {
  const changed = vi.fn();
  render(<ReleaseDatePicker value="2007-04-18" max="2026-10-16" onValueChange={changed} />);
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Go to day" });
  await user.click(trigger);
  const calendar = await screen.findByRole("dialog", { name: "Go to day" });
  expect(calendar.querySelector('input[type="date"]')).toBeNull();
  expect(screen.queryByRole("button", { name: "Latest" })).toBeNull();
  expect(screen.queryByText("Through date")).toBeNull();
  await user.selectOptions(within(calendar).getByRole("combobox", { name: /year/i }), "1998");
  await user.selectOptions(within(calendar).getByRole("combobox", { name: /month/i }), "11");
  expect(changed).not.toHaveBeenCalled();
  await user.click(within(calendar).getByRole("button", { name: /December 31.*1998/ }));
  expect(changed).toHaveBeenCalledExactlyOnceWith("1998-12-31");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await waitFor(() => expect(trigger).toHaveFocus());
});

it("disables future days and never commits them", async () => {
  const changed = vi.fn();
  render(<ReleaseDatePicker value="2026-10-15" max="2026-10-16" onValueChange={changed} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Go to day" }));
  const tomorrow = await screen.findByRole("button", { name: /October 17.*2026/ });
  expect(tomorrow).toBeDisabled();
  await user.click(tomorrow);
  expect(changed).not.toHaveBeenCalled();
});

it("supports arrow-key day selection and returns focus to its trigger", async () => {
  const changed = vi.fn();
  render(<ReleaseDatePicker value="2026-10-15" max="2026-10-16" onValueChange={changed} />);
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Go to day" });
  await user.click(trigger);
  const selected = await screen.findByRole("button", { name: /October 15.*2026.*selected/ });
  await waitFor(() => expect(selected).toHaveFocus());
  await user.keyboard("{ArrowRight}{Enter}");
  expect(changed).toHaveBeenCalledExactlyOnceWith("2026-10-16");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await waitFor(() => expect(trigger).toHaveFocus());
});

it("dismisses with Escape or an outside click without changing the selected date", async () => {
  const changed = vi.fn();
  render(<><ReleaseDatePicker value="2026-10-15" max="2026-10-16" onValueChange={changed} /><button>Outside</button></>);
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Go to day" });
  await user.click(trigger);
  await screen.findByRole("dialog");
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await waitFor(() => expect(trigger).toHaveFocus());
  await user.click(trigger);
  await screen.findByRole("dialog");
  const outside = screen.getByRole("button", { name: "Outside" });
  await user.click(outside);
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await waitFor(() => expect(outside).toHaveFocus());
  expect(changed).not.toHaveBeenCalled();
});

it("keeps its portal themed and recovers an invalid URL date through the calendar", async () => {
  const changed = vi.fn();
  render(<ReleaseDatePicker value="2026-02-30" max="2026-10-16" onValueChange={changed} appearance={{ theme: "dark", variables: { primary: "rebeccapurple" } }} />);
  expect(screen.getByRole("alert")).toHaveTextContent("Choose a valid date on or before today.");
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Go to day" });
  expect(trigger).toHaveAttribute("aria-invalid", "true");
  await user.click(trigger);
  const calendar = await screen.findByRole("dialog");
  expect(calendar.closest(".ckui")).toHaveAttribute("data-ckui-theme", "dark");
  expect(calendar.closest(".ckui")).toHaveStyle({ "--ckui-primary": "rebeccapurple" });
  await user.click(within(calendar).getByRole("button", { name: /October 16.*2026/ }));
  expect(changed).toHaveBeenCalledExactlyOnceWith("2026-10-16");
});
