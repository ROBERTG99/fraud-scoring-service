import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { routes } from "../App";
import { CASE_ID, sampleCase, signedIn } from "../test/fixtures";
import { json, renderRoutes, stubApi } from "../test/render";

let scrolled: Element[] = [];
let scrollOptions: unknown;
beforeEach(() => {
  scrolled = [];
  // jsdom has no layout, so scrollIntoView is not implemented.
  Element.prototype.scrollIntoView = function (this: Element, o?: boolean | ScrollIntoViewOptions) {
    scrolled.push(this);
    scrollOptions = o;
  };
});
afterEach(() => vi.unstubAllGlobals());

const LABEL = { label: "LABEL_FRAUD", analyst_sub: "analyst-1", created_at: "2026-09-20T10:05:00Z" } as const;

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
    const row = container.querySelector<HTMLElement>('[data-fact="feature.new_device"]')!;
    expect(row).toHaveAttribute("aria-current", "true");
    // The cited fact may be off-screen (stacked layout): bring it into view and move focus to it.
    expect(scrolled).toContain(row);
    expect(scrollOptions).toEqual({ block: "center" });
    expect(row).toHaveFocus();
    expect(row).toHaveAttribute("tabindex", "-1");
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

  it("polls the case after a regenerate until the new summary arrives", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      signedIn("analyst");
      const old = sampleCase();
      let gets = 0;
      let regenerated = false;
      vi.stubGlobal(
        "fetch",
        stubApi({
          "GET /api/v1/cases/{id}": () => {
            gets++;
            // The worker finishes on the second refetch after the request.
            return json(
              regenerated && gets >= 4
                ? sampleCase({
                    summary: { ...old.summary!, pattern: "card_testing", created_at: "2026-09-20T11:00:00Z" },
                  })
                : old,
            );
          },
          "POST /summarizer/v1/summaries/{id}:regenerate": () => {
            regenerated = true;
            return json({ case_id: CASE_ID, status: "accepted", run_id: "r1", duplicate: false }, 202);
          },
        }).fetchMock,
      );
      renderRoutes(routes, `/cases/${CASE_ID}`);
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      await user.click(await screen.findByRole("button", { name: "Regenerate" }));
      expect(await screen.findByRole("status")).toHaveTextContent(/Regeneration requested/);
      await vi.advanceTimersByTimeAsync(3_000);
      await vi.advanceTimersByTimeAsync(3_000);
      expect(await screen.findByText("card_testing")).toBeInTheDocument();
      const settled = gets;
      await vi.advanceTimersByTimeAsync(10_000);
      expect(gets).toBe(settled); // polling stops once the summary changed
    } finally {
      vi.useRealTimers();
    }
  });

  it("returns focus to Label case after Cancel and to the label text after saving", async () => {
    signedIn("analyst");
    let labelled = false;
    vi.stubGlobal(
      "fetch",
      stubApi({
        "GET /api/v1/cases/{id}": () =>
          json(labelled ? sampleCase({ status: "CASE_STATUS_LABELLED", label: LABEL }) : sampleCase()),
        "POST /api/v1/cases/{id}:label": () => {
          labelled = true;
          return json(sampleCase({ status: "CASE_STATUS_LABELLED", label: LABEL }));
        },
      }).fetchMock,
    );
    renderRoutes(routes, `/cases/${CASE_ID}`);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Label case" }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("button", { name: "Label case" })).toHaveFocus();

    await user.click(screen.getByRole("button", { name: "Label case" }));
    await user.click(screen.getByRole("radio", { name: "Fraud" }));
    await user.click(screen.getByRole("button", { name: "Confirm label" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(screen.getByText(/Labelled/).closest("[tabindex]")).toHaveFocus());
    expect(document.activeElement).not.toBe(document.body);
  });
});
