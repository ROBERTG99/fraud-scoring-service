"""Compose stack contract (spec §16, T-43): ports, healthchecks, dependencies and profiles."""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any

import yaml

ROOT = Path(__file__).resolve().parents[2]

# Host ports from spec §16 (container ports vary).
EXPECTED_PORTS = {
    "webserve": {8500},
    "scorer": {8510, 8511},
    "feature-updater": {8512},
    "case-summarizer": {8513},
    "model-server": {8514},
    "dev-issuer": {8515},
    "postgres": {8520},
    "redis": {8530},
    "redpanda": {8540, 8541},
    "mlflow": {8560},
    "triton": {8550, 8551, 8552},
}
ONE_SHOT = {"migrate", "seed", "retention"}  # jobs: they exit, so they carry no healthcheck
# Third-party observability images ship without a shell or probe binary; their dependents use
# service_started, and only inside the optional profiles (D-073).
NO_PROBE = {"jaeger", "otel-collector", "otel-collector-datadog"}


def load(name: str) -> dict[str, Any]:
    doc = yaml.safe_load((ROOT / name).read_text(encoding="utf-8"))
    assert isinstance(doc, dict)
    return doc


def dockerfile_has_healthcheck(service: dict[str, Any]) -> bool:
    build = service.get("build")
    if not isinstance(build, dict):
        return False
    path = ROOT / build.get("context", ".") / build.get("dockerfile", "Dockerfile")
    text = path.read_text(encoding="utf-8")
    target = build.get("target")
    if target:  # only the HEALTHCHECK inside the target stage counts
        stages = re.split(r"(?m)^FROM ", text)
        text = next(s for s in stages if re.match(rf"\S+ AS {re.escape(target)}\s", s))
    return bool(re.search(r"(?m)^HEALTHCHECK CMD|^HEALTHCHECK --", text))


def host_ports(service: dict[str, Any]) -> list[tuple[str, int]]:
    out = []
    for p in service.get("ports", []):
        ip, host, _container = str(p).split(":")
        out.append((ip, int(host)))
    return out


def test_ports_in_range_and_healthchecks() -> None:
    services = load("compose.yaml")["services"]
    assert EXPECTED_PORTS.keys() | ONE_SHOT <= services.keys()
    for name, svc in services.items():
        ports = host_ports(svc)
        for ip, port in ports:
            assert ip == "127.0.0.1", f"{name} publishes on {ip} (loopback only, D-049)"
            assert 8500 <= port <= 8599, f"{name} publishes {port}"
        if name in EXPECTED_PORTS:
            assert {p for _, p in ports} == EXPECTED_PORTS[name], name
        if name not in ONE_SHOT | NO_PROBE:
            assert "healthcheck" in svc or dockerfile_has_healthcheck(svc), f"{name} has no healthcheck"
        deps = svc.get("depends_on", {})
        assert isinstance(deps, dict), f"{name}: depends_on must name a condition"
        for dep, spec in deps.items():
            cond = spec["condition"]
            if dep in NO_PROBE:
                assert cond == "service_started" and svc.get("profiles"), (name, dep)
            elif dep in ONE_SHOT:
                assert cond == "service_completed_successfully", (name, dep)
            else:
                assert cond == "service_healthy", (name, dep)


def test_profiles_and_jobs() -> None:
    services = load("compose.yaml")["services"]
    profiles = {p for s in services.values() for p in s.get("profiles", [])}
    assert {"triton", "observability", "datadog", "live"} <= profiles
    assert services["triton"]["profiles"] == ["triton"]
    # `make up` must not run the seed job; `make seed` runs it with `docker compose run`.
    assert services["seed"].get("profiles") == ["seed"]
    # The scoring path never waits for the optional Triton profile or the LLM summarizer.
    assert set(services["scorer"]["depends_on"]) & {"triton", "case-summarizer"} == set()


def test_test_compose_runs_the_suite_in_containers() -> None:
    services = load("compose.test.yaml")["services"]
    assert {"postgres", "redis", "redpanda", "tests"} <= services.keys()
    runner = services["tests"]
    assert runner.get("profiles") == ["docker"]  # `make test` on the host starts only the backing services
    assert set(runner["depends_on"]) == {"postgres", "redis", "redpanda"}
    for name, svc in services.items():
        for ip, port in host_ports(svc):
            assert ip == "127.0.0.1" and 8500 <= port <= 8599, (name, port)
