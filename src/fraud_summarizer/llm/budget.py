"""Daily LLM cost cap (security audit, D-048).

Every provider response is charged at list price (`RouteConfig.cost_usd`). Once the UTC day's spend reaches
the limit the gateway makes no further provider call that day; the pipeline writes the templated summary
instead. The count is per process: N summarizer replicas may spend N times the limit.
"""

from __future__ import annotations

import math
import os
import threading
from collections.abc import Callable
from datetime import UTC, date, datetime

DEFAULT_DAILY_BUDGET_USD = 25.0


class DailyBudget:
    def __init__(self, limit_usd: float, now: Callable[[], datetime] | None = None) -> None:
        self.limit_usd = limit_usd
        self._now = now or (lambda: datetime.now(UTC))
        self._day: date | None = None
        self._spent = 0.0
        self._lock = threading.Lock()

    def _roll(self) -> None:
        today = self._now().astimezone(UTC).date()
        if today != self._day:
            self._day, self._spent = today, 0.0

    @property
    def spent_usd(self) -> float:
        with self._lock:
            self._roll()
            return self._spent

    def exhausted(self) -> bool:
        """True once today's spend reached the limit (a call's cost is known only after it returns)."""
        with self._lock:
            self._roll()
            return self._spent >= self.limit_usd

    def charge(self, usd: float) -> None:
        with self._lock:
            self._roll()
            self._spent += usd


def budget_from_env() -> DailyBudget:
    raw = os.environ.get("FRAUD_LLM_DAILY_BUDGET_USD", "")
    if not raw:
        return DailyBudget(DEFAULT_DAILY_BUDGET_USD)
    try:
        limit = float(raw)
    except ValueError:
        limit = math.nan
    if not math.isfinite(limit) or limit < 0:
        raise ValueError(f"FRAUD_LLM_DAILY_BUDGET_USD must be a finite amount >= 0, got {raw!r}")
    return DailyBudget(limit)
