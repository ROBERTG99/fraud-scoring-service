"""CI/CD and repository integrations (spec §7, §17, T-45)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import yaml

ROOT = Path(__file__).resolve().parents[2]
WF = ROOT / ".github" / "workflows"
REQUIRED = {"ci.yml", "e2e.yml", "evals.yml", "docker.yml", "terraform.yml", "codeql.yml", "release.yml"}


def workflow(name: str) -> dict[str, Any]:
    doc = yaml.safe_load((WF / name).read_text(encoding="utf-8"))
    assert isinstance(doc, dict), name
    # PyYAML reads the bare key `on` as True.
    if True in doc:
        doc["on"] = doc.pop(True)
    return doc


def run_text(doc: dict[str, Any]) -> str:
    return "\n".join(
        str(step.get("run", "")) + str(step.get("uses", ""))
        for job in doc["jobs"].values()
        for step in job.get("steps", [])
    )


def test_required_workflows_exist() -> None:
    assert {p.name for p in WF.glob("*.yml")} >= REQUIRED
    for name in REQUIRED:
        doc = workflow(name)
        assert doc.get("on") and doc.get("jobs"), name
        # Least privilege: every workflow states its token permissions.
        assert "permissions" in doc, f"{name} has no top-level permissions"
        for job_name, job in doc["jobs"].items():
            assert "timeout-minutes" in job, f"{name}:{job_name} has no timeout"


def test_ci_covers_lint_types_tests_and_bench() -> None:
    ci = workflow("ci.yml")
    assert {"lint", "typecheck", "unit", "integration", "bench"} <= ci["jobs"].keys()
    text = run_text(ci)
    for target in ("make lint", "make typecheck", "make bench"):
        assert target in text, target
    assert "pull_request" in ci["on"] and "push" in ci["on"]


def test_evals_use_the_released_evalgate_action() -> None:
    text = run_text(workflow("evals.yml"))
    assert "ROBERTG99/evalgate/action@v0.1.0" in text
    assert "evals/reports/baseline/" in (WF / "evals.yml").read_text(encoding="utf-8")


def test_docker_terraform_e2e_and_release() -> None:
    # docker.yml builds every image and runs `make security`, whose scanners include trivy and hadolint.
    docker = run_text(workflow("docker.yml"))
    assert "docker compose build" in docker and "make security" in docker and "make test-docker" in docker
    security = (ROOT / "scripts" / "security.sh").read_text(encoding="utf-8")
    assert "trivy image" in security and "hadolint" in security
    assert "make tf-check" in run_text(workflow("terraform.yml"))
    e2e = run_text(workflow("e2e.yml"))
    assert "make e2e" in e2e and "docker compose" in e2e
    release = workflow("release.yml")
    assert release["on"]["push"]["tags"] == ["v*"]
    rel = run_text(release)
    images = [
        step["with"]["images"]
        for job in release["jobs"].values()
        for step in job.get("steps", [])
        if str(step.get("uses", "")).startswith("docker/metadata-action")
    ]
    assert images and all(i.split("/", 1)[0] == "ghcr.io" for i in images), images
    assert "sbom" in rel.lower()
    # Nothing here deploys anywhere or applies Terraform (spec §0 rule 8).
    for name in REQUIRED:
        text = (WF / name).read_text(encoding="utf-8")
        assert "terraform apply" not in text and "terraform destroy" not in text, name


def test_repository_integrations() -> None:
    dep = yaml.safe_load((ROOT / ".github" / "dependabot.yml").read_text(encoding="utf-8"))
    ecosystems = {u["package-ecosystem"] for u in dep["updates"]}
    assert {"gomod", "uv", "npm", "docker", "github-actions", "terraform"} <= ecosystems
    assert (ROOT / ".github" / "CODEOWNERS").read_text(encoding="utf-8").split() == ["*", "@ROBERTG99"]
    assert (ROOT / ".github" / "pull_request_template.md").is_file()
    assert {p.name for p in (ROOT / ".github" / "ISSUE_TEMPLATE").glob("*.md")} >= {
        "bug_report.md",
        "feature_request.md",
    }
    assert (ROOT / ".pre-commit-config.yaml").is_file()
    dc = json.loads((ROOT / ".devcontainer" / "devcontainer.json").read_text(encoding="utf-8"))
    assert dc.get("postCreateCommand") == "make bootstrap"
    # No Claude attribution in templates (spec §1.1).
    for p in (ROOT / ".github").rglob("*.md"):
        text = p.read_text(encoding="utf-8")
        assert "Co-Authored-By" not in text and "Generated with Claude Code" not in text, p
