import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { routes } from "../App";
import { CASE_ID, signedIn } from "../test/fixtures";
import { json, renderRoutes, stubApi } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const ruleSet = {
  version: "2026.10.2",
  sha256: "abcdef0123456789",
  feature_spec: "v2",
  thresholds: { review: 0.3, decline: 0.85 },
  rules: [
    {
      id: "R001",
      name: "New device after credential change",
      when: "new_device && minutes_since_email_change < 60",
      action: "ACTION_REVIEW",
      reason_code: "NEW_DEVICE_AFTER_CREDENTIAL_CHANGE",
      final: false,
    },
  ],
};

describe("rules", () => {
  it("shows the active rule set and Test rule for risk engineers only", async () => {
    signedIn("analyst");
    vi.stubGlobal("fetch", stubApi({ "GET /api/v1/rules/active": () => json(ruleSet) }).fetchMock);
    renderRoutes(routes, "/rules");
    expect(await screen.findByText("R001")).toBeInTheDocument();
    expect(screen.getByText(/feature spec v2/)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Test rule" })).toBeNull();
    // The table scrolls sideways on narrow screens, so its region must be reachable from the keyboard.
    expect(screen.getByRole("region", { name: "Active rules" })).toHaveAttribute("tabindex", "0");
  });

  it("tests an expression and reports compile errors", async () => {
    signedIn("risk_engineer");
    const api = stubApi({
      "GET /api/v1/rules/active": () => json(ruleSet),
      "POST /api/v1/rules:test": async (req) => {
        const body = (await req.json()) as { when: string };
        return body.when.includes("+")
          ? json({ valid: false, error: "unexpected token EOF" })
          : json({
              valid: true,
              evaluated: "1000",
              matched: "12",
              match_rate: 0.012,
              sample_decision_ids: ["0192f000-0000-7000-9000-0000000000aa"],
            });
      },
    });
    vi.stubGlobal("fetch", api.fetchMock);
    renderRoutes(routes, "/rules");
    const box = await screen.findByLabelText("Expression");
    await userEvent.type(box, "tx_count_10m >= 8");
    await userEvent.click(screen.getByRole("button", { name: /Run on 1,000/ }));
    expect(await screen.findByText(/Matched 12 of 1000 \(1.2%\)/)).toBeInTheDocument();
    await userEvent.clear(box);
    await userEvent.type(box, "amount_minor +");
    await userEvent.click(screen.getByRole("button", { name: /Run on 1,000/ }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Invalid expression: unexpected token EOF");
    expect(JSON.parse(api.calls.at(-1)!.body)).toEqual({ when: "amount_minor +", sample_size: 1000 });
  });
});

describe("model and shadow", () => {
  it("is limited to risk and ML engineers", async () => {
    signedIn("analyst");
    const api = stubApi({});
    vi.stubGlobal("fetch", api.fetchMock);
    renderRoutes(routes, "/shadow");
    expect(await screen.findByText("This screen is for risk and ML engineers.")).toBeInTheDocument();
    expect(api.calls).toHaveLength(0);
  });

  it("shows agreement, the band matrix and disagreements", async () => {
    signedIn("ml_engineer");
    vi.stubGlobal(
      "fetch",
      stubApi({
        "GET /api/v1/shadow/summary": () =>
          json({
            primary_version: "fraud-gbdt/3",
            shadow_version: "fraud-gbdt/4",
            total: "200",
            agreement_rate: 0.95,
            dropped: "0",
            band_matrix: [
              { primary_band: "APPROVE", shadow_band: "APPROVE", count: "180" },
              { primary_band: "APPROVE", shadow_band: "REVIEW", count: "10" },
              { primary_band: "REVIEW", shadow_band: "REVIEW", count: "10" },
            ],
            disagreements: [
              {
                decision_id: "0192f000-0000-7000-9000-0000000000aa",
                primary_band: "APPROVE",
                shadow_band: "REVIEW",
                primary_score: 0.2,
                shadow_score: 0.4,
              },
            ],
          }),
      }).fetchMock,
    );
    renderRoutes(routes, "/shadow");
    expect(await screen.findByText("95.0%")).toBeInTheDocument();
    const matrix = screen.getByRole("table", { name: /Band changes/ });
    const approveRow = within(matrix).getByRole("rowheader", { name: "APPROVE" }).closest("tr")!;
    expect(
      within(approveRow)
        .getAllByRole("cell")
        .map((c) => c.textContent),
    ).toEqual(["180", "10", "0"]);
    expect(screen.getByText("000000aa")).toBeInTheDocument(); // UUIDv7: the end is random, the start is time
  });
});

describe("account timeline", () => {
  it("lists items newest first with links to cases, and an empty state", async () => {
    signedIn("analyst");
    vi.stubGlobal(
      "fetch",
      stubApi({
        "GET /api/v1/accounts/acc_00000042/timeline": () =>
          json({
            account_id: "acc_00000042",
            items: [
              {
                kind: "decision",
                id: "d1",
                ts: "2026-09-20T10:03:00Z",
                title: "Decision REVIEW",
                details: { action: "REVIEW", case_id: CASE_ID },
              },
              {
                kind: "event.transaction",
                id: "t1",
                ts: "2026-09-20T10:02:30Z",
                title: "Transaction",
                details: { amount_minor: "2640", currency: "USD", merchant_id: "m_1" },
              },
              {
                kind: "event.login",
                id: "e1",
                ts: "2026-09-20T10:02:00Z",
                title: "Login",
                details: { method: "password" },
              },
            ],
          }),
        "GET /api/v1/accounts/acc_00000099/timeline": () => json({ account_id: "acc_00000099", items: [] }),
      }).fetchMock,
    );
    renderRoutes(routes, "/accounts/acc_00000042");
    expect(await screen.findByText("Decision REVIEW")).toBeInTheDocument();
    // Amounts are minor units on the wire: USD 26.40, never "2640 USD".
    expect(screen.getByText("USD 26.40")).toHaveClass("num");
    expect(screen.queryByText(/2640 USD/)).toBeNull();
    expect(screen.getByRole("link", { name: "Open case 000000aa" })).toHaveAttribute(
      "href",
      `/cases/${CASE_ID}`,
    );
    renderRoutes(routes, "/accounts/acc_00000099");
    expect(await screen.findByText("No events, decisions or labels for this account.")).toBeInTheDocument();
  });
});

describe("layout", () => {
  it("makes the skip link readable when it receives focus", async () => {
    signedIn("analyst");
    vi.stubGlobal(
      "fetch",
      stubApi({ "GET /api/v1/cases": () => json({ cases: [], total_open: 0 }) }).fetchMock,
    );
    renderRoutes(routes, "/queue");
    const skip = await screen.findByRole("link", { name: "Skip to content" });
    for (const c of ["focus:bg-panel-raised", "focus:text-text", "focus:z-20", "focus:rounded"])
      expect(skip).toHaveClass(c);
  });
});
