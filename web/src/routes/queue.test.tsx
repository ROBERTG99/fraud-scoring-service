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
    // Roving tabindex: one Tab stop for the whole list, on the current row's case link.
    const links = screen.getAllByRole("link", { name: /^Open case / });
    expect(links.map((l) => l.getAttribute("tabindex"))).toEqual(["0", "-1"]);
    links[0]!.focus();
    await userEvent.keyboard("j");
    expect(links[1]).toHaveFocus();
    expect(links.map((l) => l.getAttribute("tabindex"))).toEqual(["-1", "0"]);
    // The current row is marked with an accent outline (≥ 3:1), not only a faint background.
    expect(links[1]!.closest("tr")).toHaveClass("outline-accent");
    expect(links[0]!.closest("tr")).not.toHaveClass("outline-accent");
    await userEvent.keyboard("k");
    expect(links[0]).toHaveFocus();
    await userEvent.keyboard("{ArrowDown}{Enter}");
    await waitFor(() =>
      expect(router.state.location.pathname).toBe("/cases/0192f000-0000-7000-8000-0000000000bb"),
    );
  });

  it("keeps the cursor on the same case when a refetch reorders the list", async () => {
    signedIn("analyst");
    const newer = sampleCase({ case_id: "0192f000-0000-7000-8000-0000000000cc", amount_minor: "5000" });
    let list = two;
    vi.stubGlobal(
      "fetch",
      stubApi({ "GET /api/v1/cases": () => json({ cases: list, total_open: list.length }) }).fetchMock,
    );
    const { qc } = renderRoutes(routes, "/queue");
    await screen.findByText("USD 940.00");
    screen.getByRole("link", { name: "Open case 000000aa" }).focus();
    await userEvent.keyboard("j");
    const bb = screen.getByRole("link", { name: "Open case 000000bb" });
    expect(bb).toHaveFocus();
    // A higher-priority case arrives at the top: rows shift, the cursor must stay on 000000bb.
    list = [newer, ...two];
    await qc.refetchQueries({ queryKey: ["cases"] });
    await screen.findByText("USD 50.00");
    expect(bb).toHaveFocus();
    expect(bb).toHaveAttribute("tabindex", "0");
    expect(bb.closest("tr")).toHaveClass("outline-accent");
    expect(screen.getByRole("link", { name: "Open case 000000cc" })).toHaveAttribute("tabindex", "-1");
    await userEvent.keyboard("k");
    expect(screen.getByRole("link", { name: "Open case 000000aa" })).toHaveFocus();
  });

  it("shows the distinct end of UUIDv7 ids, not their shared timestamp prefix", async () => {
    signedIn("analyst");
    vi.stubGlobal(
      "fetch",
      stubApi({ "GET /api/v1/cases": () => json({ cases: two, total_open: 2 }) }).fetchMock,
    );
    renderRoutes(routes, "/queue");
    expect(await screen.findByText("000000aa")).toBeInTheDocument();
    expect(screen.getByText("000000bb")).toBeInTheDocument();
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
