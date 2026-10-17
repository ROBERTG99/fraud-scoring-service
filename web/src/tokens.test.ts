import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

// WCAG 2.2 contrast for the token pairs the console actually uses as text on a surface.
function luminance(hex: string): number {
  const c = hex
    .replace("#", "")
    .match(/../g)!
    .map((h) => parseInt(h, 16) / 255);
  const [r, g, b] = c.map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4)) as [
    number,
    number,
    number,
  ];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}
function contrast(a: string, b: string): number {
  const [x, y] = [luminance(a), luminance(b)].sort((p, q) => q - p) as [number, number];
  return (x + 0.05) / (y + 0.05);
}

const css = readFileSync(resolve(__dirname, "tokens.css"), "utf8");
function theme(selector: string): Record<string, string> {
  const block = css.slice(css.indexOf(selector + " {"));
  const body = block.slice(0, block.indexOf("}"));
  return Object.fromEntries([...body.matchAll(/--([a-z-]+):\s*(#[0-9a-f]{6})/g)].map((m) => [m[1]!, m[2]!]));
}

describe.each([
  ["dark", theme(":root")],
  ["light", theme(':root[data-theme="light"]')],
])("%s theme", (_name, t) => {
  it("has the spec palette tokens", () => {
    for (const k of ["bg", "panel", "text", "accent", "decline", "review", "approve"])
      expect(t[k]).toBeDefined();
  });
  it.each(["text", "text-muted", "accent", "decline-text", "review-text", "approve-text"])(
    "%s meets 4.5:1 on bg and panels",
    (fg) => {
      for (const bg of ["bg", "panel", "panel-raised"]) {
        expect(contrast(t[fg]!, t[bg]!)).toBeGreaterThanOrEqual(4.5);
      }
    },
  );
  it.each(["decline", "review", "approve"])("%s border/fill meets 3:1 (WCAG 1.4.11)", (c) => {
    for (const bg of ["bg", "panel", "panel-raised"]) {
      expect(contrast(t[c]!, t[bg]!)).toBeGreaterThanOrEqual(3);
    }
  });
  it("chart series meets 3:1 on every surface", () => {
    for (const bg of ["bg", "panel", "panel-raised"])
      expect(contrast(t["chart-series"]!, t[bg]!)).toBeGreaterThanOrEqual(3);
  });
  it("button text meets 4.5:1 on the accent", () => {
    expect(contrast(t["on-accent"]!, t["accent"]!)).toBeGreaterThanOrEqual(4.5);
  });
});

it("dark theme uses the spec colours", () => {
  const t = theme(":root");
  expect([t.bg, t.panel, t.text, t.accent, t.decline, t.review, t.approve]).toEqual([
    "#0b1220",
    "#111a2e",
    "#e6edf7",
    "#22d3ee",
    "#f43f5e",
    "#f59e0b",
    "#10b981",
  ]);
});
