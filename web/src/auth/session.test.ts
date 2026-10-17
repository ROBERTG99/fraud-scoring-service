import { describe, expect, it, vi } from "vitest";

import { can, clearSession, fetchPersonas, getSession, setSession, signIn } from "./session";

describe("session", () => {
  it("signs in with a persona and stores the token", async () => {
    const fetchMock = vi.fn(async () =>
      Response.json({
        access_token: "t",
        expires_in: 3600,
        persona: { id: "risk-1", name: "Risk Engineer One", role: "risk_engineer" },
      }),
    );
    const s = await signIn("risk-1", fetchMock as unknown as typeof fetch, 1_000);
    expect(s.expiresAt).toBe(1_000 + 3_600_000);
    expect(getSession(2_000)?.persona.role).toBe("risk_engineer");
    expect(can("testRule", s)).toBe(true);
    expect(can("label", s)).toBe(false);
    expect(can("shadow", s)).toBe(true);
  });

  it("expires sessions and handles failures", async () => {
    setSession({ token: "t", persona: { id: "a", name: "A", role: "analyst" }, expiresAt: 10 });
    expect(getSession(11)).toBeNull();
    clearSession();
    expect(can("label", null)).toBe(false);
    const bad = vi.fn(async () => new Response("no", { status: 503 }));
    await expect(signIn("x", bad as unknown as typeof fetch)).rejects.toThrow("sign-in failed: 503");
    await expect(fetchPersonas(bad as unknown as typeof fetch)).rejects.toThrow("dev-issuer: 503");
    const ok = vi.fn(async () =>
      Response.json({ personas: [{ id: "ml-1", name: "ML", role: "ml_engineer" }] }),
    );
    expect((await fetchPersonas(ok as unknown as typeof fetch))[0]?.id).toBe("ml-1");
  });
});
