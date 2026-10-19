"""SageMaker launcher (FR-12, T-30): request shape, offline validation, moto submission, container args."""

from __future__ import annotations

import json
from datetime import UTC, datetime
from pathlib import Path

import boto3
import pytest
from moto import mock_aws

from training import sagemaker_entry
from training.sagemaker_launch import LaunchConfig, build_request, main, submit, validate_request

NOW = datetime(2026, 10, 16, 12, 0, 0, tzinfo=UTC)


@pytest.fixture
def no_aws_credentials(monkeypatch: pytest.MonkeyPatch) -> None:
    for var in ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE"):
        monkeypatch.delenv(var, raising=False)
    monkeypatch.setenv("AWS_CONFIG_FILE", "/nonexistent")
    monkeypatch.setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")


def test_dry_run_request_shape(no_aws_credentials: None, capsys: pytest.CaptureFixture[str]) -> None:
    req = build_request(LaunchConfig(image_tag="abc123"), now=NOW)
    validate_request(req)
    assert req["TrainingJobName"] == "fraud-gbdt-20261016-120000"
    assert req["RoleArn"] == "arn:aws:iam::123456789012:role/fraud-scoring-dev-sagemaker-training"
    assert req["AlgorithmSpecification"] == {
        "TrainingImage": "123456789012.dkr.ecr.us-east-1.amazonaws.com/fraud-scoring-dev-training:abc123",
        "TrainingInputMode": "File",
    }
    channel = req["InputDataConfig"][0]
    assert channel["ChannelName"] == "train"
    assert (
        channel["DataSource"]["S3DataSource"]["S3Uri"]
        == "s3://fraud-scoring-dev-artifacts-123456789012/datasets/demo/"
    )
    assert req["OutputDataConfig"] == {
        "S3OutputPath": "s3://fraud-scoring-dev-artifacts-123456789012/models/"
    }
    assert req["EnableNetworkIsolation"] is True and req["EnableInterContainerTrafficEncryption"] is True
    assert all(isinstance(v, str) for v in req["HyperParameters"].values())
    assert {"Key": "data", "Value": "synthetic"} in req["Tags"]

    assert main(["--dry-run", "--image-tag", "abc123", "--job-name", "fraud-gbdt-test"]) == 0
    printed = json.loads(capsys.readouterr().out)
    assert printed["TrainingJobName"] == "fraud-gbdt-test"
    validate_request(printed)


def test_kms_key_applies_to_output_and_volume() -> None:
    key = "arn:aws:kms:us-east-1:123456789012:key/0000"
    req = build_request(LaunchConfig(kms_key_arn=key), now=NOW)
    assert req["OutputDataConfig"]["KmsKeyId"] == key and req["ResourceConfig"]["VolumeKmsKeyId"] == key
    validate_request(req)


def test_invalid_request_is_rejected_offline(capsys: pytest.CaptureFixture[str]) -> None:
    req = build_request(LaunchConfig(), now=NOW)
    del req["RoleArn"]
    req["StoppingCondition"]["MaxRuntimeInSeconds"] = "one hour"
    with pytest.raises(ValueError, match="RoleArn"):
        validate_request(req)
    assert main(["--dry-run", "--max-runtime", "0"]) == 2  # below the API minimum of 1
    assert "invalid CreateTrainingJob request" in capsys.readouterr().err


def test_submit_to_moto(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AWS_ACCESS_KEY_ID", "testing")
    monkeypatch.setenv("AWS_SECRET_ACCESS_KEY", "testing")
    monkeypatch.setenv("AWS_DEFAULT_REGION", "us-east-1")
    with mock_aws():
        client = boto3.client("sagemaker", region_name="us-east-1")
        req = build_request(LaunchConfig(image_tag="abc123"), now=NOW)
        arn = submit(req, client)
        assert arn.endswith(":training-job/fraud-gbdt-20261016-120000")
        job = client.describe_training_job(TrainingJobName=req["TrainingJobName"])
        assert job["RoleArn"] == req["RoleArn"]
        assert (
            job["AlgorithmSpecification"]["TrainingImage"] == req["AlgorithmSpecification"]["TrainingImage"]
        )


def test_container_entry_maps_sagemaker_paths(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("SM_CHANNEL_TRAIN", raising=False)
    monkeypatch.delenv("SM_MODEL_DIR", raising=False)
    cfg = tmp_path / "input" / "config"
    cfg.mkdir(parents=True)
    assert sagemaker_entry.argv(tmp_path) == [
        "--data",
        str(tmp_path / "input" / "data" / "train"),
        "--out",
        str(tmp_path / "model"),
        "--no-mlflow",
    ]
    (cfg / "hyperparameters.json").write_text(json.dumps({"seed": "7", "tiny": "true", "mlflow": "false"}))
    monkeypatch.setenv("SM_CHANNEL_TRAIN", "/data")
    assert sagemaker_entry.argv(tmp_path)[:2] == ["--data", "/data"]
    assert "--tiny" in sagemaker_entry.argv(tmp_path)

    seen: list[list[str]] = []
    monkeypatch.setenv("SM_MODEL_DIR", str(tmp_path / "out"))
    monkeypatch.setattr(sagemaker_entry.train, "main", lambda a: seen.append(a) or 0)
    assert sagemaker_entry.main() == 0
    assert seen[0][:4] == ["--data", "/data", "--out", str(tmp_path / "out")]
