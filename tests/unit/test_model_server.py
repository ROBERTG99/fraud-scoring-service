"""BentoML model server (spec FR-6, T-16)."""

from __future__ import annotations

import json
import shutil
from pathlib import Path

import pytest
from starlette.testclient import TestClient

from serving.bentoml.runtime import ModelNotReadyError, ModelRuntime

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / "tests" / "fixtures" / "model"


@pytest.fixture
def model_dir(tmp_path: Path) -> Path:
    d = tmp_path / "model"
    d.mkdir()
    shutil.copyfile(FIXTURES / "tiny.onnx", d / "model.onnx")
    meta = json.loads((FIXTURES / "golden.json").read_text(encoding="utf-8"))
    (d / "model_meta.json").write_text(json.dumps({"model_version": meta["model_version"]}), encoding="utf-8")
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
    assert rt.predict([vectors[2]["features"]], "shadow")["scores"][0] == pytest.approx(vectors[2]["probability"], abs=1e-6)  # type: ignore[index]


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


def test_uses_cached_artifact_when_mlflow_down(model_dir: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    cache = tmp_path / "cache"
    (cache / "production").mkdir(parents=True)
    shutil.copyfile(model_dir / "model.onnx", cache / "production" / "model.onnx")
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
