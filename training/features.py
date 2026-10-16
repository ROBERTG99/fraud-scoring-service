"""Offline point-in-time features (features/spec.yaml v1).

`compute_features` replays every account's history and returns, for each transaction, the 13 features
as of its timestamp, using only events strictly before it. It must match the Go online path
(internal/features, score_tx.lua) exactly; `make feature-parity` checks that on the parity fixture.
"""

from __future__ import annotations

from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path
from typing import Any

import numpy as np
import numpy.typing as npt
import yaml

from training.dataset import KIND_DEVICE, KIND_LOGIN, KIND_TX, Events

SPEC_PATH = Path(__file__).resolve().parents[1] / "features" / "spec.yaml"

MS_10M = 600_000
MS_1H = 3_600_000
MS_24H = 86_400_000
MS_30D = 30 * MS_24H
MS_90D = 90 * MS_24H
MINUTES_CAP = 43_200
CHANGE_PWD, CHANGE_EMAIL, CHANGE_BOUND = 2, 3, 1

F64 = npt.NDArray[np.float64]
I64 = npt.NDArray[np.int64]


@lru_cache(maxsize=1)
def load_spec(path: Path = SPEC_PATH) -> dict[str, Any]:
    spec = yaml.safe_load(path.read_text(encoding="utf-8"))
    assert spec["version"] == "v1"
    return spec  # type: ignore[no-any-return]


def feature_names() -> list[str]:
    return [f["name"] for f in load_spec()["features"]]


@dataclass
class FeatureMatrix:
    """Feature rows for every transaction, in the order of `event_index`."""

    values: F64  # shape (n_tx, 13)
    event_index: I64  # row -> index into Events
    transaction_id: list[str]


def _count_before(times: I64, lo: I64, hi: I64) -> I64:
    """Number of elements of sorted `times` in [lo, hi)."""
    return np.searchsorted(times, hi, side="left") - np.searchsorted(times, lo, side="left")


def _last_before(times: I64, t: I64) -> I64:
    """Latest element of sorted `times` strictly before each t, or -1 (as int64 sentinel)."""
    idx = np.searchsorted(times, t, side="left") - 1
    out = np.full(len(t), -1, dtype=np.int64)
    ok = idx >= 0
    out[ok] = times[idx[ok]]
    return out


def compute_features(ev: Events) -> FeatureMatrix:
    n_tx = int(np.count_nonzero(ev.kind == KIND_TX))
    values = np.zeros((n_tx, 13), dtype=np.float64)
    event_index = np.zeros(n_tx, dtype=np.int64)
    # Account boundaries (events are sorted by account, then ts).
    bounds = np.flatnonzero(np.diff(ev.account)) + 1
    starts = np.concatenate(([0], bounds))
    ends = np.concatenate((bounds, [len(ev)]))
    row = 0
    for s, e in zip(starts.tolist(), ends.tolist(), strict=True):
        kind = ev.kind[s:e]
        tx_mask = kind == KIND_TX
        k = int(np.count_nonzero(tx_mask))
        if k == 0:
            continue
        ts = ev.ts[s:e]
        tx_pos = np.flatnonzero(tx_mask)
        t = ts[tx_pos]
        amt = ev.amount[s:e][tx_pos]
        out = values[row : row + k]
        event_index[row : row + k] = tx_pos + s
        _account_features(ev, s, e, t, amt, tx_pos, out)
        row += k
    tx_ids = [ev.event_id[i] for i in event_index.tolist()]
    return FeatureMatrix(values=values, event_index=event_index, transaction_id=tx_ids)


def _account_features(ev: Events, s: int, e: int, t: I64, amt: I64, tx_pos: I64, out: F64) -> None:
    kind = ev.kind[s:e]
    ts = ev.ts[s:e]
    device = ev.device[s:e]
    change = ev.change[s:e]
    # Transactions of this account are strictly increasing in time, so prefix sums give window sums.
    csum = np.concatenate(([0], np.cumsum(amt)))
    i_now = np.arange(len(t))  # index of the current tx == number of earlier txs
    i_10m = np.searchsorted(t, t - MS_10M, side="left")
    i_1h = np.searchsorted(t, t - MS_1H, side="left")
    i_24h = np.searchsorted(t, t - MS_24H, side="left")
    count_10m = i_now - i_10m
    count_1h = i_now - i_1h
    count_24h = i_now - i_24h
    sum_1h = csum[i_now] - csum[i_1h]
    sum_24h = csum[i_now] - csum[i_24h]

    # Device sightings: successful logins, device_bound events, transactions.
    sighting = (
        (kind == KIND_TX)
        | ((kind == KIND_LOGIN) & ev.success[s:e])
        | ((kind == KIND_DEVICE) & (change == CHANGE_BOUND))
    )
    s_ts = ts[sighting]
    s_dev = device[sighting]
    tx_dev = device[tx_pos]
    distinct = np.zeros(len(t), dtype=np.int64)
    new_device = np.ones(len(t), dtype=np.int64)
    for d in np.unique(s_dev):
        d_times = s_ts[s_dev == d]
        last = _last_before(d_times, t)
        seen = last >= 0
        distinct += (seen & (last >= t - MS_24H)).astype(np.int64)
        mine = tx_dev == d
        new_device[mine & seen & (last >= t - MS_90D)] = 0

    # Credential changes.
    def minutes_since(code: int) -> I64:
        times = ts[(kind == KIND_DEVICE) & (change == code)]
        last = _last_before(times, t)
        mins = np.where(last >= 0, (t - last) // 60_000, MINUTES_CAP)
        return np.minimum(mins, MINUTES_CAP)

    # MCC novelty over 30 days (transactions only).
    tx_mcc = ev.mcc[s:e][tx_pos]
    novel = np.ones(len(t), dtype=np.int64)
    for m in np.unique(tx_mcc):
        mine = tx_mcc == m
        m_times = t[mine]
        last = _last_before(m_times, t[mine])
        novel[np.flatnonzero(mine)[(last >= 0) & (last >= t[mine] - MS_30D)]] = 0

    out[:, 0] = count_10m
    out[:, 1] = count_1h
    out[:, 2] = count_24h
    out[:, 3] = sum_1h
    out[:, 4] = amt
    out[:, 5] = distinct
    out[:, 6] = new_device
    out[:, 7] = minutes_since(CHANGE_PWD)
    out[:, 8] = minutes_since(CHANGE_EMAIL)
    out[:, 9] = novel
    out[:, 10] = ev.channel[s:e][tx_pos]
    out[:, 11] = (t // MS_1H) % 24
    with np.errstate(divide="ignore", invalid="ignore"):
        ratio = amt.astype(np.float64) / (sum_24h.astype(np.float64) / count_24h.astype(np.float64))
    out[:, 12] = np.where(count_24h > 0, ratio, 1.0)


def features_naive(history: list[dict[str, Any]], tx: dict[str, Any]) -> list[float]:
    """Reference implementation for one transaction (used by property tests).

    `history` holds events as dicts with keys kind ('login'|'device'|'tx'), ts, device, success,
    change, amount, mcc. Only events strictly before tx['ts'] are used.
    """
    t = int(tx["ts"])
    past = [h for h in history if int(h["ts"]) < t]
    txs = [h for h in past if h["kind"] == "tx"]

    def in_window(h: dict[str, Any], w: int) -> bool:
        return t - w <= int(h["ts"]) < t

    c24 = [h for h in txs if in_window(h, MS_24H)]
    sightings = [
        h
        for h in past
        if h["kind"] == "tx"
        or (h["kind"] == "login" and h["success"])
        or (h["kind"] == "device" and h["change"] == "device_bound")
    ]
    last_seen: dict[str, int] = {}
    for h in sightings:
        last_seen[h["device"]] = max(last_seen.get(h["device"], -1), int(h["ts"]))

    def mins(change: str) -> int:
        times = [int(h["ts"]) for h in past if h["kind"] == "device" and h["change"] == change]
        return MINUTES_CAP if not times else min(MINUTES_CAP, (t - max(times)) // 60_000)

    mcc_seen = [int(h["ts"]) for h in txs if h["mcc"] == tx["mcc"] and in_window(h, MS_30D)]
    sum24 = sum(int(h["amount"]) for h in c24)
    amount = int(tx["amount"])
    ratio = amount / (float(sum24) / float(len(c24))) if c24 else 1.0
    dev_last = last_seen.get(tx["device"], -1)
    return [
        float(sum(1 for h in txs if in_window(h, MS_10M))),
        float(sum(1 for h in txs if in_window(h, MS_1H))),
        float(len(c24)),
        float(sum(int(h["amount"]) for h in txs if in_window(h, MS_1H))),
        float(amount),
        float(sum(1 for ls in last_seen.values() if t - MS_24H <= ls)),
        0.0 if dev_last >= 0 and dev_last >= t - MS_90D else 1.0,
        float(mins("password_changed")),
        float(mins("email_changed")),
        0.0 if mcc_seen else 1.0,
        float(tx["channel"]),
        float((t // MS_1H) % 24),
        ratio,
    ]
