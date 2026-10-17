"""`make record-fixtures`: record provider responses as cassettes for offline replay (needs a provider key).

Summarizes the summaries cases and judges the judge calibration items with the configured provider, within
LIVE_EVAL_BUDGET_USD, and stores each response under `tests/fixtures/llm/<route>/<key>.json` with
`recorded: true`. FakeLLM replays a cassette when the same route, model, prompt version and prompt recur, so
`make eval-summary` then runs in cassette mode against real model outputs.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT), str(ROOT / "src")]

from evals import live  # noqa: E402
from fraud_summarizer.judge import judge  # noqa: E402
from fraud_summarizer.llm.cassettes import CassetteStore  # noqa: E402


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Record LLM cassettes")
    parser.add_argument("--out", type=Path, default=ROOT / "tests" / "fixtures" / "llm")
    parser.add_argument("--budget", type=float, default=float(os.environ.get("LIVE_EVAL_BUDGET_USD", "2")))
    parser.add_argument("--limit", type=int, default=None, help="at most this many items per dataset")
    args = parser.parse_args(argv)
    provider = os.environ.get("FRAUD_SUMMARY_PROVIDER", "anthropic")
    if (var := live.missing_key(provider)) is not None or provider == "fake":
        print(f"record-fixtures not run: set {var or 'FRAUD_SUMMARY_PROVIDER to a real provider'}")
        return 1
    env = {**os.environ, "FRAUD_SUMMARY_PROVIDER": provider, "FRAUD_JUDGE_PROVIDER": provider}
    budget = live.Budget(limit_usd=args.budget)
    gateway = live.live_gateway(env, budget, store=CassetteStore(args.out))

    def rows(name: str) -> list[dict]:  # type: ignore[type-arg]
        lines = (
            (ROOT / "evals" / "datasets" / name / "cases.v1.jsonl").read_text(encoding="utf-8").splitlines()
        )
        return [json.loads(line) for line in lines][: args.limit]

    summaries = live.summarize_cases(rows("summaries"), gateway)
    judged = 0
    for item in rows("judge_calibration"):
        if budget.exhausted:
            break
        asyncio.run(judge(gateway, item["input"]["facts"], item["input"]["summary"]))
        judged += 1
    done = sum(1 for r in summaries if r.result is not None)
    print(
        f"recorded {done} summaries and {judged} judge verdicts into {args.out}; spent USD {budget.spent_usd:.4f}"
    )
    return 0 if done == len(summaries) else 1


if __name__ == "__main__":
    raise SystemExit(main())
