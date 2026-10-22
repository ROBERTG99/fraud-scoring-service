"""scripts/terraform.sh runs only the offline checks, without any AWS credential source (T-49, D-077)."""

from __future__ import annotations

import os
import shutil
import stat
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


def _run(tmp_path: Path, *args: str) -> subprocess.CompletedProcess[str]:
    tree = tmp_path / "repo"
    (tree / "scripts").mkdir(parents=True, exist_ok=True)
    shutil.copy(ROOT / "scripts" / "terraform.sh", tree / "scripts" / "terraform.sh")
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir(exist_ok=True)
    stub = bin_dir / "terraform"  # prints what it was given and the credential-related environment
    stub.write_text(
        '#!/bin/sh\necho "args=$*"\n'
        'echo "key=${AWS_ACCESS_KEY_ID:-} profile=${AWS_PROFILE:-} creds=${AWS_SHARED_CREDENTIALS_FILE:-} '
        'config=${AWS_CONFIG_FILE:-} imds=${AWS_EC2_METADATA_DISABLED:-} role=${AWS_ROLE_ARN:-}"\n',
        encoding="utf-8",
    )
    stub.chmod(stub.stat().st_mode | stat.S_IXUSR)
    env = {
        **os.environ,
        "PATH": f"{bin_dir}:/usr/bin:/bin",
        "AWS_ACCESS_KEY_ID": "AKIAEXAMPLE",
        "AWS_PROFILE": "default",
        "AWS_ROLE_ARN": "arn:aws:iam::123456789012:role/x",
    }
    return subprocess.run(
        ["bash", str(tree / "scripts" / "terraform.sh"), *args],
        env=env,
        capture_output=True,
        text=True,
        check=False,
        timeout=30,
    )


@pytest.mark.parametrize(
    "args",
    [
        ["apply"],
        ["-chdir=infra/terraform/envs/dev", "destroy"],
        ["plan"],
        ["init"],
        ["force-unlock", "x"],
        ["workspace", "delete", "dev"],
        ["login"],
        ["state", "rm", "x"],
        [],
    ],
)
def test_anything_but_the_offline_checks_is_refused(tmp_path: Path, args: list[str]) -> None:
    out = _run(tmp_path, *args)
    assert out.returncode == 2, out
    assert "args=" not in out.stdout  # terraform never ran


@pytest.mark.parametrize(
    "args",
    [
        ["fmt", "-check", "-recursive", "infra/terraform"],
        ["-chdir=infra/terraform/modules/sagemaker_training", "init", "-backend=false", "-input=false"],
        ["-chdir=infra/terraform/modules/sagemaker_training", "validate"],
        ["-chdir=infra/terraform/modules/sagemaker_training", "test", "-no-color"],
    ],
)
def test_offline_checks_run_without_any_credential_source(tmp_path: Path, args: list[str]) -> None:
    out = _run(tmp_path, *args)
    assert out.returncode == 0, out
    assert out.stdout.splitlines()[1] == "key= profile= creds=/dev/null config=/dev/null imds=true role="
