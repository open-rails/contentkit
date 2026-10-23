import { describe, expect, it } from "vitest";
import { attributeInput, fold, fromLocalInput, humanize, letterOf, parseAttribute, parseAttributes, slugify, toLocalInput } from "./entity-fields.js";

describe("group fields", () => {
  it("folds and buckets names as ContentKit compares them", () => {
    expect(fold("  Émile   Zola ")).toBe("emile zola");
    expect(letterOf("Éclair")).toBe("e");
    expect(letterOf("ñandú")).toBe("n");
    expect(letterOf("3x3 Eyes")).toBe("#");
    expect(letterOf("進撃の巨人")).toBe("#");
    expect(letterOf("")).toBe("#");
  });

  it("slugifies names", () => {
    expect(slugify("Night Sky / Stars!")).toBe("night-sky-stars");
    expect(slugify("Café  au lait")).toBe("cafe-au-lait");
    expect(slugify("進撃の巨人")).toBe("進撃の巨人");
  });

  it("parses attribute fields by type and refuses the wrong type", () => {
    expect(parseAttribute({ type: "string" }, "  x ")).toBe("x");
    expect(parseAttribute({ type: "string" }, " ")).toBeUndefined();
    expect(parseAttribute({ type: "int" }, "42")).toBe(42);
    expect(() => parseAttribute({ type: "int" }, "4.2")).toThrow(RangeError);
    expect(parseAttribute({ type: "bool" }, true)).toBe(true);
    expect(parseAttribute({ type: "bool" }, false)).toBeUndefined();
    expect(parseAttribute({ type: "enum", values: ["artist", "circle"] }, "circle")).toBe("circle");
    expect(() => parseAttribute({ type: "enum", values: ["artist"] }, "company")).toThrow(RangeError);
    const t = parseAttribute({ type: "time" }, "2026-10-12T14:30") as string;
    expect(new Date(t).getTime()).toBe(new Date(2026, 9, 12, 14, 30).getTime());
    expect(attributeInput({ type: "time" }, t)).toBe("2026-10-12T14:30");
    expect(attributeInput({ type: "bool" }, undefined)).toBe(false);
    expect(attributeInput({ type: "int" }, 7)).toBe("7");
  });

  it("keeps declared attributes only, dropping empty ones", () => {
    const declared = { format: { type: "enum" as const, values: ["doujin", "manga"] }, pages: { type: "int" as const }, link: { type: "string" as const } };
    expect(parseAttributes(declared, { format: "manga", pages: "", link: "https://x", extra: "y" })).toEqual({ format: "manga", link: "https://x" });
  });

  it("requires configured fields without rejecting false or zero values", () => {
    const declared = { format: { type: "enum", values: ["doujin", "cgset"], required: true } };
    for (const form of [{}, { format: "" }, { format: "  " }] as Record<string, string | boolean>[]) expect(() => parseAttributes(declared, form)).toThrow("Format is required");
    expect(parseAttributes(declared, { format: "cgset" })).toEqual({ format: "cgset" });
    expect(parseAttributes({ enabled: { type: "bool", required: true }, count: { type: "int", required: true } }, { enabled: false, count: "0" })).toEqual({ enabled: false, count: 0 });
  });

  it("round-trips local datetime inputs", () => {
    expect(toLocalInput(fromLocalInput("2026-01-02T03:04"))).toBe("2026-01-02T03:04");
    expect(fromLocalInput("")).toBeUndefined();
    expect(toLocalInput(null)).toBe("");
  });

  it("labels identifiers", () => {
    expect(humanize("voice_actor")).toBe("Voice actor");
  });
});
