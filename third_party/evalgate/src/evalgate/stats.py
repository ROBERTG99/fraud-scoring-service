"""Percentile bootstrap with numpy ``PCG64``: one-sample means and paired deltas (as upstream)."""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass

import numpy as np

DEFAULT_RESAMPLES = 10_000
DEFAULT_SEED = 1337


@dataclass(frozen=True)
class Interval:
    estimate: float
    low: float
    high: float
    n: int


def mean_ci(
    values: Sequence[float], seed: int = DEFAULT_SEED, resamples: int = DEFAULT_RESAMPLES, level: float = 0.95
) -> Interval:
    """Percentile CI of the mean of ``values``."""
    arr = np.asarray(values, dtype=np.float64)
    if arr.size == 0:
        return Interval(0.0, 0.0, 0.0, 0)
    rng = np.random.Generator(np.random.PCG64(seed))
    means = np.empty(resamples, dtype=np.float64)
    chunk = max(1, 20_000_000 // arr.size)  # bounds memory for large datasets
    for start in range(0, resamples, chunk):
        k = min(chunk, resamples - start)
        means[start : start + k] = arr[rng.integers(0, arr.size, size=(k, arr.size))].mean(axis=1)
    tail = (1 - level) / 2 * 100
    lo, hi = np.percentile(means, [tail, 100 - tail])
    return Interval(float(arr.mean()), float(lo), float(hi), int(arr.size))


def paired_delta_ci(
    baseline: Sequence[float],
    candidate: Sequence[float],
    seed: int = DEFAULT_SEED,
    resamples: int = DEFAULT_RESAMPLES,
) -> Interval:
    """Percentile CI of ``mean(candidate - baseline)``, resampling paired cases together."""
    base = np.asarray(baseline, dtype=np.float64)
    cand = np.asarray(candidate, dtype=np.float64)
    if base.shape != cand.shape:
        raise ValueError("paired samples must have the same length")
    return mean_ci(list(cand - base), seed=seed, resamples=resamples)
