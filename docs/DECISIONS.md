# Decisions log

Every ambiguous point in the specification is decided here: date, decision, alternatives, reason. Architectural decisions also have an ADR in `docs/adr/`.

| # | Date | Decision | Alternatives | Reason |
|---|---|---|---|---|
| D-001 | 2026-10-16 | The pre-seeded commit did not contain `.claude/settings.json` or `.claude/rules/git-commits.md`, so §1.1 steps 1–4 ran in full (commit `chore: configure commit attribution`). | Skip step 4 as for a fully pre-seeded repository. | The files were absent; the spec only allows skipping when both exist. |
| D-002 | 2026-10-16 | `evalgate` v0.1.0 is not tagged upstream; use a minimal compatible implementation in `third_party/evalgate/` (report.v1, check, calibrate subset) and list the switch under "Needs the maintainer". | Block on the dependency; vendor the upstream main branch (it only contains a spec). | §11 prescribes this fallback; upstream has no code yet. |
| D-003 | 2026-10-16 | Build the SageMaker `CreateTrainingJob` request with `boto3` only (no `sagemaker` SDK) and use the `xgboost-cpu` wheel. | `sagemaker` SDK v3 (pulls `torch` and CUDA wheels through `sagemaker-serve`, > 2 GB); `xgboost` (pulls `nvidia-nccl`). | The launcher only needs the request shape and a moto-mocked submission; GPU serving is out of scope (§3.3). |
