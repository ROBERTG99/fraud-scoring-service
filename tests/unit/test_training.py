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


def _isotonic_only_graph(xs: np.ndarray, ys: np.ndarray) -> bytes:
    """The calibration nodes alone, fed a [n, 2] probability tensor like the tree ensemble's output."""
    import onnx
    from onnx import TensorProto, helper

    from training.export_onnx import INPUT, OUTPUT, isotonic_nodes

    nodes, inits = isotonic_nodes(xs, ys, INPUT)
    graph = helper.make_graph(
        nodes,
        "iso",
        [helper.make_tensor_value_info(INPUT, TensorProto.FLOAT, [None, 2])],
        [helper.make_tensor_value_info(OUTPUT, TensorProto.FLOAT, [None, 1])],
        inits,
    )
    model = helper.make_model(graph, opset_imports=[helper.make_opsetid("", 15)], ir_version=8)
    onnx.checker.check_model(model)
    return model.SerializeToString()


def test_isotonic_graph_error_does_not_grow_with_the_breakpoints() -> None:
    # T-48 (D-076): a model trained on linux/arm64 missed the 1e-5 parity bound on one row because the graph
    # summed float32 rounding over every segment below p. With 2,000 breakpoints at slope <= 10 the error of
    # the calibrated map must stay at float32 precision.
    rng = np.random.default_rng(7)
    xs = np.sort(rng.uniform(0.0, 1.0, 2001))
    xs[0], xs[-1] = 0.0, 1.0
    ys, _ = T.bounded_slope(xs, np.sort(rng.uniform(0.0, 1.0, 2001)))
    p = np.concatenate([rng.uniform(-0.01, 1.01, 20_000), xs[::7]]).astype(np.float32)
    probs = np.stack([1 - p, p], axis=1).astype(np.float32)
    got = run_onnx(_isotonic_only_graph(xs, ys), probs).astype(np.float64)
    want = np.clip(np.interp(p.astype(np.float64), xs, ys), 0.0, 1.0)
    assert float(np.max(np.abs(got - want))) < 1e-6


def test_tree_scores_do_not_depend_on_the_runtime_thread_count() -> None:
    # T-48 (D-076): onnxruntime summed the 300 trees in float32, in an order that depends on the thread count
    # and the CPU; one linux/arm64 run moved a raw score by 1.3e-6, which the calibration slope (<= 10) made
    # 1.3e-5 and failed the parity bound. The exported trees must accumulate in double.
    import onnxruntime as ort
    from xgboost import XGBClassifier

    from training.export_onnx import INPUT, _probabilities_output, _tree_model

    rng = np.random.default_rng(7)
    x = rng.normal(size=(4000, 13)).astype(np.float32)
    y = (x[:, 0] + 0.5 * x[:, 1] * x[:, 2] + rng.normal(scale=0.8, size=4000) > 1.2).astype(np.int8)
    model = XGBClassifier(n_estimators=300, max_depth=6, learning_rate=0.08, random_state=7).fit(x, y)
    onnx_bytes = _tree_model(model, "xgboost").SerializeToString()
    probe = rng.normal(size=(5000, 13)).astype(np.float32)

    def scores(threads: int) -> np.ndarray:
        opts = ort.SessionOptions()
        opts.intra_op_num_threads = threads
        sess = ort.InferenceSession(onnx_bytes, sess_options=opts, providers=["CPUExecutionProvider"])
        name = _probabilities_output(onnx_load(onnx_bytes))
        out = dict(zip([o.name for o in sess.get_outputs()], sess.run(None, {INPUT: probe}), strict=True))
        return np.asarray(out[name], dtype=np.float64)[:, 1]

    single, many = scores(1), scores(4)
    assert float(np.max(np.abs(single - many))) < 1e-7
    exact = model.predict_proba(probe)[:, 1].astype(np.float64)
    assert float(np.max(np.abs(single - exact))) < 5e-7


def onnx_load(model_bytes: bytes):  # type: ignore[no-untyped-def]
    import onnx

    return onnx.load_from_string(model_bytes)


def test_double_trees_handle_a_zero_margin_base_score() -> None:
    # onnxmltools leaves out base_values when the base margin is 0 (base_score 0.5); the rewrite must not need it.
    import onnxruntime as ort
    from xgboost import XGBClassifier

    from training.export_onnx import INPUT, _probabilities_output, _tree_model

    rng = np.random.default_rng(3)
    x = rng.normal(size=(2000, 13)).astype(np.float32)
    y = (x[:, 0] > 0.3).astype(np.int8)
    model = XGBClassifier(n_estimators=40, max_depth=3, base_score=0.5, random_state=7).fit(x, y)
    m = _tree_model(model, "xgboost")
    sess = ort.InferenceSession(m.SerializeToString(), providers=["CPUExecutionProvider"])
    out = dict(zip([o.name for o in sess.get_outputs()], sess.run(None, {INPUT: x[:500]}), strict=True))
    got = np.asarray(out[_probabilities_output(m)], dtype=np.float64)[:, 1]
    assert float(np.max(np.abs(got - model.predict_proba(x[:500])[:, 1]))) < 5e-7
