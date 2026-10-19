"""SageMaker training container entrypoint (training/Dockerfile).

SageMaker mounts the `train` channel at /opt/ml/input/data/train and uploads /opt/ml/model to the job's
S3 output path. Hyperparameters arrive as strings in /opt/ml/input/config/hyperparameters.json.
MLflow is off inside the job (network isolation); register the artifact afterwards with
`make register-model` (docs/runbook.md).
"""

from __future__ import annotations

import json
import os
from pathlib import Path

from training import train

PREFIX = Path(os.environ.get("SM_PREFIX", "/opt/ml"))


def hyperparameters(prefix: Path = PREFIX) -> dict[str, str]:
    path = prefix / "input" / "config" / "hyperparameters.json"
    if not path.exists():
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    return {str(k): str(v) for k, v in data.items()}


def argv(prefix: Path = PREFIX) -> list[str]:
    hp = hyperparameters(prefix)
    data = os.environ.get("SM_CHANNEL_TRAIN", str(prefix / "input" / "data" / "train"))
    out = os.environ.get("SM_MODEL_DIR", str(prefix / "model"))
    args = ["--data", data, "--out", out]
    if hp.get("mlflow", "false").lower() != "true":
        args.append("--no-mlflow")
    if hp.get("tiny", "false").lower() == "true":
        args.append("--tiny")
    return args


def main() -> int:
    return train.main(argv())


if __name__ == "__main__":
    raise SystemExit(main())
