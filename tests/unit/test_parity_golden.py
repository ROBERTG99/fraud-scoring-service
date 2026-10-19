"""Offline features of the parity fixture equal the golden file the Go parity test reads."""

from __future__ import annotations

import csv
from pathlib import Path

from training.dataset import load_events
from training.features import compute_features, feature_names

ROOT = Path(__file__).resolve().parents[2]


def test_offline_features_match_golden() -> None:
    fm = compute_features(load_events(ROOT / "tests/fixtures/parity/events.jsonl"))
    with (ROOT / "tests/fixtures/parity/offline_features.csv").open(encoding="utf-8", newline="") as fh:
        rows = list(csv.reader(fh))
    assert rows[0][1:] == feature_names()
    golden = {r[0]: [float(v) for v in r[1:]] for r in rows[1:]}
    assert len(golden) == len(fm.transaction_id) == 3502
    for tx_id, values in zip(fm.transaction_id, fm.values, strict=True):
        assert golden[tx_id] == [float(v) for v in values], tx_id
