"""make eval-live and make record-fixtures, offline: a stub stands in for the provider (no key, no network)."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from evals import live
from fraud_summarizer.llm.cassettes import CassetteStore
from fraud_summarizer.llm.fake import FakeLLM
from fraud_summarizer.llm.types import ProviderRequest, ProviderResponse
from scripts import eval_live, record_fixtures


class StubProvider:
    """Answers like FakeLLM but reports the route's real model and token usage, as a provider would."""

    name = "anthropic"

    def __init__(self) -> None:
        self.calls = 0

    async def complete(self, req: ProviderRequest) -> ProviderResponse:
        self.calls += 1
        resp = await FakeLLM().complete(req)
        return ProviderResponse(
            text=resp.text, model=req.model, input_tokens=2_000, output_tokens=300, stop_reason="end_turn"
        )


@pytest.fixture
def stub(monkeypatch: pytest.MonkeyPatch) -> StubProvider:
    provider = StubProvider()
    monkeypatch.setenv("ANTHROPIC_API_KEY", "test-key-not-used")
    monkeypatch.setattr(live, "provider_for", lambda name: provider)
    return provider


def test_no_key_means_not_run(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("FRAUD_SUMMARY_PROVIDER", raising=False)
    assert eval_live.main(["--out", str(tmp_path / "r.json")]) == 1
    assert not (tmp_path / "r.json").exists()


def test_eval_live_writes_a_live_report_within_budget(stub: StubProvider, tmp_path: Path) -> None:
    out = tmp_path / "summaries_live.json"
    code = eval_live.main(
        ["--out", str(out), "--budget", "1.0", "--limit", "6", "--calibration-out", str(tmp_path / "c.json")]
    )
    report = json.loads(out.read_text())
    assert report["mode"] == "live" and report["models"] == ["claude-haiku-5-5"]
    assert report["n_cases"] == 6 and report["cost_usd_total"] > 0
    assert {"pattern_accuracy", "pii_to_provider", "cost_per_case", "judge_usefulness"} <= set(
        report["metrics"]
    )
    # The judge ran on every live summary (a summary FakeLLM writes is mostly judged useful).
    assert report["metrics"]["judge_usefulness"]["value"] > 0.5
    calibration = json.loads((tmp_path / "c.json").read_text())
    assert calibration["mode"] == "live" and calibration["n"] == 40
    assert report["extensions"]["judge_calibrated"] == calibration["passed"]
    assert code in (0, 1)


def test_the_budget_stops_calls(stub: StubProvider, tmp_path: Path) -> None:
    budget = live.Budget(limit_usd=0.0005)  # one call costs about 0.00035 at the configured price
    gateway = live.live_gateway({"FRAUD_SUMMARY_PROVIDER": "anthropic"}, budget)
    rows = [json.loads(line) for line in Path("evals/datasets/summaries/dev.jsonl").read_text().splitlines()][
        :5
    ]
    results = live.summarize_cases(rows, gateway)
    assert (
        stub.calls == 2 and budget.spent_usd >= budget.limit_usd
    )  # the call that crossed the limit was the last
    assert sum(1 for r in results if r.error and "budget" in r.error) == 3


def test_record_fixtures_writes_cassettes_fakellm_replays(stub: StubProvider, tmp_path: Path) -> None:
    assert record_fixtures.main(["--out", str(tmp_path), "--budget", "1.0", "--limit", "2"]) == 0
    files = sorted((tmp_path / "case_summary").glob("*.json"))
    assert len(files) == 2 and json.loads(files[0].read_text())["recorded"] is True
    req_doc = json.loads(files[0].read_text())
    assert req_doc["model"] == "claude-haiku-5-5"
    assert CassetteStore(tmp_path).root == tmp_path
