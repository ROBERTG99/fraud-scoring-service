"""Train, calibrate and export the fraud GBDT (spec FR-5).

    uv run python -m training.train --data artifacts/data/demo --out artifacts/model

XGBoost (fallback: scikit-learn HistGradientBoosting) on the train split, isotonic calibration on the
valid split, metrics on valid and test, ONNX export verified against the native model within 1e-5,
then MLflow tracking and registration of a `fraud-gbdt` version (training/registry.py).
"""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
import numpy.typing as npt
from sklearn.isotonic import IsotonicRegression

from training import metrics
from training.dataset import load_events, load_labels
from training.export_onnx import export, run_onnx
from training.features import SPEC_PATH, compute_features, feature_names

log = logging.getLogger("training")

XGB_PARAMS: dict[str, Any] = {
    "n_estimators": 300,
    "max_depth": 6,
    "learning_rate": 0.08,
    "subsample": 0.8,
    "colsample_bytree": 0.9,
    "min_child_weight": 2.0,
    "tree_method": "hist",
    "eval_metric": "aucpr",
    "random_state": 7,
    "n_jobs": 4,
}
TINY_PARAMS: dict[str, Any] = {**XGB_PARAMS, "n_estimators": 20, "max_depth": 3}
FALLBACK_FEATURES = [
    "tx_count_10m",
    "tx_count_1h",
    "tx_count_24h",
    "amount_sum_1h_minor",
    "distinct_devices_24h",
    "new_device",
    "minutes_since_pwd_change",
    "minutes_since_email_change",
    "mcc_novel_30d",
]
F32 = npt.NDArray[np.float32]
F64 = npt.NDArray[np.float64]


@dataclass
class Prepared:
    x: F64
    y: npt.NDArray[np.int8]
    split: npt.NDArray[np.str_]
    pattern: npt.NDArray[np.str_]
    transaction_id: list[str]
    ts: npt.NDArray[np.int64]


def prepare(data_dir: Path) -> Prepared:
    ev = load_events(data_dir / "events.jsonl")
    labels = load_labels(data_dir / "labels.csv")
    fm = compute_features(ev)
    pos = {tx: i for i, tx in enumerate(labels.transaction_id)}
    order = np.asarray([pos[tx] for tx in fm.transaction_id], dtype=np.int64)
    return Prepared(
        x=fm.values,
        y=labels.is_fraud[order],
        split=np.asarray(labels.split)[order],
        pattern=np.asarray(labels.pattern)[order],
        transaction_id=fm.transaction_id,
        ts=ev.ts[fm.event_index],
    )


def fit_model(x: F64, y: npt.NDArray[np.int8], params: dict[str, Any]) -> tuple[Any, str]:
    try:
        from xgboost import XGBClassifier

        model = XGBClassifier(**params)
        model.fit(x.astype(np.float32), y)
        return model, "xgboost"
    except ImportError:  # pragma: no cover - exercised only without xgboost
        from sklearn.ensemble import HistGradientBoostingClassifier

        hgb = HistGradientBoostingClassifier(
            max_iter=params["n_estimators"], max_depth=params["max_depth"], random_state=7
        )
        hgb.fit(x.astype(np.float32), y)
        return hgb, "sklearn_hgb"


def raw_scores(model: Any, x: F64) -> F64:
    return np.asarray(model.predict_proba(x.astype(np.float32))[:, 1], dtype=np.float64)


def fit_isotonic(raw: F64, y: npt.NDArray[np.int8]) -> IsotonicRegression:
    iso = IsotonicRegression(y_min=0.0, y_max=1.0, out_of_bounds="clip", increasing=True)
    iso.fit(raw, y)
    return iso


MAX_CALIBRATION_SLOPE = 10.0


def bounded_slope(x: F64, y: F64, max_slope: float = MAX_CALIBRATION_SLOPE) -> tuple[F64, F64]:
    """Spread near-vertical isotonic steps so the map's slope is at most max_slope (docs/DECISIONS.md D-008).

    Isotonic regression can place two breakpoints 1e-8 apart; float32 rounding in the ONNX runtime then
    moves outputs by up to 1e-3. Moving each breakpoint right just enough keeps the map monotone and
    Lipschitz, so native and ONNX scores agree within 1e-5.
    """
    upper = max(1.0, float(x[-1]))
    gap = [max((y[i + 1] - y[i]) / max_slope, 1e-9) for i in range(len(x) - 1)]
    xs = [float(v) for v in x]
    for i in range(1, len(xs)):  # forward: push right
        xs[i] = max(xs[i], xs[i - 1] + gap[i - 1])
    if xs[-1] > upper:  # out of room near 1: pull left from the upper bound
        xs[-1] = upper
        for i in range(len(xs) - 2, -1, -1):
            xs[i] = min(xs[i], xs[i + 1] - gap[i])
    return np.asarray(xs), np.asarray(y, dtype=np.float64)


@dataclass
class Calibration:
    x: F64
    y: F64

    def __call__(self, raw: F64) -> F64:
        return np.interp(raw, self.x, self.y)


def calibration_from(iso: IsotonicRegression) -> Calibration:
    xs, ys = bounded_slope(np.asarray(iso.X_thresholds_), np.asarray(iso.y_thresholds_))
    return Calibration(xs, ys)


def calibrated(model: Any, cal: Calibration, x: F64) -> F64:
    # np.interp over the breakpoints is the exact function the ONNX graph implements.
    return cal(raw_scores(model, x))


def fallback_defaults(x_train: F64) -> dict[str, int]:
    names = feature_names()
    p90 = np.percentile(x_train, 90, axis=0)
    return {n: int(np.ceil(p90[names.index(n)])) for n in FALLBACK_FEATURES}


def update_spec_defaults(defaults: dict[str, int], spec_path: Path = SPEC_PATH) -> None:
    text = spec_path.read_text(encoding="utf-8")
    head, _, _ = text.partition("fallback_defaults:\n")
    body = "".join(f"  {k}: {v}\n" for k, v in defaults.items())
    spec_path.write_text(head + "fallback_defaults:\n" + body, encoding="utf-8")


def split_metrics(y: npt.NDArray[np.int8], p: F64) -> dict[str, float]:
    recall, fpr, threshold = metrics.recall_at_fpr(y, p, 0.01)
    return {
        "pr_auc": metrics.pr_auc(y, p),
        "roc_auc": metrics.roc_auc(y, p),
        "recall_at_1pct_fpr": recall,
        "fpr_at_operating_point": fpr,
        "threshold_at_1pct_fpr": threshold,
        "ece": metrics.expected_calibration_error(y, p),
        "n": float(len(y)),
        "positives": float(int(y.sum())),
    }


def golden_vector() -> list[float]:
    """The journey transaction (§3.4): USD 940 online, new device, email changed 2 minutes ago."""
    return [0, 1, 3, 4200, 94000, 2, 1, 43200, 2, 1, 2, 14, 9.4]


def train(data_dir: Path, out_dir: Path, tiny: bool = False) -> dict[str, Any]:
    t0 = time.perf_counter()
    prep = prepare(data_dir)
    t_feat = time.perf_counter() - t0
    tr, va, te = (prep.split == s for s in ("train", "valid", "test"))
    if not (tr.any() and va.any() and te.any()):
        raise ValueError("dataset needs non-empty train, valid and test splits")
    assert prep.ts[tr].max() < prep.ts[va].min() <= prep.ts[va].max() < prep.ts[te].min(), (
        "temporal split overlap"
    )
    params = TINY_PARAMS if tiny else XGB_PARAMS
    model, backend = fit_model(prep.x[tr], prep.y[tr], params)
    cal = calibration_from(fit_isotonic(raw_scores(model, prep.x[va]), prep.y[va]))
    p_valid = calibrated(model, cal, prep.x[va])
    p_test = calibrated(model, cal, prep.x[te])
    result: dict[str, Any] = {
        "backend": backend,
        "params": params,
        "valid": split_metrics(prep.y[va], p_valid),
        "test": split_metrics(prep.y[te], p_test),
        "train_rows": int(tr.sum()),
        "feature_names": feature_names(),
        "fallback_defaults": fallback_defaults(prep.x[tr]),
    }

    tree_bytes = export(model, backend, cal.x, cal.y, "pending").SerializeToString()
    version = "gbdt-" + hashlib.sha256(tree_bytes).hexdigest()[:10]
    onnx_model = export(model, backend, cal.x, cal.y, version)
    onnx_bytes = onnx_model.SerializeToString()
    check = np.concatenate([prep.x[va], prep.x[te]])
    native = calibrated(model, cal, check)
    via_onnx = run_onnx(onnx_bytes, check.astype(np.float32)).astype(np.float64)
    max_diff = float(np.max(np.abs(native - via_onnx)))
    if max_diff > 1e-5:
        raise AssertionError(f"ONNX differs from the native model by {max_diff:.2e} (> 1e-5)")
    result["onnx_max_abs_diff"] = max_diff

    golden = np.asarray([golden_vector()], dtype=np.float32)
    golden_score = float(run_onnx(onnx_bytes, golden)[0])
    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / "model.onnx").write_bytes(onnx_bytes)
    onnx_sha = hashlib.sha256(onnx_bytes).hexdigest()
    meta = {
        "model_version": version,
        "onnx_sha256": onnx_sha,
        "feature_spec": "v1",
        "calibration": {"method": "isotonic, slope <= 10", "x": cal.x.tolist(), "y": cal.y.tolist()},
        "golden": {"features": golden_vector(), "probability": golden_score},
        "data_dir": str(data_dir),
        **result,
        "seconds": {"features": round(t_feat, 2), "total": round(time.perf_counter() - t0, 2)},
    }
    (out_dir / "model_meta.json").write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8")
    log.info(
        "trained %s in %.1fs: test pr_auc=%.3f recall@1%%fpr=%.3f ece=%.4f",
        version,
        meta["seconds"]["total"],
        result["test"]["pr_auc"],
        result["test"]["recall_at_1pct_fpr"],
        result["test"]["ece"],
    )
    return meta


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--data", type=Path, default=Path("artifacts/data/demo"))
    ap.add_argument("--out", type=Path, default=Path("artifacts/model"))
    ap.add_argument("--tiny", action="store_true", help="20 shallow trees (test fixture)")
    ap.add_argument(
        "--update-spec", action="store_true", help="write fallback_defaults into features/spec.yaml"
    )
    ap.add_argument("--no-mlflow", action="store_true")
    args = ap.parse_args(argv)
    logging.basicConfig(level=logging.INFO, format="%(message)s")
    start = time.perf_counter()
    meta = train(args.data, args.out, tiny=args.tiny)
    if args.update_spec:
        update_spec_defaults(meta["fallback_defaults"])
    if not args.no_mlflow:
        from training.registry import log_and_register

        reg = log_and_register(args.out, meta)
        meta["registered"] = reg
        (args.out / "model_meta.json").write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8")
    elapsed = time.perf_counter() - start
    print(
        f"train: {meta['model_version']} in {elapsed:.1f}s | test pr_auc={meta['test']['pr_auc']:.4f} "
        f"recall@1%fpr={meta['test']['recall_at_1pct_fpr']:.4f} ece={meta['test']['ece']:.4f} "
        f"onnx_max_abs_diff={meta['onnx_max_abs_diff']:.2e}"
        + (f" | registered fraud-gbdt v{meta['registered']['version']}" if "registered" in meta else "")
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
