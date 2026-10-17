import { afterEach, describe, expect, it, vi } from "vitest";

import { newIdempotencyKey } from "./queries";

afterEach(() => vi.unstubAllGlobals());

describe("newIdempotencyKey", () => {
  it("works without crypto.randomUUID (plain HTTP on a non-localhost host is not a secure context)", () => {
    vi.stubGlobal("crypto", { getRandomValues: crypto.getRandomValues.bind(crypto) });
    const keys = new Set(Array.from({ length: 100 }, newIdempotencyKey));
    expect(keys.size).toBe(100);
    for (const k of keys)
      expect(k).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  });
});
