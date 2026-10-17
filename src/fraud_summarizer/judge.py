"""Summary-usefulness judge (route `judge_usefulness`, live evals only; spec §11).

The judge never runs on the scoring or summary path: `make eval-live` uses it after it agrees with the
labelled calibration set (`evals/datasets/judge_calibration`, agreement >= 0.8). Facts and the candidate
summary are fenced as data, like the case-summary prompt, so text inside either cannot steer the judge.
"""

from __future__ import annotations

import json
import re
from typing import TYPE_CHECKING, Any

from pydantic import BaseModel, ConfigDict, Field

from fraud_summarizer import facts as factfmt
from fraud_summarizer.paths import repo_root

if TYPE_CHECKING:
    from fraud_summarizer.llm import LLMGateway, LLMResult

JUDGE_PROMPT_VERSION = "judge_summary_usefulness.v1"
BEGIN, END = "<candidate_summary>", "</candidate_summary>"
_BLOCK = re.compile(re.escape(BEGIN) + r"\s*```json\s*(\{.*?\})\s*```\s*" + re.escape(END), re.DOTALL)


class JudgeVerdict(BaseModel):
    model_config = ConfigDict(extra="forbid", title="judge_usefulness_v1")

    useful: bool
    rationale: str = Field(min_length=1, max_length=300)


def load_judge_prompt() -> str:
    return (repo_root() / "prompts" / f"{JUDGE_PROMPT_VERSION}.md").read_text(encoding="utf-8")


def judge_message(facts: dict[str, str], summary: dict[str, Any]) -> str:
    body = json.dumps(summary, indent=1, sort_keys=True, ensure_ascii=False)
    body = body.replace("<", "\\u003c").replace(">", "\\u003e").replace("`", "\\u0060")
    return (
        "Judge whether this summary is useful to the analyst.\n\n"
        f"{factfmt.render(facts)}\n\n{BEGIN}\n```json\n{body}\n```\n{END}"
    )


def parse_message(text: str) -> tuple[dict[str, str], dict[str, Any]]:
    m = _BLOCK.search(text)
    if not m:
        raise ValueError("no <candidate_summary> block in prompt")
    summary: dict[str, Any] = json.loads(m.group(1))
    return factfmt.parse(text), summary


def _validate(data: dict[str, Any]) -> None:
    JudgeVerdict.model_validate(data)


async def judge(
    gateway: LLMGateway, facts: dict[str, str], summary: dict[str, Any]
) -> tuple[JudgeVerdict, LLMResult]:
    result = await gateway.complete(
        "judge_usefulness",
        system=load_judge_prompt(),
        user=judge_message(facts, summary),
        schema_name="judge_usefulness_v1",
        schema=JudgeVerdict.model_json_schema(),
        prompt_version=JUDGE_PROMPT_VERSION,
        validate=_validate,
    )
    return JudgeVerdict.model_validate(result.data), result
