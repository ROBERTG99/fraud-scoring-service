"""FakeLLM: a deterministic provider for tests, offline evals and DEMO_MODE=offline.

Recorded cassettes for the route's real model win when present; otherwise rules map reason codes and
features to a pattern, and evidence cites the three most extreme features with their exact values.
It reads only fact keys and numeric feature values, so text inside facts (merchant names) cannot steer it.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any

from fraud_summarizer import facts as factfmt
from fraud_summarizer.llm.cassettes import CassetteStore
from fraud_summarizer.llm.types import ProviderRequest, ProviderResponse

# (neutral value, scale, direction): extremeness = direction * (value - neutral) / scale
_EXTREMENESS: dict[str, tuple[float, float, int]] = {
    "tx_count_10m": (0, 3, 1),
    "tx_count_1h": (1, 6, 1),
    "tx_count_24h": (5, 12, 1),
    "amount_sum_1h_minor": (0, 50000, 1),
    "amount_minor": (2500, 40000, 1),
    "distinct_devices_24h": (1, 1.5, 1),
    "new_device": (0, 0.8, 1),
    "minutes_since_pwd_change": (43200, 14400, -1),
    "minutes_since_email_change": (43200, 14400, -1),
    "mcc_novel_30d": (0, 2, 1),
    "amount_ratio_24h": (1, 4, 1),
}

_STATEMENTS = {
    "tx_count_10m": "{v} earlier transactions in the ten-minute window before this one.",
    "tx_count_1h": "{v} earlier transactions in the last hour.",
    "tx_count_24h": "{v} earlier transactions in the last day.",
    "amount_sum_1h_minor": "Spend in the last hour totals {v} minor units.",
    "amount_minor": "The transaction amount is {v} minor units.",
    "distinct_devices_24h": "{v} distinct devices were used on the account in the last day.",
    "new_device": "The device was not seen on the account before (new_device = {v}).",
    "minutes_since_pwd_change": "The password was changed {v} minutes before the transaction.",
    "minutes_since_email_change": "The email address was changed {v} minutes before the transaction.",
    "mcc_novel_30d": "The merchant category is new for this account in the last month (mcc_novel_30d = {v}).",
    "amount_ratio_24h": "The amount is {v} times the account's recent average.",
}

_CHECKS = {
    "account_takeover": [
        "Confirm the recent credential change with the customer through a known contact channel.",
        "Review logins from the new device and its IP prefix.",
        "Hold further high-value online purchases until the customer confirms.",
    ],
    "card_testing": [
        "Check the merchants in the burst for other cards tested the same way.",
        "Block the card if the customer does not recognise the small transactions.",
    ],
    "mule_account": [
        "Review incoming funds and P2P counterparties for the last day.",
        "Verify the identity behind the newly bound devices.",
    ],
    "friendly_fraud": [
        "Compare with the customer's purchase history at this merchant.",
        "Request delivery or usage evidence from the merchant.",
    ],
    "unclear": ["Review the account timeline manually before labelling."],
}


def _num(facts: dict[str, str], key: str, default: float = 0.0) -> float:
    try:
        return float(facts.get(key, default))
    except ValueError:
        return default


def _events(facts: dict[str, str], kind: str, field: str) -> list[str]:
    return [v for k, v in facts.items() if k.startswith(f"event.{kind}.") and k.endswith(f".{field}")]


def classify(facts: dict[str, str]) -> str:
    """Pattern from the facts, following the distinctions of cmd/gen/PATTERNS.md (tuned on dev splits only)."""
    codes = set(filter(None, facts.get("decision.reason_codes", "").split(",")))
    f = {k.removeprefix("feature."): _num(facts, k) for k in facts if k.startswith("feature.")}
    new_device = f.get("new_device") == 1
    amount = f.get("amount_minor", 0)
    ratio = f.get("amount_ratio_24h", 0)
    # A phone the customer bound (device_bound) differs from a device that only logged in.
    bound = "device_bound" in _events(facts, "device", "change")
    tx_device = facts.get("event.transaction.0.device")
    login_from_tx_device = tx_device is not None and tx_device in _events(facts, "login", "device")
    recent_change = (
        min(f.get("minutes_since_pwd_change", 43200), f.get("minutes_since_email_change", 43200)) < 1440
    )
    # Card testing comes from a device new to the account: a burst, or a run of small tests within the hour.
    if new_device and (f.get("tx_count_10m", 0) >= 5 or (f.get("tx_count_1h", 0) >= 4 and amount <= 1000)):
        return "card_testing"
    # Many small purchases on the home device are in-app spending, not testing.
    if f.get("tx_count_10m", 0) >= 5:
        return "unclear"
    # Mules: P2P with device churn, or from a freshly bound device at mule-size amounts.
    if f.get("channel") == 3 and (
        "P2P_DEVICE_CHURN" in codes or f.get("distinct_devices_24h", 0) >= 3 or (bound and amount >= 30000)
    ):
        return "mule_account"
    takeover_codes = codes & {"NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", "LARGE_AMOUNT_NEW_DEVICE"}
    if takeover_codes or (new_device and recent_change):
        # A reset followed by binding a new phone and an ordinary purchase is the legitimate twin.
        if bound and ratio < 3 and "LARGE_AMOUNT_NEW_DEVICE" not in codes:
            return "unclear"
        return "account_takeover"
    # Quiet takeover: a device that logged in but was never bound by the customer, then buys online.
    if new_device and not bound and login_from_tx_device and f.get("channel") == 2:
        return "account_takeover"
    if f.get("channel") == 2 and not new_device and not recent_change and ratio >= 1.5:
        return "friendly_fraud"
    return "unclear"


def _fmt(value: str) -> str:
    try:
        x = float(value)
    except ValueError:
        return value
    return str(int(x)) if x.is_integer() else value


def summarize(facts: dict[str, str]) -> dict[str, object]:
    pattern = classify(facts)
    scored: list[tuple[float, str]] = []
    for name, (neutral, scale, direction) in _EXTREMENESS.items():
        key = f"feature.{name}"
        if key in facts:
            scored.append((direction * (_num(facts, key) - neutral) / scale, key))
    scored.sort(key=lambda t: (-t[0], t[1]))
    evidence = [
        {"fact_key": key, "statement": _STATEMENTS[key.removeprefix("feature.")].format(v=_fmt(facts[key]))}
        for _, key in scored[:3]
    ]
    if not evidence:
        evidence = [{"fact_key": "decision.action", "statement": "The decision was REVIEW."}]
    score = _num(facts, "decision.score", -1)
    confidence = round(min(0.95, 0.5 + score / 2), 2) if score >= 0 else 0.5
    if pattern == "unclear":
        confidence = min(confidence, 0.5)
    return {
        "pattern": pattern,
        "evidence": evidence,
        "recommended_checks": _CHECKS[pattern],
        "confidence": confidence,
    }


def fake_judge(facts: dict[str, str], summary: dict[str, Any]) -> dict[str, object]:
    """Offline stand-in for the usefulness judge: the three tests of judge_summary_usefulness.v1, by rule.

    It reads only fact keys, values and the summary's fields, so instructions inside text cannot steer it.
    """
    from fraud_summarizer.faithfulness import check

    evidence = [
        (str(e.get("fact_key", "")), str(e.get("statement", ""))) for e in summary.get("evidence", [])
    ]
    if not evidence or check(evidence, facts).score < 1.0:
        return {
            "useful": False,
            "rationale": "An evidence item cites an absent fact or a number that differs.",
        }
    if summary.get("pattern") != classify(facts):
        return {"useful": False, "rationale": "The pattern is not the one the facts support."}
    if not any(len(str(c).split()) >= 6 for c in summary.get("recommended_checks", [])):
        return {"useful": False, "rationale": "No recommended check is a concrete next step."}
    return {"useful": True, "rationale": "Faithful evidence, a supported pattern and a concrete check."}


@dataclass
class FakeLLM:
    """Deterministic provider. `cassette_model` is the route's real model whose recordings win."""

    cassettes: CassetteStore | None = None
    cassette_model: str | None = None
    name: str = "fake"

    async def complete(self, req: ProviderRequest) -> ProviderResponse:
        if self.cassettes is not None:
            hit = self.cassettes.get(req, self.cassette_model)
            if hit is not None:
                return hit
        if req.route == "judge_usefulness":
            from fraud_summarizer.judge import parse_message

            body: dict[str, object] = fake_judge(*parse_message(req.user))
        else:
            body = summarize(factfmt.parse(req.user))
        text = json.dumps(body, sort_keys=True)
        return ProviderResponse(
            text=text,
            model=req.model,
            input_tokens=(len(req.system) + len(req.user)) // 4,
            output_tokens=len(text) // 4,
            stop_reason="end_turn",
        )
