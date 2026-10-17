// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { isReleaseDate, localReleaseDate, ReleaseDatePicker } from "./release-date-picker.js";

it("validates real past calendar dates without converting them to UTC days", () => {
  expect(localReleaseDate(new Date(2026, 0, 2, 0, 30))).toBe("2026-01-02");
  for (const date of ["", "2000-02-29", "0001-01-01", "2026-10-16"]) expect(isReleaseDate(date, "2026-10-16")).toBe(true);
  for (const date of ["1900-02-29", "2026-02-30", "2026-10-17", "2026-1-02", "2026-01", "0000-01-01", "nonsense"]) expect(isReleaseDate(date, "2026-10-16")).toBe(false);
});

it("offers a native date calendar with an editable year and resets to latest", () => {
  const changed = vi.fn();
  const view = render(<ReleaseDatePicker value="2007-04-18" max="2026-10-16" onValueChange={changed} />);
  const input = screen.getByLabelText("Through date");
  expect(input).toHaveAttribute("type", "date");
  expect(input).toHaveAttribute("max", "2026-10-16");
  expect(input).toHaveValue("2007-04-18");
  fireEvent.change(input, { target: { value: "1998-12-31" } });
  expect(changed).toHaveBeenLastCalledWith("1998-12-31");
  changed.mockClear();
  fireEvent.change(input, { target: { value: "2026-10-17" } });
  expect(changed).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Latest" }));
  expect(changed).toHaveBeenLastCalledWith("");
  view.rerender(<ReleaseDatePicker max="2026-10-16" onValueChange={changed} />);
  expect(screen.getByRole("button", { name: "Latest" })).toBeDisabled();
});

it("makes invalid URL dates explicit and allows recovery with Latest", () => {
  const changed = vi.fn();
  render(<ReleaseDatePicker value="2026-02-30" max="2026-10-16" onValueChange={changed} />);
  expect(screen.getByRole("alert")).toHaveTextContent("Choose a valid date on or before today.");
  expect(screen.getByLabelText("Through date")).toHaveAttribute("aria-invalid", "true");
  fireEvent.click(screen.getByRole("button", { name: "Latest" }));
  expect(changed).toHaveBeenCalledWith("");
});
