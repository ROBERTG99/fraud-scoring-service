// @vitest-environment node
import { describe, expect, it } from "vitest";

import config from "../vite.config";

describe("production build", () => {
  // The CSP is font-src 'self' (cmd/webserve): a font inlined as a data: URI would be blocked.
  it("never inlines fonts as data: URIs", () => {
    const limit = config.build?.assetsInlineLimit;
    expect(typeof limit).toBe("function");
    const inline = limit as (file: string, content: Buffer) => boolean | undefined;
    for (const f of ["jetbrains-mono-latin-400-normal.woff2", "inter-latin-600-normal.woff", "x.ttf"])
      expect(inline(f, Buffer.alloc(100))).toBe(false);
    expect(inline("logo.svg", Buffer.alloc(100))).toBeUndefined();
  });
});
