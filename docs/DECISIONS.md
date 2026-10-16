# Decisions log

Every ambiguous point in the specification is decided here: date, decision, alternatives, reason. Architectural decisions also have an ADR in `docs/adr/`.

| # | Date | Decision | Alternatives | Reason |
|---|---|---|---|---|
| D-001 | 2026-10-16 | The pre-seeded commit did not contain `.claude/settings.json` or `.claude/rules/git-commits.md`, so §1.1 steps 1–4 ran in full (commit `chore: configure commit attribution`). | Skip step 4 as for a fully pre-seeded repository. | The files were absent; the spec only allows skipping when both exist. |
| D-002 | 2026-10-16 | `evalgate` v0.1.0 is not tagged upstream; use a minimal compatible implementation in `third_party/evalgate/` (report.v1, check, calibrate subset) and list the switch under "Needs the maintainer". | Block on the dependency; vendor the upstream main branch (it only contains a spec). | §11 prescribes this fallback; upstream has no code yet. |
| D-003 | 2026-10-16 | Build the SageMaker `CreateTrainingJob` request with `boto3` only (no `sagemaker` SDK) and use the `xgboost-cpu` wheel. | `sagemaker` SDK v3 (pulls `torch` and CUDA wheels through `sagemaker-serve`, > 2 GB); `xgboost` (pulls `nvidia-nccl`). | The launcher only needs the request shape and a moto-mocked submission; GPU serving is out of scope (§3.3). |
| D-004 | 2026-10-16 | `buf lint` runs STANDARD except `RPC_REQUEST_RESPONSE_UNIQUE`, `RPC_RESPONSE_STANDARD_NAME` and `SERVICE_SUFFIX`. | Rename to `ScorerService`/`ScoreResponse`/`GetCaseResponse`. | §4.4 fixes the service and message names (`Scorer`, `Decision`, `Case`, …) that clients depend on; every other STANDARD rule applies. |
| D-005 | 2026-10-16 | Add table `account_events` (event_id PK, account_id, kind, event_ts, payload jsonb), written in batches by the feature-updater. | Read events from Redis; read from Kafka on demand. | §4.3 and §13 need "last 50 events" and an account timeline from Postgres; §4.5 lists no events table. Retention 90 days. |
| D-006 | 2026-10-16 | `buf breaking` compares against `.git#branch=main` only when `main` already contains `proto/`; the first introduction is reported as "no baseline". | Fail when the baseline is empty. | `main` holds only the scaffold until this branch merges. |
| D-007 | 2026-10-16 | Add `Transaction.merchant_name = 12` (additive) and optional `merchant_name` in `fraud.event.transaction.v1`. | Keep merchant names out of the system. | §10 and §15 require injection fixtures in merchant names reaching the summarizer; the field is never a model or rule input. |
