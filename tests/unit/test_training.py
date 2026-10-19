"""Training pipeline: temporal split, calibration map and ONNX parity (spec FR-5, T-14)."""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest
from hypothesis import given, settings
from hypothesis import strategies as st

from training import train as T
from training.export_onnx import run_onnx

ROOT = Path(__file__).resolve().parents[2]
PARITY = ROOT / "tests" / "fixtures" / "parity"
DAY = 86_400_000


@pytest.fixture(scope="module")
def prepared() -> T.Prepared:
    return T.prepare(PARITY)


def test_temporal_split_has_no_overlap(prepared: T.Prepared) -> None:
    ranges = {}
    ids = {}
    for split in ("train", "valid", "test"):
        mask = prepared.split == split
        assert mask.any(), split
        ranges[split] = (prepared.ts[mask].min(), prepared.ts[mask].max())
        ids[split] = {tx for tx, m in zip(prepared.transaction_id, mask, strict=True) if m}
    assert ranges["train"][1] < ranges["valid"][0]
    assert ranges["valid"][1] < ranges["test"][0]
    assert (
        not ids["train"] & ids["valid"] and not ids["valid"] & ids["test"] and not ids["train"] & ids["test"]
    )
    # Splits are whole days: no calendar day appears in two splits.
    days = {s: {int(t // DAY) for t in prepared.ts[prepared.split == s]} for s in ("train", "valid", "test")}
    assert not days["train"] & days["valid"] and not days["valid"] & days["test"]


def test_onnx_matches_native_within_1e5(prepared: T.Prepared, tmp_path: Path) -> None:
    meta = T.train(PARITY, tmp_path, tiny=True)
    assert meta["onnx_max_abs_diff"] <= 1e-5
    onnx_bytes = (tmp_path / "model.onnx").read_bytes()
    x = prepared.x[prepared.split == "test"].astype(np.float32)
    scores = run_onnx(onnx_bytes, x)
    assert scores.shape == (len(x),)
    assert np.all((scores >= 0) & (scores <= 1))
    stored = json.loads((tmp_path / "model_meta.json").read_text(encoding="utf-8"))
    assert stored["golden"]["probability"] == pytest.approx(
        float(run_onnx(onnx_bytes, np.asarray([T.golden_vector()], dtype=np.float32))[0]), abs=0
    )


@settings(max_examples=200, deadline=None)
@given(
    st.lists(st.floats(min_value=0, max_value=1, allow_nan=False), min_size=2, max_size=40, unique=True),
    st.lists(st.floats(min_value=0, max_value=1, allow_nan=False), min_size=2, max_size=40),
)
def test_bounded_slope_is_monotone_and_lipschitz(xs: list[float], ys: list[float]) -> None:
    n = min(len(xs), len(ys))
    x = np.sort(np.asarray(xs[:n]))
    y = np.sort(np.asarray(ys[:n]))
    bx, by = T.bounded_slope(x, y)
    assert np.all(np.diff(bx) > 0)
    assert np.all(np.diff(by) >= 0)
    slopes = np.diff(by) / np.diff(bx)
    assert np.all(slopes <= T.MAX_CALIBRATION_SLOPE * (1 + 1e-9))
    assert np.array_equal(by, y)  # calibrated values are kept; only breakpoints move
    assert bx[-1] <= max(1.0, x[-1]) + 1e-12
    assert bx[0] >= -1e-12


def test_fallback_defaults_are_p90(prepared: T.Prepared) -> None:
    d = T.fallback_defaults(prepared.x[prepared.split == "train"])
    assert set(d) == set(T.FALLBACK_FEATURES)
    assert d["minutes_since_pwd_change"] == 43200
