"""Metrics: the ``exact`` built-in and ``@evalgate.metric`` custom metrics (``kind: custom``)."""

from __future__ import annotations

import inspect
import math
from collections.abc import Callable
from typing import Any

from evalgate.contracts import Case, MetricScore, MetricSpec, TargetResult

CustomFn = Callable[..., Any]
_REGISTRY: dict[str, CustomFn] = {}
_MISSING = object()


def metric(name: str) -> Callable[[CustomFn], CustomFn]:
    """Register a custom metric, called as ``fn(case, result)`` or ``fn(case, result, params)``.

    It returns a float in [0, 1], a bool, or a :class:`MetricScore`.
    """

    def decorator(fn: CustomFn) -> CustomFn:
        if name in _REGISTRY and _REGISTRY[name] is not fn:
            raise ValueError(f"metric {name!r} is already registered")
        _REGISTRY[name] = fn
        return fn

    return decorator


def select(path: str, ctx: dict[str, Any]) -> Any:
    """Resolve ``output.items.0.name`` against ``{"input", "output", "expected", "meta"}``."""
    cur: Any = ctx
    for part in path.split("."):
        if isinstance(cur, dict):
            cur = cur.get(part, _MISSING)
        elif isinstance(cur, list) and part.lstrip("-").isdigit() and -len(cur) <= int(part) < len(cur):
            cur = cur[int(part)]
        else:
            return _MISSING
        if cur is _MISSING:
            return _MISSING
    return cur


def worst(spec: MetricSpec) -> float:
    """The score of a case that produced no output: the worst end of the metric."""
    return 1.0 if spec.direction == "lower" else 0.0


def _clamp(value: float) -> float:
    if not math.isfinite(value):
        raise ValueError(f"metric value {value!r} is not a finite number")
    return max(0.0, min(1.0, float(value)))


class Scorer:
    """Scores one metric on one case."""

    def __init__(self, spec: MetricSpec) -> None:
        self.spec = spec
        self.params = dict(spec.params)
        default_pass = 0.5 if spec.kind == "custom" else 1.0
        self.pass_at = float(
            self.params.get("pass_at", 1.0 - default_pass if spec.direction == "lower" else default_pass)
        )
        if spec.kind == "custom":
            target = str(self.params.get("metric", spec.name))
            if target not in _REGISTRY:
                raise KeyError(
                    f"unknown custom metric {target!r}; registered: {', '.join(sorted(_REGISTRY)) or 'none'}"
                )
            self.fn: CustomFn | None = _REGISTRY[target]
        elif spec.kind == "exact":
            self.fn = None
        else:
            raise NotImplementedError(f"metric kind {spec.kind!r} is not in the vendored evalgate subset")

    def _passed(self, value: float) -> bool:
        return value <= self.pass_at if self.spec.direction == "lower" else value >= self.pass_at

    def score(self, case: Case, result: TargetResult) -> MetricScore:
        if self.fn is None:  # exact
            ctx = {
                "input": case.input,
                "output": result.output,
                "expected": case.expected or {},
                "meta": case.meta,
            }
            actual = select(str(self.params.get("actual", "output")), ctx)
            expected = select(str(self.params.get("expected", "expected")), ctx)
            value = 1.0 if actual is not _MISSING and expected is not _MISSING and actual == expected else 0.0
            return MetricScore(name=self.spec.name, value=value, passed=self._passed(value))
        options = {k: v for k, v in self.params.items() if k not in ("metric", "pass_at")}
        takes_params = len(inspect.signature(self.fn).parameters) >= 3
        raw = self.fn(case, result, options) if takes_params else self.fn(case, result)
        if isinstance(raw, MetricScore):
            return raw.model_copy(update={"name": self.spec.name})
        value = _clamp(1.0 if raw is True else 0.0 if raw is False else float(raw))
        return MetricScore(name=self.spec.name, value=value, passed=self._passed(value))
