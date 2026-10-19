"""Graders and targets of the decisions and summaries suites (T-38)."""

from __future__ import annotations

from typing import Any

from evalgate.contracts import Case, TargetResult
from evals.graders import decisions as dg
from evals.graders import summaries as sg


def case(expected: dict[str, Any], meta: dict[str, Any] | None = None) -> Case:
    return Case(id="c", split="dev", input={}, expected=expected, meta=meta or {})


def result(output: dict[str, Any]) -> TargetResult:
    return TargetResult(output=output)


def test_reason_code_set_grader() -> None:
    c = case(
        {"action": "REVIEW", "reason_codes": ["MODEL_SCORE_ELEVATED", "P2P_DEVICE_CHURN"], "degraded": False}
    )
    assert (
        dg.reason_code_set(c, result({"reason_codes": ["P2P_DEVICE_CHURN", "MODEL_SCORE_ELEVATED"]})) == 1.0
    )
    assert dg.reason_code_set(c, result({"reason_codes": ["P2P_DEVICE_CHURN"]})) == 0.0  # missing one
    extra = ["MODEL_SCORE_ELEVATED", "P2P_DEVICE_CHURN", "VELOCITY_SPIKE"]
    assert dg.reason_code_set(c, result({"reason_codes": extra})) == 0.0  # an extra code is wrong too
    assert dg.reason_code_set(c, result({"error": "boom"})) == 0.0
    empty = case({"action": "APPROVE", "reason_codes": [], "degraded": False})
    assert dg.reason_code_set(empty, result({"reason_codes": []})) == 1.0


def test_degraded_handling_needs_the_flag_and_the_right_code() -> None:
    model_down = case({"action": "REVIEW", "reason_codes": ["MODEL_UNAVAILABLE"], "degraded": True})
    good = {"degraded": True, "reason_codes": ["MODEL_UNAVAILABLE"]}
    assert dg.degraded_handling(model_down, result(good)) == 1.0
    assert dg.degraded_handling(model_down, result({**good, "degraded": False})) == 0.0
    wrong_code = {"degraded": True, "reason_codes": ["FEATURES_UNAVAILABLE"]}
    assert dg.degraded_handling(model_down, result(wrong_code)) == 0.0
    healthy = case({"action": "APPROVE", "reason_codes": [], "degraded": False})
    assert dg.degraded_handling(healthy, result({"degraded": False, "reason_codes": []})) == 1.0
    assert (
        dg.degraded_handling(healthy, result({"degraded": True, "reason_codes": ["MODEL_UNAVAILABLE"]}))
        == 0.0
    )


def test_decisions_target_runs_the_real_scorer() -> None:
    row = {
        "transaction": {
            "transaction_id": "0192f000-0000-4000-8000-000000000001",
            "account_id": "acc_00000001",
            "amount_minor": 4000,
            "currency": "USD",
            "merchant_id": "m_00001",
            "merchant_name": "Shop",
            "mcc": "7995",
            "channel": "CARD_PRESENT",
            "country": "US",
            "ip_prefix": "23.1.2.0/24",
            "device_id": "a" * 64,
            "event_ts": "2026-09-21T10:00:00.000Z",
        },
        "features": None,
        "model": {"score": 0.1},
    }
    out = dg.target(row).output
    assert isinstance(out, dict)
    assert out["action"] == "DECLINE" and out["degraded"] is True
    assert out["reason_codes"] == ["BLOCKED_MCC", "FEATURES_UNAVAILABLE"]


def test_pii_grader_counts_planted_strings_that_reach_the_provider() -> None:
    c = case({"pattern": "unclear"}, meta={"pii": ["jamie.roe12@example.com"]})
    assert sg.pii_to_provider(c, result({"provider_prompt": "merchant [EMAIL]"})) == 0.0
    assert sg.pii_to_provider(c, result({"provider_prompt": "merchant jamie.roe12@example.com"})) == 1.0
    assert sg.pii_to_provider(case({"pattern": "unclear"}), result({"provider_prompt": "x"})) == 0.0


def test_schema_validity_is_the_model_output_passing_validation() -> None:
    c = case({"pattern": "unclear"})
    assert sg.schema_valid(c, result({"status": "ok"})) == 1.0
    assert sg.schema_valid(c, result({"status": "unfaithful"})) == 1.0
    assert sg.schema_valid(c, result({"status": "fallback"})) == 0.0
