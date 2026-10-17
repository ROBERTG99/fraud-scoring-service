import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { routes } from "../App";
import { CASE_ID, sampleCase, signedIn } from "../test/fixtures";
import { json, renderRoutes, stubApi } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("case detail", () => {
  it("separates the AI summary and highlights the fact an evidence item cites", async () => {
    signedIn("analyst");
    vi.stubGlobal("fetch", stubApi({ "GET /api/v1/cases/{id}": () => json(sampleCase()) }).fetchMock);
    const { container } = renderRoutes(routes, `/cases/${CASE_ID}`);
    expect(
      await screen.findByRole("heading", { name: "AI summary · does not affect decisions" }),
    ).toBeInTheDocument();
    expect(screen.getByText("fake-case-summary-v1")).toBeInTheDocument();
    expect(screen.getByText("case_summary.v1")).toBeInTheDocument();
    expect(screen.getByText("100%")).toBeInTheDocument(); // faithfulness
    await userEvent.click(screen.getByRole("button", { name: /new device/ }));
    const row = container.querySelector('[data-fact="feature.new_device"]');
    expect(row).toHaveAttribute("aria-current", "true");
    expect(screen.getByRole("button", { name: /new device/ })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "Label case" })).toBeInTheDocument();
    expect(screen.getByText("NEW_DEVICE_AFTER_CREDENTIAL_CHANGE")).toBeInTheDocument();
  });

  it("hides labelling from other roles and explains fallback summaries", async () => {
    signedIn("risk_engineer");
    vi.stubGlobal(
      "fetch",
      stubApi({
        "GET /api/v1/cases/{id}": () => json(sampleCase({ summary_status: "fallback", summary: undefined })),
      }).fetchMock,
    );
    renderRoutes(routes, `/cases/${CASE_ID}`);
    expect(await screen.findByText(/AI summary unavailable/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Label case" })).toBeNull();
    expect(screen.getByRole("button", { name: "Regenerate" })).toBeInTheDocument();
  });

  it("regenerates with an idempotency key and shows errors with retry", async () => {
    signedIn("analyst");
    let n = 0;
    const api = stubApi({
      "GET /api/v1/cases/{id}": () =>
        n++ === 0 ? json({ code: 14, message: "upstream unavailable" }, 502) : json(sampleCase()),
      "POST /summarizer/v1/summaries/{id}:regenerate": () =>
        json({ case_id: CASE_ID, status: "accepted", run_id: "r1", duplicate: false }, 202),
    });
    vi.stubGlobal("fetch", api.fetchMock);
    renderRoutes(routes, `/cases/${CASE_ID}`);
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load case (502)");
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    await userEvent.click(await screen.findByRole("button", { name: "Regenerate" }));
    const call = api.calls.find((c) => c.method === "POST")!;
    expect(call.path).toBe(`/summarizer/v1/summaries/${CASE_ID}:regenerate`);
    expect(call.headers.get("Idempotency-Key")).toMatch(/^[A-Za-z0-9_-]{8,128}$/);
    expect(call.headers.get("Authorization")).toBe("Bearer tok-analyst-1");
  });
});
