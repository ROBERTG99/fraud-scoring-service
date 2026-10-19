import { describe, expect, it, vi } from "vitest";

import { getSession, setSession } from "../auth/session";
import { ApiError, createApi, signInPath, toApiError, unwrap } from "./client";

function session() {
  setSession({
    token: "tok-1",
    persona: { id: "analyst-1", name: "Analyst One", role: "analyst" },
    expiresAt: Date.now() + 60_000,
  });
}

describe("api client", () => {
  it("401 clears the session and redirects to sign-in", async () => {
    session();
    const onUnauthorized = vi.fn();
    const fetchMock = vi.fn(
      async () => new Response(JSON.stringify({ code: 16, message: "invalid token" }), { status: 401 }),
    );
    const api = createApi({ baseUrl: "http://test", fetch: fetchMock, onUnauthorized });
    const r = await api.GET("/v1/cases");
    expect(r.response.status).toBe(401);
    expect(onUnauthorized).toHaveBeenCalledOnce();
    expect(getSession()).toBeNull();
    expect(() => unwrap(r)).toThrow(ApiError);
  });

  it("sends the bearer token and returns typed data", async () => {
    session();
    let auth = "";
    const fetchMock = vi.fn(async (req: Request) => {
      auth = req.headers.get("Authorization") ?? "";
      return new Response(JSON.stringify({ cases: [], total_open: 0 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    });
    const api = createApi({
      baseUrl: "http://test",
      fetch: fetchMock as unknown as typeof fetch,
      onUnauthorized: vi.fn(),
    });
    const data = unwrap(await api.GET("/v1/cases", { params: { query: { page_size: 10 } } }));
    expect(auth).toBe("Bearer tok-1");
    expect(data.total_open).toBe(0);
    expect((fetchMock.mock.calls[0]![0] as Request).url).toBe("http://test/v1/cases?page_size=10");
  });

  it("maps field violations from google.rpc.Status", () => {
    const err = toApiError(400, {
      message: "invalid request",
      details: [{ field_violations: [{ field: "note", description: "at most 2000 characters" }] }],
    });
    expect(err.fields).toEqual([{ field: "note", description: "at most 2000 characters" }]);
    expect(toApiError(503, undefined).message).toBe("request failed (503)");
  });

  it("builds the sign-in path with the current location as next", () => {
    expect(signInPath({ pathname: "/cases/abc", search: "?x=1" })).toBe(
      "/sign-in?next=%2Fcases%2Fabc%3Fx%3D1",
    );
    expect(signInPath({ pathname: "/sign-in", search: "?next=%2F" })).toBe("/sign-in");
  });
});
