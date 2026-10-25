import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { resolve, sep } from "node:path";
import { chromium } from "../../../sdk/ui/node_modules/@playwright/test/index.mjs";

// Only static built-package modules are fulfilled here. API, gateway and S3
// traffic use their real local HTTP servers with ordinary browser security.
export async function runBrowserTransports(fixture, plan) {
  const browser = await chromium.launch({
    headless: true,
    ...(process.env.CONTENTKIT_CRITICAL_CHROMIUM
      ? { executablePath: process.env.CONTENTKIT_CRITICAL_CHROMIUM }
      : {}),
  });
  const diagnostics = [];
  try {
    const context = await browser.newContext();
    await context.grantPermissions(["local-network-access"], {
      origin: "https://critical.invalid",
    });
    const page = await context.newPage();
    const roots = {
      sdk: fileURLToPath(new URL("../../../sdk/ui/dist/", import.meta.url)),
      noble: fileURLToPath(
        new URL("../../../sdk/ui/node_modules/@noble/hashes/", import.meta.url),
      ),
    };
    page.on("console", (message) => {
      if (message.type() === "error") diagnostics.push(message.text());
    });
    page.on("requestfailed", (request) =>
      diagnostics.push(request.failure()?.errorText ?? "request failed"),
    );
    await page.route("https://critical.invalid/**", async (route) => {
      const path = new URL(route.request().url()).pathname;
      if (path === "/") {
        await route.fulfill({
          contentType: "text/html",
          body: '<!doctype html><title>Real browser transport</title><script type="importmap">{"imports":{"@noble/hashes/":"/noble/"}}</script>',
        });
        return;
      }
      const [category, ...parts] = path.slice(1).split("/");
      const root = roots[category];
      if (!root) {
        await route.abort();
        return;
      }
      const file = resolve(root, parts.join("/"));
      assert(file.startsWith(root.endsWith(sep) ? root : root + sep));
      await route.fulfill({
        contentType: "text/javascript",
        body: await readFile(file),
      });
    });
    await page.goto("https://critical.invalid/");
    const report = await page.evaluate(
      async ({ fixture, plan }) => {
        const sdk = await import("/sdk/client.js");
        const client = sdk.createContentKitClient({
          baseUrl: fixture.base,
          token: () => fixture.owner,
        });
        const xhr = new XMLHttpRequest();
        client.media.hls.segment(xhr, fixture.segment);
        xhr.responseType = "arraybuffer";
        const segment = await new Promise((resolve, reject) => {
          xhr.onload = () =>
            resolve({
              status: xhr.status,
              size: xhr.response.byteLength,
              credentials: xhr.withCredentials,
            });
          xhr.onerror = () =>
            reject(new Error("real segment XMLHttpRequest failed"));
          xhr.send();
        });
        const credentialed = new XMLHttpRequest();
        sdk
          .createContentKitClient({
            baseUrl: fixture.base,
            credentials: "include",
          })
          .media.hls.segment(credentialed, fixture.segment);
        const raw = Uint8Array.from(atob(fixture.source), (x) =>
          x.charCodeAt(0),
        );
        const file = new File([raw], "source.mp4", { type: "video/mp4" });
        const progress = [];
        await sdk.xhrTransport(plan.put, file, {
          onProgress: (n) => progress.push(n),
        });
        const abort = new AbortController();
        abort.abort();
        let aborted;
        try {
          await sdk.xhrTransport(plan.put, file, { signal: abort.signal });
          aborted = "incorrect_success";
        } catch (error) {
          aborted = error.code;
        }
        return {
          segment,
          credentialed: credentialed.withCredentials,
          progress,
          bytes: file.size,
          aborted,
        };
      },
      { fixture, plan },
    );
    assert.equal(report.segment.status, 200);
    assert(report.segment.size > 100);
    assert.equal(report.segment.credentials, false);
    assert.equal(report.credentialed, true);
    assert.equal(report.progress.at(-1), report.bytes);
    assert.equal(report.aborted, "aborted");
  } catch (error) {
    throw new Error(
      String(error) + "\nBrowser diagnostics: " + diagnostics.join("\n"),
    );
  } finally {
    await browser.close();
  }
}
