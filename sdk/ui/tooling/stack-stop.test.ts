import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";
import { expect, it } from "vitest";

// Exercise the actual stop closure without starting Docker or a harness.
// Child exit models main.go's teardown removing the containers.
function harness(options: { keepLogs?: boolean; logFailure?: boolean }) {
  const source = ts.createSourceFile("stack.ts", readFileSync(new URL("../e2e/support/stack.ts", import.meta.url), "utf8"), ts.ScriptTarget.Latest, true);
  const findStop = (node: ts.Node): ts.ArrowFunction | undefined => {
    if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.name.text === "stop" && node.initializer && ts.isArrowFunction(node.initializer)) return node.initializer;
    return ts.forEachChild(node, findStop);
  };
  const initializer = findStop(source);
  if (!initializer) throw new Error("stack stop closure was not found");
  const events: string[] = [];
  let containers = true;
  let output = "";
  const stop = runInNewContext(ts.transpile(`(${initializer.getText(source)})`, { target: ts.ScriptTarget.ES2022 }), {
    server: {
      exitCode: null,
      kill() { events.push("terminate"); },
      once(_event: string, listener: () => void) { containers = false; events.push("child teardown"); listener(); },
    },
    env: () => options.keepLogs,
    dc: (...args: string[]) => {
      if (args.includes("logs")) {
        events.push("logs");
        if (!containers || options.logFailure) throw new Error("logs unavailable");
        return "worker and gateway diagnostics";
      }
      if (args.includes("down")) { containers = false; events.push("compose cleanup"); }
      return "";
    },
    process: { stderr: { write: (text: string) => { output += text; } } },
    rmSync: () => events.push("work cleanup"),
    work: "owned temporary work",
    log: () => {},
  }) as () => Promise<void>;
  return { stop, events, output: () => output };
}

it("retains opted-in worker logs before child teardown removes containers", async () => {
  const h = harness({ keepLogs: true });
  await h.stop();
  expect(h.output()).toBe("worker and gateway diagnostics");
  expect(h.events.indexOf("logs")).toBeLessThan(h.events.indexOf("child teardown"));
  expect(h.events).toContain("compose cleanup");
  expect(h.events).toContain("work cleanup");
});

it("keeps log collection opt-in and still cleans up", async () => {
  const h = harness({});
  await h.stop();
  expect(h.events).not.toContain("logs");
  expect(h.output()).toBe("");
  expect(h.events).toContain("child teardown");
  expect(h.events).toContain("compose cleanup");
  expect(h.events).toContain("work cleanup");
});

it("still terminates and cleans up when optional log collection fails", async () => {
  const h = harness({ keepLogs: true, logFailure: true });
  await h.stop();
  expect(h.events).toContain("logs");
  expect(h.events).toContain("child teardown");
  expect(h.events).toContain("compose cleanup");
  expect(h.events).toContain("work cleanup");
});
