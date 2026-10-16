"""Fact rendering shared by the prompt builder, FakeLLM and the faithfulness checker.

Facts are flat `key -> string value` pairs (`decision.score`, `feature.<name>`, `event.<kind>.<n>.<field>`).
In prompts they are fenced as data between <case_facts> tags so instructions inside values (for example a
merchant name) are never read as instructions.
"""

from __future__ import annotations

import json
import re

BEGIN = "<case_facts>"
END = "</case_facts>"
_BLOCK = re.compile(re.escape(BEGIN) + r"\s*```json\s*(\{.*?\})\s*```\s*" + re.escape(END), re.DOTALL)


def render(facts: dict[str, str]) -> str:
    body = json.dumps(dict(sorted(facts.items())), indent=1, ensure_ascii=False)
    return f"{BEGIN}\n```json\n{body}\n```\n{END}"


def parse(text: str) -> dict[str, str]:
    m = _BLOCK.search(text)
    if not m:
        raise ValueError("no <case_facts> block in prompt")
    data = json.loads(m.group(1))
    return {str(k): str(v) for k, v in data.items()}
