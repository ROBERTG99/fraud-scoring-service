"""Contracts shared with upstream evalgate (main at 0681335): suite.v1, dataset cases, report.v1, check.v1.

Field names, types and ``extra="forbid"`` match upstream so reports and suites carry over unchanged when the
tagged release replaces this subset (D-002, D-030).
"""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

Split = Literal["dev", "test"]
Direction = Literal["higher", "lower"]
MetricKind = Literal[
    "exact", "json_schema", "numeric_tolerance", "tool_calls", "contains_all", "regex", "judge", "custom"
]


def _default_splits() -> list[Split]:
    return ["dev", "test"]


class Case(BaseModel):
    """A dataset row (one JSONL line)."""

    model_config = ConfigDict(extra="forbid")

    id: str = Field(min_length=1, max_length=200)
    split: Split
    input: dict[str, Any]
    expected: dict[str, Any] | None = None
    tags: list[str] = Field(default_factory=list)
    meta: dict[str, Any] = Field(default_factory=dict)


class TargetResult(BaseModel):
    """What a target returns for one case."""

    model_config = ConfigDict(extra="forbid")

    output: dict[str, Any] | str
    steps: list[dict[str, Any]] = Field(default_factory=list)
    cost_usd: float = Field(default=0.0, ge=0)
    latency_ms: float = Field(default=0.0, ge=0)
    trace_id: str | None = None


class MetricScore(BaseModel):
    """The score of one metric on one case. ``value`` is in [0, 1]."""

    model_config = ConfigDict(extra="forbid")

    name: str
    value: float = Field(ge=0.0, le=1.0)
    passed: bool
    details: dict[str, Any] = Field(default_factory=dict)


class TargetSpec(BaseModel):
    """Upstream also accepts ``http`` and ``langgraph`` targets; this subset runs ``python`` only."""

    model_config = ConfigDict(extra="forbid")

    python: str = Field(pattern=r"^[A-Za-z_][\w.]*:[A-Za-z_]\w*$")


class MetricSpec(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str = Field(pattern=r"^[a-z][a-z0-9_]*$", max_length=64)
    kind: MetricKind
    params: dict[str, Any] = Field(default_factory=dict)
    threshold: float | None = None
    direction: Direction = "higher"
    gate: bool = True


class BudgetSpec(BaseModel):
    model_config = ConfigDict(extra="forbid")

    max_cost_usd: float | None = Field(default=None, ge=0)
    max_p95_ms: float | None = Field(default=None, gt=0)


class Suite(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    schema_: Literal["evalgate.suite.v1"] = Field(alias="schema")
    name: str = Field(pattern=r"^[a-z0-9][a-z0-9_.-]*$", max_length=64)
    description: str | None = None
    dataset: str = Field(min_length=1)
    target: TargetSpec
    concurrency: int = Field(default=8, ge=1, le=256)
    metrics: list[MetricSpec] = Field(min_length=1)
    budget: BudgetSpec | None = None
    splits: list[Split] = Field(default_factory=_default_splits)
    plugins: list[str] = Field(default_factory=list)

    @model_validator(mode="after")
    def _unique_metric_names(self) -> Suite:
        names = [m.name for m in self.metrics]
        dupes = sorted({n for n in names if names.count(n) > 1})
        if dupes:
            raise ValueError(f"duplicate metric names: {', '.join(dupes)}")
        return self


class MetricSummary(BaseModel):
    model_config = ConfigDict(extra="forbid", allow_inf_nan=False)

    value: float
    threshold: float | None = None
    direction: Direction = "higher"
    passed: bool
    ci_low: float | None = None
    ci_high: float | None = None


class CaseReport(BaseModel):
    model_config = ConfigDict(extra="forbid", allow_inf_nan=False)

    id: str
    split: Split
    scores: dict[str, float]
    passed: bool
    cost_usd: float = Field(ge=0)
    latency_ms: float = Field(ge=0)
    error: str | None = None


class Report(BaseModel):
    """The stable run report. ``extensions`` is free-form and may be ignored by consumers."""

    model_config = ConfigDict(extra="forbid", populate_by_name=True, allow_inf_nan=False)

    schema_: Literal["evalgate.report.v1"] = Field(alias="schema")
    suite: str
    run_id: str
    created_at: str
    git_sha: str | None = None
    mode: Literal["offline", "live"]
    models: list[str] = Field(default_factory=list)
    prompt_hashes: dict[str, str] = Field(default_factory=dict)
    n_cases: int = Field(ge=0)
    metrics: dict[str, MetricSummary]
    cost_usd_total: float = Field(ge=0)
    cost_usd_per_case: float = Field(ge=0)
    latency_p50_ms: float = Field(ge=0)
    latency_p95_ms: float = Field(ge=0)
    cases: list[CaseReport]
    extensions: dict[str, Any] | None = None

    @model_validator(mode="after")
    def _unique_case_ids(self) -> Report:
        """Pairing is by case id: a duplicate would be paired twice and inflate ``n_paired``."""
        seen: set[str] = set()
        for case in self.cases:
            if case.id in seen:
                raise ValueError(f"duplicate case id {case.id!r}")
            seen.add(case.id)
        return self


FailureReason = Literal["threshold", "regression", "budget_cost", "budget_p95", "error"]


class CheckFailure(BaseModel):
    model_config = ConfigDict(extra="forbid", allow_inf_nan=False)

    metric: str
    reason: FailureReason
    value: float | None = None
    threshold: float | None = None
    delta_pp: float | None = None
    ci_low_pp: float | None = None
    ci_high_pp: float | None = None


class Comparison(BaseModel):
    model_config = ConfigDict(extra="forbid", allow_inf_nan=False)

    metric: str
    baseline: float
    candidate: float
    delta_pp: float
    ci_low_pp: float
    ci_high_pp: float
    n_paired: int = Field(ge=0)


class CheckResult(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True, allow_inf_nan=False)

    schema_: Literal["evalgate.check.v1"] = Field(default="evalgate.check.v1", alias="schema")
    report_sha256: str
    baseline_sha256: str | None = None
    max_regression_pp: float
    seed: int
    resamples: int
    passed: bool
    failures: list[CheckFailure] = Field(default_factory=list)
    comparisons: list[Comparison] = Field(default_factory=list)
