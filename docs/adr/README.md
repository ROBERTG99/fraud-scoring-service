# Architecture decision records

Each record states one architectural choice: its context, the decision, the alternatives considered and the
consequences, with references to the code and to the rows of `docs/DECISIONS.md` that refine it. The system
they shape is described in `docs/architecture.md`.

| ADR | Title | Status | Date |
|---|---|---|---|
| [0001](0001-feature-spec-v2-first-seen-device.md) | Feature spec v2, "new device" means first seen less than 24 h ago | Accepted | 2026-10-17 |
| [0002](0002-go-grpc-scorer-vs-python.md) | The scorer is a Go gRPC service, not a Python one | Accepted | 2026-10-18 |
| [0003](0003-kafka-vs-sqs.md) | Kafka protocol (Redpanda) for events and decisions, not SQS | Accepted | 2026-10-18 |
| [0004](0004-bentoml-default-vs-triton-default.md) | BentoML is the default model server; Triton is optional | Accepted | 2026-10-18 |
| [0005](0005-xgboost-onnx-vs-native-go-model.md) | An XGBoost model exported to ONNX, not a model evaluated natively in Go | Accepted | 2026-10-18 |
| [0006](0006-llm-off-the-hot-path.md) | The LLM writes case summaries asynchronously and never touches a decision | Accepted | 2026-10-18 |
| [0007](0007-group-commit-plus-outbox.md) | Group commit before responding, an outbox for publishing, a disk spool for outages | Accepted | 2026-10-18 |
| [0008](0008-rules-and-model-table-vs-single-score.md) | Decisions come from a rules-and-model combination table, not a single score | Accepted | 2026-10-18 |
| [0009](0009-scorer-writes-its-own-transaction-to-redis.md) | The scorer adds its own transaction to Redis instead of waiting for the feature-updater | Accepted | 2026-10-18 |

Later refinements carry their own dates in `docs/DECISIONS.md`.

New records use the `new-adr` skill (`.claude/skills/new-adr/`) and take the next free number.
