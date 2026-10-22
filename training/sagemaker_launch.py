"""Build (and optionally submit) the SageMaker `CreateTrainingJob` request for the fraud model.

    python -m training.sagemaker_launch --dry-run      # print the request; no credentials needed
    python -m training.sagemaker_launch --submit       # submit (never run by make targets or CI)

The request is validated offline against botocore's own SageMaker service model, so a dry run catches
shape errors without AWS access. Resource names follow infra/terraform/modules/sagemaker_training.
Uses boto3 only (docs/DECISIONS.md D-003).
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from dataclasses import dataclass, field
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import boto3
import botocore.session
from botocore.validate import ParamValidator

REPO_ROOT = Path(__file__).resolve().parents[1]
PLACEHOLDER_ACCOUNT = "123456789012"  # AWS documentation account; dry runs never resolve a real one
PROJECT = "fraud-scoring"


@dataclass(frozen=True)
class LaunchConfig:
    region: str = "us-east-1"
    account_id: str = PLACEHOLDER_ACCOUNT
    environment: str = "dev"
    image_tag: str = "dev"
    instance_type: str = "ml.m5.xlarge"
    volume_gb: int = 30
    max_runtime_s: int = 3600
    data_prefix: str = "datasets/demo/"
    job_name: str | None = None
    kms_key_arn: str | None = None
    hyperparameters: dict[str, str] = field(default_factory=lambda: {"seed": "7", "mlflow": "false"})

    @property
    def name_prefix(self) -> str:
        return f"{PROJECT}-{self.environment}"

    @property
    def bucket(self) -> str:
        return f"{self.name_prefix}-artifacts-{self.account_id}"

    @property
    def role_arn(self) -> str:
        return f"arn:aws:iam::{self.account_id}:role/{self.name_prefix}-sagemaker-training"

    @property
    def image_uri(self) -> str:
        return f"{self.account_id}.dkr.ecr.{self.region}.amazonaws.com/{self.name_prefix}-training:{self.image_tag}"


def default_job_name(now: datetime | None = None) -> str:
    stamp = (now or datetime.now(UTC)).strftime("%Y%m%d-%H%M%S")
    return f"fraud-gbdt-{stamp}"  # ≤ 63 chars, [a-zA-Z0-9-]


def build_request(cfg: LaunchConfig, *, now: datetime | None = None) -> dict[str, Any]:
    output: dict[str, Any] = {"S3OutputPath": f"s3://{cfg.bucket}/models/"}
    resources: dict[str, Any] = {
        "InstanceType": cfg.instance_type,
        "InstanceCount": 1,
        "VolumeSizeInGB": cfg.volume_gb,
    }
    if cfg.kms_key_arn:
        output["KmsKeyId"] = cfg.kms_key_arn
        resources["VolumeKmsKeyId"] = cfg.kms_key_arn
    return {
        "TrainingJobName": cfg.job_name or default_job_name(now),
        "AlgorithmSpecification": {"TrainingImage": cfg.image_uri, "TrainingInputMode": "File"},
        "RoleArn": cfg.role_arn,
        "InputDataConfig": [
            {
                "ChannelName": "train",
                "DataSource": {
                    "S3DataSource": {
                        "S3DataType": "S3Prefix",
                        "S3Uri": f"s3://{cfg.bucket}/{cfg.data_prefix}",
                        "S3DataDistributionType": "FullyReplicated",
                    }
                },
                "ContentType": "application/jsonlines",
                "InputMode": "File",
            }
        ],
        "OutputDataConfig": output,
        "ResourceConfig": resources,
        "StoppingCondition": {"MaxRuntimeInSeconds": cfg.max_runtime_s},
        "HyperParameters": dict(cfg.hyperparameters),
        # Synthetic data in, model artifact out: the container needs no network access.
        "EnableNetworkIsolation": True,
        "EnableInterContainerTrafficEncryption": True,
        "Tags": [
            {"Key": "project", "Value": PROJECT},
            {"Key": "environment", "Value": cfg.environment},
            {"Key": "data", "Value": "synthetic"},
        ],
    }


def validate_request(request: dict[str, Any]) -> None:
    """Raise ValueError when the request does not match botocore's CreateTrainingJob input shape."""
    model = botocore.session.get_session().get_service_model("sagemaker")
    shape = model.operation_model("CreateTrainingJob").input_shape
    report = ParamValidator().validate(request, shape)
    if report.has_errors():
        raise ValueError(report.generate_report())


def submit(request: dict[str, Any], client: Any) -> str:
    validate_request(request)
    resp = client.create_training_job(**request)
    return str(resp["TrainingJobArn"])


def git_short_sha() -> str:
    try:
        out = subprocess.run(
            ["git", "rev-parse", "--short=12", "HEAD"],  # noqa: S607 -- developer tool on PATH
            cwd=REPO_ROOT,
            capture_output=True,
            text=True,
            check=True,
            timeout=5,
        )
        return out.stdout.strip() or "dev"
    except (OSError, subprocess.SubprocessError):
        return "dev"


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    mode = ap.add_mutually_exclusive_group(required=True)
    mode.add_argument("--dry-run", action="store_true", help="print the validated request and exit")
    mode.add_argument("--submit", action="store_true", help="call CreateTrainingJob with ambient credentials")
    ap.add_argument("--region", default=os.environ.get("AWS_REGION", "us-east-1"))
    ap.add_argument("--account-id", default=os.environ.get("FRAUD_AWS_ACCOUNT_ID", PLACEHOLDER_ACCOUNT))
    ap.add_argument("--environment", default="dev")
    ap.add_argument("--image-tag", default=None, help="training image tag (default: git short SHA)")
    ap.add_argument("--instance-type", default="ml.m5.xlarge")
    ap.add_argument("--max-runtime", type=int, default=3600)
    ap.add_argument("--data-prefix", default="datasets/demo/")
    ap.add_argument("--job-name", default=None)
    ap.add_argument("--kms-key-arn", default=None)
    args = ap.parse_args(argv)
    cfg = LaunchConfig(
        region=args.region,
        account_id=args.account_id,
        environment=args.environment,
        image_tag=args.image_tag or git_short_sha(),
        instance_type=args.instance_type,
        max_runtime_s=args.max_runtime,
        data_prefix=args.data_prefix,
        job_name=args.job_name,
        kms_key_arn=args.kms_key_arn,
    )
    request = build_request(cfg)
    try:
        validate_request(request)
    except ValueError as exc:
        print(f"invalid CreateTrainingJob request:\n{exc}", file=sys.stderr)
        return 2
    if args.dry_run:
        print(json.dumps(request, indent=2, sort_keys=True))
        return 0
    arn = submit(request, boto3.client("sagemaker", region_name=cfg.region))
    print(arn)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
