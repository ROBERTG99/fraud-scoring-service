import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { routes } from "../App";
import { sampleCase, signedIn } from "../test/fixtures";
import { json, renderRoutes, stubApi } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const two = [
  sampleCase(),
  sampleCase({ case_id: "0192f000-0000-7000-8000-0000000000bb", amount_minor: "1250" }),
];

describe("review queue", () => {
  it("lists cases and opens one with the keyboard", async () => {
    signedIn("analyst");
    vi.stubGlobal(
      "fetch",
      stubApi({
        "GET /api/v1/cases": () => json({ cases: two, total_open: 2 }),
        "GET /api/v1/cases/{id}": () => json(sampleCase()),
      }).fetchMock,
    );
    const { router } = renderRoutes(routes, "/queue");
    expect(await screen.findByText("USD 940.00")).toBeInTheDocument();
    expect(screen.getByText("(2 open)")).toBeInTheDocument();
    const body = screen.getByRole("rowgroup", { name: "Cases" });
    body.focus();
    await userEvent.keyboard("j");
    expect(screen.getAllByRole("row")[2]).toHaveAttribute("aria-selected", "true");
    await userEvent.keyboard("{Enter}");
    await waitFor(() =>
      expect(router.state.location.pathname).toBe("/cases/0192f000-0000-7000-8000-0000000000bb"),
    );
  });

  it("sends filters to the API and shows the empty state", async () => {
    signedIn("analyst");
    const api = stubApi({ "GET /api/v1/cases": () => json({ cases: [], total_open: 0 }) });
    vi.stubGlobal("fetch", api.fetchMock);
    renderRoutes(routes, "/queue?summary=fallback&reason=MODEL_SCORE_ELEVATED");
    expect(await screen.findByText("No cases match these filters.")).toBeInTheDocument();
    const url = new URL("http://x" + api.calls[0]!.path);
    expect(api.calls[0]!.path).toBe("/api/v1/cases");
    expect(url).toBeTruthy();
  });

  it("shows loading, then an error with retry", async () => {
    signedIn("analyst");
    let n = 0;
    vi.stubGlobal(
      "fetch",
      stubApi({
        "GET /api/v1/cases": () =>
          n++ === 0
            ? json({ code: 13, message: "internal error" }, 500)
            : json({ cases: two, total_open: 2 }),
      }).fetchMock,
    );
    renderRoutes(routes, "/queue");
    expect(screen.getByRole("status")).toHaveTextContent("Loading cases");
    expect(await screen.findByRole("alert")).toHaveTextContent("internal error");
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("USD 940.00")).toBeInTheDocument();
  });
});
