import fs from "node:fs";
import { describe, expect, it } from "vitest";
import { designCss, driftAgainstDesignCss, generate, outCss, parseVars, readTokens } from "./tokens-lib";

describe("design tokens", () => {
  const generated = generate(readTokens());

  it("src/styles/tokens.css is exactly what tokens.json generates", () => {
    expect(fs.readFileSync(outCss, "utf8")).toBe(generated);
  });

  it("agrees with the hand-exported docs/design/tokens.css, variable by variable", () => {
    expect(driftAgainstDesignCss(generated, fs.readFileSync(designCss, "utf8"))).toEqual([]);
  });

  it("keeps the themes separate: the title typeface, accent and button size differ", () => {
    const v = parseVars(generated);
    expect(v.dark["ff-title"]).toContain("Inter");
    expect(v.light["ff-title"]).toContain("Cormorant Garamond");
    expect(v.dark.accent).toBe("#5e6ad2");
    expect(v.light.accent).toBe("#cc785c");
    expect(v.dark["btn-h"]).toBe("34px");
    expect(v.light["btn-h"]).toBe("40px");
  });

  it("notices drift", () => {
    const drifted = generated.replace("#5e6ad2", "#000000");
    expect(driftAgainstDesignCss(drifted, fs.readFileSync(designCss, "utf8")).join("\n")).toMatch(/--accent/);
  });

  it("has a status colour for every run status the design lists", () => {
    const v = parseVars(generated);
    for (const t of ["dark", "light"] as const) for (const k of ["ok", "run", "wait", "fail", "sleep"]) expect(v[t][k]).toMatch(/^#[0-9a-f]{6}$/);
  });
});
