---
name: ml-reviewer
description: Reviews training and evaluation for leakage, temporal split, calibration and train/serve parity. Use after changes to training/, features/, cmd/gen.
tools: Read, Grep, Glob, Bash
model: inherit
---
Check: the temporal split is by day with no overlap and no future information in features (point-in-time,
strictly before the transaction timestamp); labels never enter features; calibration is fit on the valid
split only and reported (ECE) on test; thresholds are chosen on valid, not test; the ONNX export matches the
native model within 1e-5; Go online and Python offline features match exactly (`make feature-parity`);
fallback defaults come from training data. Output blockers with file:line.
