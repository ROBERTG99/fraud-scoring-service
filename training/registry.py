"""MLflow tracking and model registry for `fraud-gbdt` (spec FR-5).

    uv run python -m training.registry --model-dir artifacts/model --alias production

The ONNX graph and model_meta.json are logged as run artifacts; the registered model version points at
them and carries the headline metrics as tags. Aliases `production` and `shadow` select what the
model server loads.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
from pathlib import Path
from typing import Any

MODEL_NAME = os.environ.get("FRAUD_MODEL_NAME", "fraud-gbdt")
EXPERIMENT = "fraud-gbdt"
HEADLINE = ("pr_auc", "recall_at_1pct_fpr", "ece", "roc_auc")


def tracking_uri() -> str:
    """MLFLOW_TRACKING_URI, else a local SQLite store in ./mlruns (MLflow 3 retired the plain file store)."""
    uri = os.environ.get("MLFLOW_TRACKING_URI")
    if uri:
        return uri
    Path("mlruns").mkdir(exist_ok=True)
    return "sqlite:///" + str(Path("mlruns/mlflow.db").resolve())


def _artifact_location(uri: str) -> str | None:
    if uri.startswith("sqlite:///"):
        return (Path(uri.removeprefix("sqlite:///")).parent / "artifacts").resolve().as_uri()
    return None


def _mlflow() -> Any:
    import mlflow

    uri = tracking_uri()
    mlflow.set_tracking_uri(uri)
    mlflow.set_registry_uri(uri)
    return mlflow


def _client() -> Any:
    from mlflow.tracking import MlflowClient

    _mlflow()
    return MlflowClient()


def log_and_register(model_dir: Path, meta: dict[str, Any]) -> dict[str, Any]:
    """Log a training run with its artifacts and register a new model version."""
    mlflow = _mlflow()
    client = _client()
    if client.get_experiment_by_name(EXPERIMENT) is None:
        client.create_experiment(EXPERIMENT, artifact_location=_artifact_location(tracking_uri()))
    mlflow.set_experiment(EXPERIMENT)
    with mlflow.start_run(run_name=str(meta["model_version"])) as run:
        mlflow.log_params({k: v for k, v in meta.get("params", {}).items()})
        mlflow.log_param("backend", meta.get("backend"))
        mlflow.log_param("feature_spec", meta.get("feature_spec", "v1"))
        for split in ("valid", "test"):
            for key, value in meta.get(split, {}).items():
                mlflow.log_metric(f"{split}_{key}", float(value))
        mlflow.log_artifact(str(model_dir / "model.onnx"), artifact_path="model")
        mlflow.log_artifact(str(model_dir / "model_meta.json"), artifact_path="model")
        run_id = run.info.run_id
    try:
        client.create_registered_model(MODEL_NAME)
    except Exception as exc:  # already exists
        if "already exists" not in str(exc).lower() and "RESOURCE_ALREADY_EXISTS" not in str(exc):
            raise
    source = f"runs:/{run_id}/model"
    mv = client.create_model_version(MODEL_NAME, source=source, run_id=run_id)
    tags = {k: str(meta["test"][k]) for k in HEADLINE if k in meta.get("test", {})}
    tags |= {"model_version": str(meta["model_version"]), "onnx_sha256": str(meta.get("onnx_sha256", "")), "backend": str(meta.get("backend", ""))}
    for key, value in tags.items():
        client.set_model_version_tag(MODEL_NAME, mv.version, key, value)
    return {"name": MODEL_NAME, "version": str(mv.version), "run_id": run_id, "source": source}


def describe(version: str) -> dict[str, Any]:
    mv = _client().get_model_version(MODEL_NAME, version)
    return {"version": str(mv.version), "tags": dict(mv.tags), "run_id": mv.run_id, "aliases": list(getattr(mv, "aliases", []))}


def set_alias(alias: str, version: str) -> None:
    _client().set_registered_model_alias(MODEL_NAME, alias, version)


def version_for_alias(alias: str) -> str:
    return str(_client().get_model_version_by_alias(MODEL_NAME, alias).version)


def download_alias(alias: str, dest: Path) -> Path:
    """Download model.onnx and model_meta.json for an alias into dest; return dest."""
    client = _client()
    mv = client.get_model_version_by_alias(MODEL_NAME, alias)
    dest.mkdir(parents=True, exist_ok=True)
    tmp = Path(client.download_artifacts(mv.run_id, "model", str(dest)))
    for name in ("model.onnx", "model_meta.json"):
        src = tmp / name
        if src.resolve() != (dest / name).resolve():
            shutil.copyfile(src, dest / name)
    meta = json.loads((dest / "model_meta.json").read_text(encoding="utf-8"))
    meta["registry"] = {"name": MODEL_NAME, "version": str(mv.version), "alias": alias}
    (dest / "model_meta.json").write_text(json.dumps(meta, indent=2), encoding="utf-8")
    return dest


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--model-dir", type=Path, default=Path("artifacts/model"))
    ap.add_argument("--alias", choices=["production", "shadow"], required=True)
    ap.add_argument("--version", help="model version (default: the one recorded by make train)")
    args = ap.parse_args(argv)
    version = args.version
    if version is None:
        meta = json.loads((args.model_dir / "model_meta.json").read_text(encoding="utf-8"))
        if "registered" not in meta:
            meta["registered"] = log_and_register(args.model_dir, meta)
            (args.model_dir / "model_meta.json").write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8")
        version = str(meta["registered"]["version"])
    set_alias(args.alias, version)
    print(f"registry: {MODEL_NAME} v{version} -> @{args.alias}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
