"""MLflow registry (spec FR-5, T-15) against a local file store."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from training import registry
from training import train as T

ROOT = Path(__file__).resolve().parents[2]


@pytest.fixture
def tracking(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> str:
    uri = "sqlite:///" + str(tmp_path / "mlruns" / "mlflow.db")
    (tmp_path / "mlruns").mkdir()
    monkeypatch.setenv("MLFLOW_TRACKING_URI", uri)
    return uri


@pytest.fixture
def trained(tmp_path: Path) -> tuple[Path, dict[str, object]]:
    out = tmp_path / "model"
    meta = T.train(ROOT / "tests/fixtures/parity", out, tiny=True)
    return out, meta


def test_set_alias_production(tracking: str, trained: tuple[Path, dict[str, object]]) -> None:
    out, meta = trained
    reg = registry.log_and_register(out, meta)
    assert reg["name"] == "fraud-gbdt"
    version = reg["version"]
    info = registry.describe(version)
    assert float(info["tags"]["pr_auc"]) == pytest.approx(meta["test"]["pr_auc"])  # type: ignore[index]
    assert "recall_at_1pct_fpr" in info["tags"]
    registry.set_alias("production", version)
    assert registry.version_for_alias("production") == version
    # A second registration moves only the alias that is set explicitly.
    reg2 = registry.log_and_register(out, meta)
    registry.set_alias("shadow", reg2["version"])
    assert registry.version_for_alias("production") == version
    assert registry.version_for_alias("shadow") == reg2["version"]
    dest = registry.download_alias("shadow", out.parent / "dl")
    assert (dest / "model.onnx").read_bytes() == (out / "model.onnx").read_bytes()
    assert (
        json.loads((dest / "model_meta.json").read_text(encoding="utf-8"))["model_version"]
        == meta["model_version"]
    )


def test_cli_sets_alias(tracking: str, trained: tuple[Path, dict[str, object]]) -> None:
    out, meta = trained
    reg = registry.log_and_register(out, meta)
    meta["registered"] = reg
    (out / "model_meta.json").write_text(json.dumps(meta), encoding="utf-8")
    assert registry.main(["--model-dir", str(out), "--alias", "production"]) == 0
    assert registry.version_for_alias("production") == reg["version"]
