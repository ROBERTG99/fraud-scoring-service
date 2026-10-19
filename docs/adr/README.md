# Architecture decision records

Each record states one architectural choice: its context, the decision, the alternatives considered and the
consequences, with references to the code and to the rows of `docs/DECISIONS.md` that refine it. The system
they shape is described in `docs/architecture.md`.

ADR 0001 predates the specification's list: it was written during the build, when feature spec v2 changed the
model input contract (D-026), before any of the eight ADRs the specification asks for. The specification
(`docs/specs/000-build-spec.md` §18) numbers those eight 0001 to 0008; here they are 0002 to 0009, in the
specification's order, so that ADR 0001 and the references to it keep their number (D-070).

| ADR | Title | Spec §18 number | Status | Date |
|---|---|---|---|---|
| [0001](0001-feature-spec-v2-first-seen-device.md) | Feature spec v2, "new device" means first seen less than 24 h ago | none | Accepted | 2026-10-17 |
| [0002](0002-go-grpc-scorer-vs-python.md) | The scorer is a Go gRPC service, not a Python one | 0001 | Accepted | 2026-10-18 |
| [0003](0003-kafka-vs-sqs.md) | Kafka protocol (Redpanda) for events and decisions, not SQS | 0002 | Accepted | 2026-10-18 |
| [0004](0004-bentoml-default-vs-triton-default.md) | BentoML is the default model server; Triton is optional | 0003 | Accepted | 2026-10-18 |
| [0005](0005-xgboost-onnx-vs-native-go-model.md) | An XGBoost model exported to ONNX, not a model evaluated natively in Go | 0004 | Accepted | 2026-10-18 |
| [0006](0006-llm-off-the-hot-path.md) | The LLM writes case summaries asynchronously and never touches a decision | 0005 | Accepted | 2026-10-18 |
| [0007](0007-group-commit-plus-outbox.md) | Group commit before responding, an outbox for publishing, a disk spool for outages | 0006 | Accepted | 2026-10-18 |
| [0008](0008-rules-and-model-table-vs-single-score.md) | Decisions come from a rules-and-model combination table, not a single score | 0007 | Accepted | 2026-10-18 |
| [0009](0009-scorer-writes-its-own-transaction-to-redis.md) | The scorer adds its own transaction to Redis instead of waiting for the feature-updater | 0008 | Accepted | 2026-10-18 |

ADRs 0002 to 0009 record choices the specification made before the build (§5, §18); their date is the day the
records were written. Later refinements carry their own dates in `docs/DECISIONS.md`.

New records use the `new-adr` skill (`.claude/skills/new-adr/`) and take the next free number.
