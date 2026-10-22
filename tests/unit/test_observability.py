"""Observability profile: dashboard, alerts and compose wiring stay in step with the code."""

from __future__ import annotations

import json
import re
from pathlib import Path
from typing import Any

import yaml

ROOT = Path(__file__).resolve().parents[2]
DASHBOARD = ROOT / "deploy" / "grafana" / "fraud.json"
ALERTS = ROOT / "deploy" / "prometheus" / "alerts.yml"


def registered_series() -> set[str]:
    """Series names the services expose: Go metrics.go names and Python prometheus_client names."""
    names: set[str] = set()
    for path in [ROOT / "internal/telemetry/metrics.go"]:
        for kind, name in re.findall(
            r'prometheus\.New(\w+?)(?:Vec)?\(prometheus\.\w+Opts\{\s*Name:\s*"(\w+)"', path.read_text(), re.S
        ):
            names |= {name} | (
                {f"{name}_bucket", f"{name}_count", f"{name}_sum"} if kind == "Histogram" else set()
            )
    for path in (ROOT / "src/fraud_summarizer").glob("*.py"):
        for kind, name in re.findall(r'\b(Counter|Gauge|Histogram)\(\s*"(\w+)"', path.read_text()):
            # prometheus_client strips a trailing _total from counter names and exposes <name>_total.
            names |= {f"{name.removesuffix('_total')}_total"} if kind == "Counter" else {name}
    return names


def exprs(node: Any) -> list[str]:
    if isinstance(node, dict):
        return [v for k, v in node.items() if k == "expr" and isinstance(v, str)] + [
            e for v in node.values() for e in exprs(v)
        ]
    if isinstance(node, list):
        return [e for v in node for e in exprs(v)]
    return []


def metric_names(expr: str) -> set[str]:
    return set(re.findall(r"\b(fraud_\w+)", expr))


def test_dashboard_and_alerts_use_only_registered_metrics() -> None:
    known = registered_series()
    assert {
        "fraud_score_latency_seconds_bucket",
        "fraud_llm_cost_usd_total",
        "fraud_decisions_total",
    } <= known
    dashboard = json.loads(DASHBOARD.read_text())
    used = {m for e in exprs(dashboard) + exprs(yaml.safe_load(ALERTS.read_text())) for m in metric_names(e)}
    assert used and used <= known, f"unknown metrics: {sorted(used - known)}"
    # Service metrics all appear on the dashboard.
    for m in (
        "fraud_score_latency_seconds_bucket",
        "fraud_decisions_total",
        "fraud_degraded_total",
        "fraud_updater_lag_seconds",
        "fraud_llm_cost_usd_total",
        "fraud_outbox_oldest_unsent_age_seconds",
    ):
        assert any(m in e for e in exprs(dashboard)), m


def test_slo_alerts_match_the_spec() -> None:
    rules = {r["alert"]: r for g in yaml.safe_load(ALERTS.read_text())["groups"] for r in g["rules"]}
    assert set(rules) >= {"ScoreLatencyHigh", "DegradedDecisions", "OutboxUnsentAge"}
    lat = rules["ScoreLatencyHigh"]["expr"]
    assert "histogram_quantile(0.95" in lat and "[5m]" in lat and "> 0.05" in lat
    deg = rules["DegradedDecisions"]["expr"]
    assert (
        "fraud_degraded_total" in deg
        and "fraud_decisions_total" in deg
        and "[10m]" in deg
        and "> 0.01" in deg
    )
    assert "fraud_outbox_oldest_unsent_age_seconds > 60" in rules["OutboxUnsentAge"]["expr"]
    for r in rules.values():
        assert r["labels"]["severity"] in {"warning", "critical"} and r["annotations"]["runbook"].startswith(
            "docs/runbook.md#"
        )


def test_dashboard_datasources_are_provisioned() -> None:
    prov = yaml.safe_load((ROOT / "deploy/grafana/provisioning/datasources/datasources.yaml").read_text())
    uids = {d["uid"] for d in prov["datasources"]}
    dashboard = json.loads(DASHBOARD.read_text())
    used = {p["datasource"]["uid"] for p in dashboard["panels"] if "datasource" in p}
    assert used and used <= uids
    assert dashboard["uid"] == "fraud" and dashboard["title"]


def test_observability_profile_ports() -> None:
    services = yaml.safe_load((ROOT / "compose.yaml").read_text())["services"]
    obs = {n: s for n, s in services.items() if "observability" in s.get("profiles", [])}
    ports = {p.split(":")[1] for s in obs.values() for p in s.get("ports", [])}
    assert ports == {"8580", "8581", "8590", "8591", "8592"}
    assert all(":" in s["image"] and not s["image"].endswith(":latest") for s in obs.values())
    dd = [s for s in services.values() if "datadog" in s.get("profiles", [])]
    assert dd and all("DD_API_KEY" in json.dumps(s.get("environment", {})) for s in dd)


def test_committed_dashboard_is_generated() -> None:
    from scripts import gen_dashboard

    assert DASHBOARD.read_text() == gen_dashboard.render(), "run `python scripts/gen_dashboard.py`"


def test_compose_publishes_on_loopback_only() -> None:
    """Security audit: the local stack is reachable from this machine only."""
    for name, svc in yaml.safe_load((ROOT / "compose.yaml").read_text())["services"].items():
        for port in svc.get("ports", []):
            assert str(port).startswith("127.0.0.1:"), f"{name} publishes {port} on every interface"


def test_grafana_has_no_default_credentials_and_prometheus_no_lifecycle_api() -> None:
    """Security audit: no admin/admin, no basic auth or login form; anonymous users are read-only viewers."""
    services = yaml.safe_load((ROOT / "compose.yaml").read_text())["services"]
    env = services["grafana"]["environment"]
    assert env["GF_SECURITY_DISABLE_INITIAL_ADMIN_CREATION"] == "true"
    assert env["GF_AUTH_BASIC_ENABLED"] == "false" and env["GF_AUTH_DISABLE_LOGIN_FORM"] == "true"
    assert env["GF_AUTH_ANONYMOUS_ORG_ROLE"] == "Viewer"
    assert services["grafana"]["image"] != "grafana/grafana:11.5.2"  # 3 HIGH findings in Grafana itself
    assert "--web.enable-lifecycle" not in services["prometheus"].get("command", [])
    obs_host = (ROOT / "scripts/obs_host.sh").read_text()
    assert "GF_SECURITY_DISABLE_INITIAL_ADMIN_CREATION=true" in obs_host


def test_service_containers_are_hardened() -> None:
    """Security audit: read-only root, no capabilities, no privilege gain; bases pinned by digest."""
    services = yaml.safe_load((ROOT / "compose.yaml").read_text())["services"]
    built = {n: s for n, s in services.items() if "build" in s}
    # Every image this repository builds runs hardened, jobs included.
    assert {"scorer", "case-summarizer", "model-server", "webserve", "seed", "migrate", "mlflow"} <= set(
        built
    )
    for name, svc in built.items():
        assert svc.get("read_only") is True, name
        assert svc.get("cap_drop") == ["ALL"], name
        assert "no-new-privileges:true" in svc.get("security_opt", []), name
    for dockerfile in (
        "Dockerfile.go",
        "Dockerfile.summarizer",
        "Dockerfile.test",
        "serving/bentoml/Dockerfile",
        "deploy/mlflow/Dockerfile",
        "deploy/seed/Dockerfile",
    ):
        text = (ROOT / dockerfile).read_text()
        bases = re.findall(r"^FROM (\S+)", text, re.M) + re.findall(r"^ARG \w+_IMAGE=(\S+)", text, re.M)
        for base in bases:
            if not base.startswith("${"):
                assert "@sha256:" in base, f"{dockerfile}: {base} is not pinned by digest"
