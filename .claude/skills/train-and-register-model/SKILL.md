---
name: train-and-register-model
description: Train, calibrate, export to ONNX and register a model version with an alias.
---
# Train and register

1. `make gen-data SIZE=demo` (or `eval`).
2. `make train` — XGBoost, isotonic calibration on valid, ONNX export, parity check within 1e-5.
3. `make register-model ALIAS=shadow` and enable shadow; review agreement on the console Shadow page.
4. Promote with `make register-model ALIAS=production`; record the decision in `docs/DECISIONS.md`.
5. `make eval-model` and check the gate.
