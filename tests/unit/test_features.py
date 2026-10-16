"""Point-in-time correctness of the offline features (spec FR-3)."""

from __future__ import annotations

from typing import Any

import numpy as np
from hypothesis import given, settings
from hypothesis import strategies as st

from training.dataset import events_from_dicts
from training.features import compute_features, feature_names, features_naive

BASE = 1_788_000_000_000
DEVICES = ["a" * 64, "b" * 64, "c" * 64, "d" * 64]
MCCS = ["5411", "5732", "4829", "5815"]
CHANNELS = {"CARD_PRESENT": 1, "ECOMMERCE": 2, "P2P": 3}


@st.composite
def histories(draw: st.DrawFn, max_events: int = 40) -> list[dict[str, Any]]:
    n = draw(st.integers(min_value=1, max_value=max_events))
    offsets = draw(
        st.lists(st.integers(min_value=0, max_value=40 * 86_400_000), min_size=n, max_size=n, unique=True)
    )
    out = []
    for i, off in enumerate(sorted(offsets)):
        kind = draw(st.sampled_from(["tx", "tx", "tx", "login", "device"]))
        h: dict[str, Any] = {
            "kind": kind,
            "ts": BASE + off,
            "device": draw(st.sampled_from(DEVICES)),
            "success": draw(st.booleans()),
            "change": draw(
                st.sampled_from(["device_bound", "password_changed", "email_changed", "phone_changed"])
            ),
            "amount": draw(st.integers(min_value=1, max_value=500_000)),
            "mcc": draw(st.sampled_from(MCCS)),
            "channel": draw(st.sampled_from(list(CHANNELS))),
            "id": f"0192f000-0000-7000-8000-{i:012d}",
        }
        out.append(h)
    return out


def to_rows(history: list[dict[str, Any]], account: str = "acc_00000001") -> list[dict[str, object]]:
    rows: list[dict[str, object]] = []
    for h in history:
        from datetime import UTC, datetime

        ts = (
            datetime.fromtimestamp(h["ts"] / 1000, tz=UTC).strftime("%Y-%m-%dT%H:%M:%S.")
            + f"{h['ts'] % 1000:03d}Z"
        )
        base: dict[str, object] = {
            "event_id": h["id"],
            "account_id": account,
            "event_ts": ts,
            "device_id": h["device"],
        }
        if h["kind"] == "tx":
            base |= {
                "schema": "fraud.event.transaction.v1",
                "amount_minor": h["amount"],
                "mcc": h["mcc"],
                "channel": h["channel"],
            }
        elif h["kind"] == "login":
            base |= {"schema": "fraud.event.login.v1", "success": h["success"]}
        else:
            base |= {"schema": "fraud.event.device.v1", "change": h["change"]}
        rows.append(base)
    return rows


def naive_tx(h: dict[str, Any]) -> dict[str, Any]:
    return {**h, "channel": CHANNELS[h["channel"]]}


@settings(max_examples=150, deadline=None)
@given(histories())
def test_vectorized_matches_naive(history: list[dict[str, Any]]) -> None:
    fm = compute_features(events_from_dicts(to_rows(history)))
    txs = [h for h in history if h["kind"] == "tx"]
    assert len(fm.values) == len(txs)
    for row, tx in zip(fm.values, txs, strict=True):
        expected = features_naive(history, naive_tx(tx))
        np.testing.assert_array_equal(row, np.asarray(expected, dtype=np.float64), err_msg=str(tx))


@settings(max_examples=150, deadline=None)
@given(histories(), histories(max_events=15))
def test_point_in_time_excludes_future_events(
    history: list[dict[str, Any]], future: list[dict[str, Any]]
) -> None:
    """Adding any events at or after a transaction's timestamp never changes its features."""
    txs = [h for h in history if h["kind"] == "tx"]
    if not txs:
        return
    target = txs[len(txs) // 2]
    shifted = []
    for i, f in enumerate(future):
        g = dict(f)
        g["ts"] = target["ts"] + 1 + (f["ts"] - BASE)
        g["id"] = f"0192f000-0000-7000-9000-{i:012d}"
        shifted.append(g)
    taken = {h["ts"] for h in history}
    shifted = [g for g in shifted if g["ts"] not in taken]
    before = compute_features(events_from_dicts(to_rows(history)))
    after = compute_features(events_from_dicts(to_rows(sorted(history + shifted, key=lambda h: h["ts"]))))
    idx_before = before.transaction_id.index(target["id"])
    idx_after = after.transaction_id.index(target["id"])
    np.testing.assert_array_equal(before.values[idx_before], after.values[idx_after])


def test_feature_order_matches_spec() -> None:
    assert feature_names() == [
        "tx_count_10m",
        "tx_count_1h",
        "tx_count_24h",
        "amount_sum_1h_minor",
        "amount_minor",
        "distinct_devices_24h",
        "new_device",
        "minutes_since_pwd_change",
        "minutes_since_email_change",
        "mcc_novel_30d",
        "channel",
        "hour_of_day_utc",
        "amount_ratio_24h",
    ]


def test_card_testing_burst_counts() -> None:
    history = [
        {
            "kind": "tx",
            "ts": BASE + i * 30_000,
            "device": DEVICES[0],
            "success": True,
            "change": "",
            "amount": 100,
            "mcc": "5815",
            "channel": "ECOMMERCE",
            "id": f"0192f000-0000-7000-8000-{i:012d}",
        }
        for i in range(10)
    ]
    fm = compute_features(events_from_dicts(to_rows(history)))
    assert fm.values[9, 0] == 9  # nine earlier transactions within 10 minutes
    assert fm.values[0, 6] == 1  # first sighting of the device
    assert fm.values[1, 6] == 0
    assert fm.values[0, 12] == 1.0
