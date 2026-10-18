"""Model runtime behind the BentoML service: ONNX sessions per alias, loaded from MLflow or a local dir.

Load order per alias (spec §4.7, MLflow row):
1. MLflow registry `models:/fraud-gbdt@<alias>` (downloaded into the cache dir),
2. the last cached artifact in FRAUD_MODEL_CACHE/<alias>,
3. FRAUD_MODEL_DIR (a local `make train` output; used for both aliases),
otherwise the alias is not ready and /readyz fails.

Every source is checked against the SHA-256 recorded for it before an InferenceSession is built (audit M6 M6),
and a model with no recorded digest at all is refused (round 2, D-059):
`FRAUD_MODEL_SHA256_<ALIAS>` when the deployment pins one, the registry's `onnx_sha256` version tag for an
MLflow download (required), and the `onnx_sha256` that training wrote into model_meta.json.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import shutil
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


def _session(model: bytes) -> ort.InferenceSession:
    opts = ort.SessionOptions()
    opts.intra_op_num_threads = 1
    opts.inter_op_num_threads = 1
    opts.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    return ort.InferenceSession(model, sess_options=opts, providers=["CPUExecutionProvider"])


def _check_digest(alias: str, model_dir: Path, model: bytes, source: str) -> None:
    """Refuse model bytes that do not match a recorded SHA-256 (the bytes hashed are the bytes loaded)."""
    digest = hashlib.sha256(model).hexdigest()
    meta_path = model_dir / "model_meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8")) if meta_path.is_file() else {}
    expected = {
        f"FRAUD_MODEL_SHA256_{alias.upper()}": os.environ.get(f"FRAUD_MODEL_SHA256_{alias.upper()}"),
        "registry tag onnx_sha256": (meta.get("registry") or {}).get("onnx_sha256"),
        "model_meta.json onnx_sha256": meta.get("onnx_sha256"),
    }
    if source == "mlflow" and not expected["registry tag onnx_sha256"]:
        raise ModelNotReadyError("registry version has no onnx_sha256 tag; refusing an unverifiable model")
    if not any(expected.values()):  # every source needs one: deleting model_meta.json must not skip the check
        raise ModelNotReadyError(
            "no recorded sha256 for model.onnx (pin one or keep model_meta.json); refusing it"
        )
    for where, want in expected.items():
        if want and want.lower() != digest:
            raise ModelNotReadyError(f"model.onnx sha256 {digest[:12]} does not match {where} {want[:12]}")


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


# The feature spec the scorer computes (features/spec.yaml `version`). A model trained on another spec would
# score the wrong inputs, so it is refused: the server stays not-ready and the scorer degrades to rules-only.
EXPECTED_FEATURE_SPEC = os.environ.get("FRAUD_FEATURE_SPEC_VERSION", "v2")


def _check_feature_spec(model_dir: Path) -> None:
    meta_path = model_dir / "model_meta.json"
    if not meta_path.is_file():
        return  # bare test fixtures carry no metadata
    spec = json.loads(meta_path.read_text(encoding="utf-8")).get("feature_spec")
    if spec != EXPECTED_FEATURE_SPEC:
        raise ModelNotReadyError(
            f"model trained on feature spec {spec!r}, server expects {EXPECTED_FEATURE_SPEC!r}"
        )


class ModelRuntime:
    def __init__(
        self, model_dir: str | None = None, cache_dir: str | None = None, use_mlflow: bool | None = None
    ) -> None:
        self.model_dir = Path(model_dir) if model_dir else None
        # Not a fixed /tmp path, which another local user could pre-create or symlink (bandit B108).
        default_cache = Path(os.environ.get("XDG_CACHE_HOME") or Path.home() / ".cache") / "fraud-model"
        self.cache_dir = Path(cache_dir or os.environ.get("FRAUD_MODEL_CACHE") or default_cache)
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
            # Downloaded into a staging dir and verified there, so a bad artifact never replaces the cache.
            staging = self.cache_dir / f".{alias}.download"
            try:
                from training import registry

                shutil.rmtree(staging, ignore_errors=True)
                registry.download_alias(alias, staging)
                loaded = self._install(alias, staging, "mlflow")
                cached.mkdir(parents=True, exist_ok=True)
                for name in ("model.onnx", "model_meta.json"):
                    os.replace(staging / name, cached / name)
                return loaded
            except Exception as exc:  # MLflow down, alias missing or the artifact failed its checks
                errors.append(f"mlflow: {exc}")
                log.warning("registry model for @%s not loaded: %s", alias, exc)
            finally:
                shutil.rmtree(staging, ignore_errors=True)
        if (cached / "model.onnx").is_file():
            return self._install(alias, cached, "cache")
        if self.model_dir and (self.model_dir / "model.onnx").is_file():
            return self._install(alias, self.model_dir, "local")
        raise ModelNotReadyError("; ".join(errors) or "no model source configured")

    def _install(self, alias: str, model_dir: Path, source: str) -> LoadedModel:
        _check_feature_spec(model_dir)
        model = (model_dir / "model.onnx").read_bytes()
        _check_digest(alias, model_dir, model, source)
        loaded = LoadedModel(
            alias=alias,
            version=_version_from_meta(model_dir),
            source=source,
            session=_session(model),
        )
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
