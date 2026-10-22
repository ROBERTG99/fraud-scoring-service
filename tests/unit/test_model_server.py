"""BentoML model server."""

from __future__ import annotations

import hashlib
import json
import shutil
from pathlib import Path

import pytest
from starlette.testclient import TestClient

from serving.bentoml.runtime import ModelNotReadyError, ModelRuntime

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / "tests" / "fixtures" / "model"


@pytest.fixture(autouse=True)
def _dev_mode(monkeypatch: pytest.MonkeyPatch) -> None:
    """A local stack: model_meta.json's digest is enough. Outside dev a pin is required (tested below)."""
    monkeypatch.setenv("FRAUD_ENV", "dev")


@pytest.fixture
def model_dir(tmp_path: Path) -> Path:
    d = tmp_path / "model"
    d.mkdir()
    shutil.copyfile(FIXTURES / "tiny.onnx", d / "model.onnx")
    meta = json.loads((FIXTURES / "golden.json").read_text(encoding="utf-8"))
    digest = hashlib.sha256((d / "model.onnx").read_bytes()).hexdigest()
    (d / "model_meta.json").write_text(
        json.dumps({"model_version": meta["model_version"], "feature_spec": "v2", "onnx_sha256": digest}),
        encoding="utf-8",
    )
    return d


def golden() -> list[dict[str, object]]:
    return json.loads((FIXTURES / "golden.json").read_text(encoding="utf-8"))["vectors"]  # type: ignore[no-any-return]


def test_predict_golden_vector(model_dir: Path, tmp_path: Path) -> None:
    rt = ModelRuntime(model_dir=str(model_dir), cache_dir=str(tmp_path / "cache"), use_mlflow=False)
    rt.load_all()
    assert rt.ready()
    vectors = golden()
    out = rt.predict([v["features"] for v in vectors], "production")  # type: ignore[misc]
    for got, v in zip(out["scores"], vectors, strict=True):
        assert got == pytest.approx(v["probability"], abs=1e-6)
    assert out["model_version"].startswith("gbdt-")
    assert rt.predict([vectors[2]["features"]], "shadow")["scores"][0] == pytest.approx(
        vectors[2]["probability"], abs=1e-6
    )  # type: ignore[index]


def test_rejects_bad_inputs(model_dir: Path, tmp_path: Path) -> None:
    rt = ModelRuntime(model_dir=str(model_dir), cache_dir=str(tmp_path / "cache"), use_mlflow=False)
    rt.load_all()
    for bad in ([[1.0] * 12], [], [[float("nan")] * 13]):
        with pytest.raises(ValueError):
            rt.predict(bad)
    with pytest.raises(ValueError):
        rt.predict([[0.0] * 13], alias="canary")


def test_not_ready_without_model(tmp_path: Path) -> None:
    rt = ModelRuntime(model_dir=None, cache_dir=str(tmp_path / "empty"), use_mlflow=False)
    rt.load_all()
    assert not rt.ready()
    with pytest.raises(ModelNotReadyError):
        rt.predict([[0.0] * 13])


def test_uses_cached_artifact_when_mlflow_down(
    model_dir: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    cache = tmp_path / "cache"
    (cache / "production").mkdir(parents=True)
    shutil.copyfile(model_dir / "model.onnx", cache / "production" / "model.onnx")
    shutil.copyfile(model_dir / "model_meta.json", cache / "production" / "model_meta.json")
    monkeypatch.setenv("MLFLOW_TRACKING_URI", "http://127.0.0.1:9")  # nothing listens
    monkeypatch.setenv("MLFLOW_HTTP_REQUEST_MAX_RETRIES", "0")
    rt = ModelRuntime(model_dir=None, cache_dir=str(cache), use_mlflow=True)
    assert rt.load("production").source == "cache"


def test_http_predict(model_dir: Path, monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setenv("FRAUD_MODEL_DIR", str(model_dir))
    monkeypatch.setenv("FRAUD_MODEL_CACHE", str(tmp_path / "cache"))
    monkeypatch.delenv("MLFLOW_TRACKING_URI", raising=False)
    from serving.bentoml.service import FraudModel

    with TestClient(FraudModel.to_asgi()) as client:
        v = golden()[3]
        resp = client.post("/predict", json={"inputs": [v["features"]], "alias": "production"})
        assert resp.status_code == 200, resp.text
        body = resp.json()
        assert body["scores"][0] == pytest.approx(v["probability"], abs=1e-6)
        assert client.get("/readyz").status_code == 200
        assert client.post("/predict", json={"inputs": [[1.0] * 5]}).status_code == 400


def test_refuses_model_trained_on_another_feature_spec(model_dir: Path, tmp_path: Path) -> None:
    meta = json.loads((model_dir / "model_meta.json").read_text(encoding="utf-8"))
    (model_dir / "model_meta.json").write_text(json.dumps({**meta, "feature_spec": "v1"}), encoding="utf-8")
    rt = ModelRuntime(model_dir=str(model_dir), cache_dir=str(tmp_path / "cache"), use_mlflow=False)
    status = rt.load_all()
    assert not rt.ready()
    assert "feature spec 'v1'" in status["production"]


def _sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def test_refuses_a_model_file_whose_digest_differs_from_its_metadata(model_dir: Path, tmp_path: Path) -> None:
    """Security audit: a swapped model.onnx must not be served."""
    meta = json.loads((model_dir / "model_meta.json").read_text(encoding="utf-8"))
    (model_dir / "model_meta.json").write_text(
        json.dumps({**meta, "onnx_sha256": "0" * 64}), encoding="utf-8"
    )
    rt = ModelRuntime(model_dir=str(model_dir), cache_dir=str(tmp_path / "cache"), use_mlflow=False)
    status = rt.load_all()
    assert not rt.ready()
    assert "sha256" in status["production"]


def test_a_pinned_digest_is_enforced(
    model_dir: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    rt = ModelRuntime(model_dir=str(model_dir), cache_dir=str(tmp_path / "cache"), use_mlflow=False)
    monkeypatch.setenv("FRAUD_MODEL_SHA256_PRODUCTION", "f" * 64)
    with pytest.raises(ModelNotReadyError, match="sha256"):
        rt.load("production")
    monkeypatch.setenv("FRAUD_MODEL_SHA256_PRODUCTION", _sha(model_dir / "model.onnx"))
    assert rt.load("production").source == "local"


def test_a_registry_download_that_fails_its_digest_keeps_the_cached_model(
    model_dir: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The registry tag records the digest at registration; a swapped artifact leaves the cache intact."""
    good = _sha(model_dir / "model.onnx")
    cache = tmp_path / "cache"
    (cache / "production").mkdir(parents=True)
    shutil.copyfile(model_dir / "model.onnx", cache / "production" / "model.onnx")
    shutil.copyfile(model_dir / "model_meta.json", cache / "production" / "model_meta.json")

    def download_swapped(alias: str, dest: Path) -> Path:
        dest.mkdir(parents=True, exist_ok=True)
        (dest / "model.onnx").write_bytes((model_dir / "model.onnx").read_bytes() + b"\0")
        (dest / "model_meta.json").write_text(
            json.dumps(
                {
                    "feature_spec": "v2",
                    "registry": {"name": "fraud-gbdt", "version": "9", "onnx_sha256": good},
                }
            ),
            encoding="utf-8",
        )
        return dest

    monkeypatch.setattr("training.registry.download_alias", download_swapped)
    rt = ModelRuntime(model_dir=None, cache_dir=str(cache), use_mlflow=True)
    loaded = rt.load("production")
    assert loaded.source == "cache"
    assert _sha(cache / "production" / "model.onnx") == good


def test_a_registry_download_without_a_recorded_digest_is_refused(
    model_dir: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    def download_untagged(alias: str, dest: Path) -> Path:
        dest.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(model_dir / "model.onnx", dest / "model.onnx")
        (dest / "model_meta.json").write_text(
            json.dumps({"feature_spec": "v2", "registry": {"name": "fraud-gbdt", "version": "9"}}),
            encoding="utf-8",
        )
        return dest

    monkeypatch.setattr("training.registry.download_alias", download_untagged)
    rt = ModelRuntime(model_dir=None, cache_dir=str(tmp_path / "cache"), use_mlflow=True)
    with pytest.raises(ModelNotReadyError, match="sha256"):
        rt.load("production")


def test_a_verified_registry_download_replaces_the_cache(
    model_dir: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    good = _sha(model_dir / "model.onnx")

    def download(alias: str, dest: Path) -> Path:
        dest.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(model_dir / "model.onnx", dest / "model.onnx")
        (dest / "model_meta.json").write_text(
            json.dumps(
                {
                    "feature_spec": "v2",
                    "registry": {"name": "fraud-gbdt", "version": "9", "onnx_sha256": good},
                }
            ),
            encoding="utf-8",
        )
        return dest

    monkeypatch.setattr("training.registry.download_alias", download)
    cache = tmp_path / "cache"
    rt = ModelRuntime(model_dir=None, cache_dir=str(cache), use_mlflow=True)
    loaded = rt.load("production")
    assert (loaded.source, loaded.version) == ("mlflow", "fraud-gbdt/9")
    assert _sha(cache / "production" / "model.onnx") == good
    assert rt.load("production").source == "mlflow"  # a second download over an existing cache


@pytest.mark.parametrize("meta", ["deleted", "no digest"])
def test_a_model_without_any_recorded_digest_is_refused(model_dir: Path, tmp_path: Path, meta: str) -> None:
    """Security audit round 2, N3: swapping model.onnx and deleting its metadata must not load it."""
    (model_dir / "model.onnx").write_bytes((model_dir / "model.onnx").read_bytes() + b"\0")
    if meta == "deleted":
        (model_dir / "model_meta.json").unlink()
    else:
        doc = json.loads((model_dir / "model_meta.json").read_text(encoding="utf-8"))
        del doc["onnx_sha256"]
        (model_dir / "model_meta.json").write_text(json.dumps(doc), encoding="utf-8")
    rt = ModelRuntime(model_dir=str(model_dir), cache_dir=str(tmp_path / "cache"), use_mlflow=False)
    status = rt.load_all()
    assert not rt.ready()
    assert "no recorded sha256" in status["production"]


def test_outside_dev_a_local_model_needs_a_pinned_digest(
    model_dir: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Round-3 audit: model_meta.json sits next to the model, so whoever swaps one swaps both (D-047)."""
    monkeypatch.delenv("FRAUD_ENV")
    monkeypatch.delenv("FRAUD_MODEL_SHA256_PRODUCTION", raising=False)
    rt = ModelRuntime(model_dir=str(model_dir), cache_dir=str(tmp_path / "cache"), use_mlflow=False)
    with pytest.raises(ModelNotReadyError, match="FRAUD_MODEL_SHA256_PRODUCTION"):
        rt.load("production")
    monkeypatch.setenv("FRAUD_MODEL_SHA256_PRODUCTION", _sha(model_dir / "model.onnx"))
    assert rt.load("production").source == "local"
    # An offline evaluation of a given artifact may opt out explicitly.
    monkeypatch.delenv("FRAUD_MODEL_SHA256_PRODUCTION")
    trusted = ModelRuntime(model_dir=str(model_dir), use_mlflow=False, require_pin=False)
    assert trusted.load("production").source == "local"


def test_a_model_registered_after_startup_is_loaded_by_the_next_readiness_check(
    model_dir: Path, tmp_path: Path
) -> None:
    # Compose starts the model server before `make seed` trains and registers a model (D-073).
    late = tmp_path / "late"
    clock = [100.0]
    rt = ModelRuntime(
        model_dir=str(late), cache_dir=str(tmp_path / "cache"), use_mlflow=False, clock=lambda: clock[0]
    )
    rt.load_all()
    assert not rt.refresh()
    shutil.copytree(model_dir, late)
    clock[0] += 1.0
    assert not rt.refresh()  # retries are rate limited
    clock[0] += rt.retry_interval_s
    assert rt.refresh() and rt.ready()
    assert set(rt.models) == {"production", "shadow"}


def test_every_worker_loads_a_late_model_without_a_readiness_probe(model_dir: Path, tmp_path: Path) -> None:
    # BentoML runs several worker processes and /readyz reaches only one of them: a worker that never answers a
    # probe must still load a model registered after it started (one of two workers kept answering 503).
    import threading
    import time

    late = tmp_path / "late"
    rt = ModelRuntime(model_dir=str(late), cache_dir=str(tmp_path / "cache"), use_mlflow=False)
    rt.retry_interval_s = 0.05
    rt.load_all()
    stop = threading.Event()
    watcher = threading.Thread(target=rt.watch, args=(stop,), daemon=True)
    watcher.start()
    try:
        shutil.copytree(model_dir, late)
        deadline = time.monotonic() + 5
        while not (rt.ready() and "shadow" in rt.models) and time.monotonic() < deadline:
            time.sleep(0.02)
        assert rt.ready() and set(rt.models) == {"production", "shadow"}
        watcher.join(timeout=2)
        assert not watcher.is_alive()  # it stops once every alias is loaded
    finally:
        stop.set()
