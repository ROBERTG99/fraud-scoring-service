"""Evaluation metrics: PR-AUC, recall at a fixed false-positive rate, expected calibration error."""

from __future__ import annotations

import numpy as np
import numpy.typing as npt
from sklearn.metrics import average_precision_score, roc_auc_score

F64 = npt.NDArray[np.float64]


def pr_auc(y: npt.ArrayLike, score: npt.ArrayLike) -> float:
    return float(average_precision_score(np.asarray(y), np.asarray(score)))


def roc_auc(y: npt.ArrayLike, score: npt.ArrayLike) -> float:
    return float(roc_auc_score(np.asarray(y), np.asarray(score)))


def threshold_at_fpr(y: npt.ArrayLike, score: npt.ArrayLike, max_fpr: float) -> float:
    """Smallest threshold t such that FPR(score >= t) <= max_fpr (ties handled conservatively)."""
    y_arr = np.asarray(y).astype(bool)
    s = np.asarray(score, dtype=np.float64)
    neg = np.sort(s[~y_arr])[::-1]
    if len(neg) == 0:
        return float(s.min())
    k = int(np.floor(max_fpr * len(neg)))  # number of negatives allowed at or above the threshold
    if k >= len(neg):
        return float(neg[-1])
    # Threshold strictly above the (k+1)-th highest negative so at most k negatives pass.
    return float(np.nextafter(neg[k], np.inf))


def recall_at_fpr(
    y: npt.ArrayLike, score: npt.ArrayLike, max_fpr: float = 0.01
) -> tuple[float, float, float]:
    """Return (recall, achieved_fpr, threshold) at the largest operating point with FPR <= max_fpr."""
    y_arr = np.asarray(y).astype(bool)
    s = np.asarray(score, dtype=np.float64)
    t = threshold_at_fpr(y_arr, s, max_fpr)
    flagged = s >= t
    recall = float(flagged[y_arr].mean()) if y_arr.any() else 0.0
    fpr = float(flagged[~y_arr].mean()) if (~y_arr).any() else 0.0
    return recall, fpr, t


def recall_at_fpr_with_flags(
    y: npt.ArrayLike, score: npt.ArrayLike, flags: npt.ArrayLike, max_fpr: float = 0.01
) -> tuple[float, float, float]:
    """Combined operating point: flagged = rule flag OR score >= t, with t chosen so total FPR <= max_fpr.

    If the rule flags alone exceed the FPR budget, the score adds nothing (t = +inf) and the reported FPR
    is the rules' own FPR.
    """
    y_arr = np.asarray(y).astype(bool)
    s = np.asarray(score, dtype=np.float64)
    f = np.asarray(flags).astype(bool)
    neg_total = int((~y_arr).sum())
    budget = int(np.floor(max_fpr * neg_total)) - int((f & ~y_arr).sum())
    if budget < 0:
        t = np.inf
    else:
        candidates = np.sort(s[~y_arr & ~f])[::-1]
        t = float(np.nextafter(candidates[budget], np.inf)) if budget < len(candidates) else -np.inf
    flagged = f | (s >= t)
    recall = float(flagged[y_arr].mean()) if y_arr.any() else 0.0
    fpr = float(flagged[~y_arr].mean()) if neg_total else 0.0
    return recall, fpr, float(t)


def expected_calibration_error(y: npt.ArrayLike, prob: npt.ArrayLike, bins: int = 15) -> float:
    """ECE with equal-width bins over [0, 1], weighted by bin size."""
    y_arr = np.asarray(y, dtype=np.float64)
    p = np.clip(np.asarray(prob, dtype=np.float64), 0.0, 1.0)
    idx = np.minimum((p * bins).astype(int), bins - 1)
    ece = 0.0
    for b in range(bins):
        mask = idx == b
        if mask.any():
            ece += mask.mean() * abs(p[mask].mean() - y_arr[mask].mean())
    return float(ece)
