"""Model runtime behind the BentoML service: ONNX sessions per alias, loaded from MLflow or a local dir.

Load order per alias (spec §4.7, MLflow row):
1. MLflow registry `models:/fraud-gbdt@<alias>` (downloaded into the cache dir),
2. the last cached artifact in FRAUD_MODEL_CACHE/<alias>,
3. FRAUD_MODEL_DIR (a local `make train` output; used for both aliases),
otherwise the alias is not ready and /readyz fails.
"""

from __future__ import annotations

import json
import logging
import os
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
import onnxruntime as ort

N_FEATURES = 13
ALIASES = ("production", "shadow")
log = logging.getLogger("model-server")


class ModelNotReadyError(RuntimeError):
    pass


@dataclass
class LoadedModel:
    alias: str
    version: str
    source: str
    session: ort.InferenceSession

    def predict(self, rows: np.ndarray[Any, np.dtype[np.float32]]) -> list[float]:
        out = self.session.run(["probability"], {"features": rows})[0]
        return [float(v) for v in np.asarray(out, dtype=np.float32).reshape(-1)]


def _session(path: Path) -> ort.InferenceSession:
    opts = ort.SessionOptions()
    opts.intra_op_num_threads = 1
    opts.inter_op_num_threads = 1
    opts.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    return ort.InferenceSession(path.read_bytes(), sess_options=opts, providers=["CPUExecutionProvider"])


def _version_from_meta(model_dir: Path) -> str:
    meta_path = model_dir / "model_meta.json"
    if not meta_path.is_file():
        return model_dir.name
    meta = json.loads(meta_path.read_text(encoding="utf-8"))
    reg = meta.get("registry")
    if reg:
        return f"{reg['name']}/{reg['version']}"
    if "registered" in meta:
        return f"{meta['registered']['name']}/{meta['registered']['version']}"
    return str(meta.get("model_version", model_dir.name))


class ModelRuntime:
    def __init__(self, model_dir: str | None = None, cache_dir: str | None = None, use_mlflow: bool | None = None) -> None:
        self.model_dir = Path(model_dir) if model_dir else None
        self.cache_dir = Path(cache_dir or os.environ.get("FRAUD_MODEL_CACHE", "/tmp/fraud-model-cache"))  # noqa: S108
        self.use_mlflow = bool(os.environ.get("MLFLOW_TRACKING_URI")) if use_mlflow is None else use_mlflow
        self.models: dict[str, LoadedModel] = {}
        self._lock = threading.Lock()

    @classmethod
    def from_env(cls) -> ModelRuntime:
        return cls(model_dir=os.environ.get("FRAUD_MODEL_DIR"))

    def load_all(self) -> dict[str, str]:
        status = {}
        for alias in ALIASES:
            try:
                status[alias] = self.load(alias).version
            except ModelNotReadyError as exc:
                status[alias] = f"unavailable: {exc}"
        return status

    def load(self, alias: str) -> LoadedModel:
        errors: list[str] = []
        cached = self.cache_dir / alias
        if self.use_mlflow:
            # Fail fast when MLflow is down so the cached artifact takes over (spec §4.7).
            os.environ.setdefault("MLFLOW_HTTP_REQUEST_MAX_RETRIES", "1")
            os.environ.setdefault("MLFLOW_HTTP_REQUEST_BACKOFF_FACTOR", "0")
            os.environ.setdefault("MLFLOW_HTTP_REQUEST_TIMEOUT", "5")
            try:
                from training.registry import download_alias

                download_alias(alias, cached)
                return self._install(alias, cached, "mlflow")
            except Exception as exc:  # MLflow down or alias missing
                errors.append(f"mlflow: {exc}")
        if (cached / "model.onnx").is_file():
            return self._install(alias, cached, "cache")
        if self.model_dir and (self.model_dir / "model.onnx").is_file():
            return self._install(alias, self.model_dir, "local")
        raise ModelNotReadyError("; ".join(errors) or "no model source configured")

    def _install(self, alias: str, model_dir: Path, source: str) -> LoadedModel:
        loaded = LoadedModel(alias=alias, version=_version_from_meta(model_dir), source=source, session=_session(model_dir / "model.onnx"))
        with self._lock:
            self.models[alias] = loaded
        log.info("loaded %s as @%s from %s", loaded.version, alias, source)
        return loaded

    def ready(self) -> bool:
        return "production" in self.models

    def predict(self, inputs: list[list[float]], alias: str = "production") -> dict[str, Any]:
        if alias not in ALIASES:
            raise ValueError(f"alias must be one of {ALIASES}")
        model = self.models.get(alias)
        if model is None:
            raise ModelNotReadyError(f"alias {alias} not loaded")
        rows = np.asarray(inputs, dtype=np.float32)
        if rows.ndim != 2 or rows.shape[1] != N_FEATURES or rows.shape[0] == 0 or rows.shape[0] > 1024:
            raise ValueError(f"inputs must be 1..1024 rows of {N_FEATURES} floats")
        if not np.all(np.isfinite(rows)):
            raise ValueError("inputs must be finite")
        return {"scores": model.predict(rows), "model_version": model.version}
