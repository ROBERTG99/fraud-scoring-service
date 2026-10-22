# ADR 0004: BentoML is the default model server; Triton is optional

## Status

Accepted.

## Date

2026-10-18

## Context

The scorer needs a calibrated probability for one 13-feature row within 20 ms, for two aliases:
`production`, which decides, and `shadow`, which is only compared (see ADR 0008). The model is one ONNX graph
(ADR 0005) registered in MLflow as `fraud-gbdt` with aliases. Serving is CPU only; GPU serving is out of
scope. Everything must run offline, and `make verify` must never need
an optional component. The design calls for BentoML with onnxruntime by default and NVIDIA Triton
(KServe v2) as an option, both behind one Go interface.

## Decision

- **Default: BentoML.** `serving/bentoml/service.py` serves `POST /predict` (`{"inputs": [[13 floats]],
  "alias": ...}`) on port 8514 with `FRAUD_MODEL_WORKERS` workers (default 2, docs/DECISIONS.md D-024).
  `serving/bentoml/runtime.py` loads each alias from `models:/fraud-gbdt@<alias>`, else its cached copy, else
  `FRAUD_MODEL_DIR`, and verifies the ONNX bytes against every recorded SHA-256 before building an
  onnxruntime session (D-047, D-059, D-063). It refuses a model built for another feature spec (ADR 0001) and
  is ready only once `production` is loaded (`runtime.py:190-191`).
- **Option: Triton.** `FRAUD_MODEL_BACKEND=triton` switches the scorer to `internal/model/triton.go`, a KServe
  v2 client for the models `fraud_gbdt` and `fraud_gbdt_shadow` (`triton.go:29-34`). The model repository is
  `serving/triton/model_repository/`; `scripts/triton_repo.sh` copies the same ONNX files into it after
  checking each against its `model_meta.json`.
- **One interface.** Both clients implement `model.Client` (`internal/model/client.go:29-31`) and wrap every
  failure in `ErrUnavailable`, which the scorer turns into the rules-only path. A `Batcher` in front of either
  client coalesces concurrent single-row calls into one call per alias, at most 64 rows
  (`internal/model/batcher.go:21-25`, D-024).

## Alternatives considered

- **Triton by default.** A C++ server with an ONNX Runtime backend and server-side batching, but its image comes
  from `nvcr.io`, which this environment's network policy blocks (`docs/KNOWN_LIMITATIONS.md`, "Serving"), so the
  default stack could not have been built or tested here.
- **TorchServe.** Rejected; the model is not a PyTorch model.
- **A hand-written FastAPI server around onnxruntime.** Possible, but BentoML already provides workers,
  readiness, request validation and packaging.
- **onnxruntime inside the Go scorer.** See ADR 0005.

## Consequences

Positive:

- The default path runs and is tested here: 5 golden vectors through HTTP within 1e-6
  (`test_predict_golden_vector`), both aliases through the Go client within 1e-5 (`TestGoldenVectorScore`,
  `make bench-model`).
- Single-row latency: p50 2.99 ms, p95 4.63 ms, p99 6.65 ms over 2,000 requests, against the 8 ms model-server budget
  (`make bench-model`).
- Loading from MLflow aliases, the digest checks and the feature-spec check live in one Python module, which
  the model eval also uses (`make eval-model` loads the artifact through `ModelRuntime`, D-033).
- Swapping backends needs no scorer code change, and `make verify` never needs Triton.

Negative:

- BentoML is the throughput limit on a shared host. Without the batcher, 250 tx/s with shadow scoring degraded
  6,771 of 16,863 decisions to rules-only (D-024); with it, the CI bench passes (p95 19.6 ms at 300 rps,
  `evals/REPORT.md`), but the laptop profile reached 586 of 2,000 rps (`make bench BENCH_PROFILE=laptop`). Higher rates need
  more workers or Triton (`docs/KNOWN_LIMITATIONS.md`, "Model server throughput on a shared host").
- Shadow scoring doubles the load on the same server (D-024).
- Triton has never run in this repository. Only the request and response shapes are tested, against a
  recorded exchange (`TestKServeV2RequestShape`); `TestTritonGoldenVector` skips without a Triton URL. The
  `triton` compose profile is started by `make triton-up`.
- The Triton path lacks parts of the default one: it loads files from the model repository rather than MLflow
  aliases, its digest check runs once in `scripts/triton_repo.sh` rather than at load time, and
  `config.pbtxt` sets `max_batch_size: 0`, so batching still comes only from the Go `Batcher`.
