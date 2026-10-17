import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { routes } from "../App";
import { getSession } from "../auth/session";
import { json, renderRoutes } from "../test/render";
import { safeNext } from "./signin";

const personas = {
  personas: [
    { id: "analyst-1", name: "Analyst One", role: "analyst" },
    { id: "risk-1", name: "Risk Engineer One", role: "risk_engineer" },
  ],
};

afterEach(() => vi.unstubAllGlobals());

describe("sign-in", () => {
  it("redirects anonymous users to sign-in, then back to where they were going", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) =>
        url === "/auth/personas"
          ? json(personas)
          : json({ access_token: "t", expires_in: 3600, persona: personas.personas[0] }),
      ),
    );
    const { router } = renderRoutes(routes, "/rules");
    await waitFor(() => expect(router.state.location.pathname).toBe("/sign-in"));
    expect(router.state.location.search).toBe("?next=%2Frules");
    await userEvent.click(await screen.findByRole("button", { name: /Analyst One/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/rules"));
    expect(getSession()?.persona.id).toBe("analyst-1");
    expect(screen.getByRole("navigation", { name: "Main" })).toBeInTheDocument();
    // Analysts do not see the shadow screen in the navigation.
    expect(screen.queryByRole("link", { name: /shadow/i })).toBeNull();
  });

  it("shows an error with retry, then the empty state", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response("down", { status: 503 }))
      .mockResolvedValueOnce(json({ personas: [] }));
    vi.stubGlobal("fetch", fetchMock);
    renderRoutes(routes, "/sign-in");
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load personas");
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("The dev-issuer has no personas.")).toBeInTheDocument();
  });

  it("only redirects to same-origin paths", () => {
    expect(safeNext("/cases/1")).toBe("/cases/1");
    expect(safeNext("//evil.example")).toBe("/queue");
    expect(safeNext("https://evil.example")).toBe("/queue");
    expect(safeNext("/sign-in")).toBe("/queue");
    expect(safeNext(null)).toBe("/queue");
  });
});
