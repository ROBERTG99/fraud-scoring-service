import type { Case } from "../api/queries";
import { setSession, type Role } from "../auth/session";

export const CASE_ID = "0192f000-0000-7000-8000-0000000000aa";

export function signedIn(
  role: Role,
  id = role === "analyst" ? "analyst-1" : role === "risk_engineer" ? "risk-1" : "ml-1",
) {
  setSession({ token: `tok-${id}`, persona: { id, name: id, role }, expiresAt: Date.now() + 3_600_000 });
}

export function sampleCase(over: Partial<Case> = {}): Case {
  return {
    case_id: CASE_ID,
    account_id: "acc_00000042",
    amount_minor: "94000",
    currency: "USD",
    status: "CASE_STATUS_OPEN",
    priority: 0.62,
    summary_status: "ok",
    created_at: new Date(Date.now() - 120_000).toISOString(),
    decision: {
      decision_id: "0192f000-0000-7000-9000-0000000000aa",
      action: "ACTION_REVIEW",
      score: 0.62,
      reason_codes: ["NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", "MODEL_SCORE_ELEVATED"],
      model_version: "fraud-gbdt/3",
      rules_version: "2026.10.2",
      degraded: false,
      decided_at: "2026-09-20T10:03:00Z",
    },
    facts: [
      { key: "decision.score", value: "0.62" },
      { key: "feature.minutes_since_email_change", value: "2" },
      { key: "feature.new_device", value: "1" },
      { key: "feature.amount_minor", value: "94000" },
    ],
    summary: {
      pattern: "account_takeover",
      model: "fake-case-summary-v1",
      prompt_version: "case_summary.v1",
      faithfulness: 1,
      confidence: 0.8,
      status: "ok",
      evidence: [
        { fact_key: "feature.new_device", statement: "The purchase came from a new device.", verified: true },
        {
          fact_key: "feature.minutes_since_email_change",
          statement: "The email changed 2 minutes earlier.",
          verified: true,
        },
      ],
      recommended_checks: ["Call the customer on the phone number on file."],
    },
    ...over,
  };
}
