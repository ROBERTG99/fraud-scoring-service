"""Prometheus metrics for the summarizer (spec §4.7, §14)."""

from __future__ import annotations

from prometheus_client import Counter

SUMMARY_STATUS = Counter(
    "fraud_summary_status_total", "Case summaries written, by status (ok, fallback, unfaithful).", ["status"]
)
LLM_COST = Counter(
    "fraud_llm_cost_usd", "LLM spend in USD at list price, by route and model (spec §14).", ["route", "model"]
)
LLM_CALLS = Counter(
    "fraud_llm_calls",
    "Provider calls, by route, model and outcome (ok, error).",
    ["route", "model", "outcome"],
)
MESSAGES = Counter(
    "fraud_summarizer_messages_total",
    "decisions messages handled, by outcome (summarized, skipped, duplicate, missing_case, invalid, retry).",
    ["outcome"],
)
