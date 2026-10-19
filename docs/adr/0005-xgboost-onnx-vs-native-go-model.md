# ADR 0005: An XGBoost model exported to ONNX, not a model evaluated natively in Go

## Status

Accepted.

## Date

2026-10-18

## Context

The decision model is a gradient-boosted tree classifier over the 13 features of `features/spec.yaml`,
followed by isotonic calibration, because the combination table (ADR 0008) compares a calibrated probability
with fixed thresholds. It is trained in Python (`training/train.py`) and must give the same calibrated score in
every place that uses it: the training check, the BentoML model server, the optional Triton server, and the
offline model eval. The scorer is written in Go (ADR 0002). The spec (FR-5, §5) asks for XGBoost with a
HistGradientBoosting fallback, isotonic calibration on the valid split, ONNX export, and rejects a native Go
model.

## Decision

- **The model is XGBoost.** `fit_model` trains an `XGBClassifier` with 300 trees, depth 6 and
  `tree_method: hist` (`training/train.py:32-43`, `:86-92`) from the `xgboost-cpu` wheel (docs/DECISIONS.md
  D-003). scikit-learn's `HistGradientBoostingClassifier` is used only if xgboost cannot be imported
  (`training/train.py:93-101`). The version the evals measure, `fraud-gbdt/3`, records `backend: xgboost` in its
  `model_meta.json` and in the registry's `backend` tag (`training/registry.py:79-86`).
- **Calibration is part of the model.** Isotonic regression is fit on the valid split and post-processed so
  the map's slope is at most 10 (`training/train.py:107-142`, D-008), then emitted as ONNX nodes after the trees
  (`training/export_onnx.py:51-106`). Every ONNX runtime therefore returns the calibrated probability.
- **One artifact.** `model.onnx` plus `model_meta.json` (feature spec, ONNX SHA-256, metrics, golden vector).
  Training refuses an export whose scores differ from the native model by more than 1e-5
  (`training/train.py:213-220`). Serving verifies the digest before loading (D-047).
- **The scorer never runs the model.** It calls a model server over HTTP (`internal/model`, ADR 0004).

## Alternatives considered

- **A native Go evaluator over the XGBoost model dump.** No network hop and no Python server on the scoring
  path. But calibration would need a second implementation, every tree format change would need Go work, and
  parity with training would rest on a hand-written evaluator.
- **onnxruntime in the Go process through cgo.** Keeps one artifact and removes the hop, but the scorer would
  no longer be a static binary on a distroless base (`Dockerfile.go`), and a native library crash would take
  the scorer down with it.
- **A linear model computed in Go.** Trivial to embed, but not the tabular GBDT the spec asks for.

## Consequences

Positive:

- Native and ONNX scores agree within 7.14e-06 on demo data (`python -m training.train`, `PROGRESS.md` T-14);
  5 golden vectors agree within 1e-5 through the Go client for both aliases (`TestGoldenVectorScore`,
  `PROGRESS.md` T-17).
- The same file is what the eval measures: `make eval-model` scores the serving artifact, giving PR-AUC 0.8740
  and ECE 0.0007 on the 169,604-transaction test split (`evals/REPORT.md`, D-033).
- A new model ships by registering a version and moving an alias (`make register-model ALIAS=production`); the
  scorer is not rebuilt.
- The calibrated score comes out of Triton too, which has no Python post-processing step (D-008).

Negative:

- The model is a network hop and a Python process away from the scorer, inside the 20 ms budget. When the model
  server is slow, decisions degrade to rules-only; that is the main source of degraded decisions on a loaded
  host (`docs/KNOWN_LIMITATIONS.md`, "Model server throughput on a shared host").
- Float32 inference forced the calibration slope cap: raw isotonic breakpoints 1e-8 apart gave 3.6e-3 output
  differences on demo data (D-008). The cap slightly smooths very steep steps of the map.
- Parity is checked on demo data only; the M5 ML review saw a few eval-data rows just over 1e-5, though no
  decision changes at the thresholds (`docs/KNOWN_LIMITATIONS.md`, "Data and model").
- The export depends on converters (`onnxmltools` for XGBoost, `skl2onnx` for the fallback). The
  HistGradientBoosting path is excluded from coverage (`# pragma: no cover`); every version in the local
  registry (`mlruns/mlflow.db`, git-ignored, versions 1 to 3) is tagged `backend: xgboost`.
