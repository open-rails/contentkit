// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { ContentFavorites } from "./favorites.js";

it("asks a signed-out visitor to sign in instead of reading account favorites", async () => {
  const fetch = vi.fn(async () => Response.json({}));
  const onSignIn = vi.fn();
  render(<ContentKitProvider client={createContentKitClient({ baseUrl: "https://content.test", fetch })} viewer={null} onSignIn={onSignIn}>
    <ContentFavorites />
  </ContentKitProvider>);
  expect(screen.getByText("Sign in to save favorites")).toBeVisible();
  await userEvent.setup().click(screen.getByRole("button", { name: "Sign in" }));
  expect(onSignIn).toHaveBeenCalledTimes(1);
  expect(fetch.mock.calls.some(([url]: unknown[]) => String(url).includes("favorites"))).toBe(false);
});
