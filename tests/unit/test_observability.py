"""Observability profile (spec §14, T-41): dashboard, alerts and compose wiring stay in step with the code."""

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
    # Spec §14 metrics all appear on the dashboard.
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
    ports = {p.split(":")[0] for s in obs.values() for p in s.get("ports", [])}
    assert ports == {"8580", "8581", "8590", "8591", "8592"}
    assert all(":" in s["image"] and not s["image"].endswith(":latest") for s in obs.values())
    dd = [s for s in services.values() if "datadog" in s.get("profiles", [])]
    assert dd and all("DD_API_KEY" in json.dumps(s.get("environment", {})) for s in dd)


def test_committed_dashboard_is_generated() -> None:
    from scripts import gen_dashboard

    assert DASHBOARD.read_text() == gen_dashboard.render(), "run `python scripts/gen_dashboard.py`"
