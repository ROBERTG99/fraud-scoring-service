"""Write deploy/grafana/fraud.json, the provisioned Grafana dashboard of the `observability` profile (spec §14).

`python scripts/gen_dashboard.py` regenerates it; `tests/unit/test_observability.py` fails when the
committed file differs. Colors (dataviz reference palette, dark steps): actions take categorical slots 1-3
in fixed order, latency quantiles one blue ramp, SLO thresholds the reserved status colors. Every panel has
one y-axis.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "deploy" / "grafana" / "fraud.json"
PROM = {"type": "prometheus", "uid": "prometheus"}

ACTIONS = {"APPROVE": "#3987e5", "REVIEW": "#d95926", "DECLINE": "#199e70"}  # categorical slots 1-3 (dark)
QUANTILES = {"p50": "#86b6ef", "p95": "#3987e5", "p99": "#1c5cab"}  # blue ramp 250 / 400 / 550
GOOD, WARNING, CRITICAL = "#0ca30c", "#fab219", "#d03b3b"  # status palette
SLOTS = ["#3987e5", "#d95926", "#199e70", "#c98500", "#d55181", "#9085e9"]  # categorical slots 1-6 (dark)


def thresholds(*steps: tuple[float | None, str]) -> dict[str, Any]:
    return {"mode": "absolute", "steps": [{"value": v, "color": c} for v, c in steps]}


def target(expr: str, legend: str = "", ref: str = "A", instant: bool = False) -> dict[str, Any]:
    t: dict[str, Any] = {"refId": ref, "datasource": PROM, "expr": expr, "legendFormat": legend or "__auto"}
    if instant:
        t["instant"] = True
    return t


def stat(title: str, expr: str, unit: str, steps: dict[str, Any], x: int, desc: str) -> dict[str, Any]:
    return {
        "type": "stat",
        "title": title,
        "description": desc,
        "datasource": PROM,
        "gridPos": {"x": x, "y": 0, "w": 6, "h": 4},
        "targets": [target(expr)],
        "fieldConfig": {
            "defaults": {"unit": unit, "decimals": 2, "thresholds": steps, "color": {"mode": "thresholds"}}
        },
        "options": {
            "reduceOptions": {"calcs": ["lastNotNull"]},
            "colorMode": "background",
            "graphMode": "area",
        },
    }


def series(
    title: str,
    targets: list[dict[str, Any]],
    unit: str,
    pos: tuple[int, int, int],
    desc: str,
    colors: dict[str, str] | None = None,
    line: tuple[float, str] | None = None,
    soft_max: float | None = None,
) -> dict[str, Any]:
    x, y, w = pos
    defaults: dict[str, Any] = {
        "unit": unit,
        "min": 0,
        "custom": {"lineWidth": 2, "fillOpacity": 0, "showPoints": "never", "axisSoftMin": 0},
    }
    if soft_max is not None:  # keep the SLO line on screen when traffic is well under it
        defaults["custom"]["axisSoftMax"] = soft_max
    if line is not None:  # the SLO as a dashed threshold line, not a second axis
        defaults["thresholds"] = thresholds((None, "transparent"), line)
        defaults["custom"]["thresholdsStyle"] = {"mode": "dashed"}
    overrides = [
        {
            "matcher": {"id": "byName", "options": name},
            "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": color}}],
        }
        for name, color in (colors or {}).items()
    ]
    return {
        "type": "timeseries",
        "title": title,
        "description": desc,
        "datasource": PROM,
        "gridPos": {"x": x, "y": y, "w": w, "h": 8},
        "targets": targets,
        "fieldConfig": {"defaults": defaults, "overrides": overrides},
        "options": {
            "legend": {"displayMode": "list", "placement": "bottom", "showLegend": True},
            "tooltip": {"mode": "multi", "sort": "desc"},
        },
    }


def quantile(q: float) -> str:
    return (
        f"histogram_quantile({q}, sum by (le) (rate(fraud_score_latency_seconds_bucket[$__rate_interval])))"
    )


def dashboard() -> dict[str, Any]:
    degraded_ratio = (
        "sum(rate(fraud_degraded_total[10m])) / clamp_min(sum(rate(fraud_decisions_total[10m])), 1e-9)"
    )
    panels = [
        stat(
            "Score p95 (5 min)",
            "histogram_quantile(0.95, sum by (le) (rate(fraud_score_latency_seconds_bucket[5m])))",
            "s",
            thresholds((None, GOOD), (0.045, WARNING), (0.05, CRITICAL)),
            0,
            "SLO: p95 < 50 ms over 5 minutes (alert ScoreLatencyHigh).",
        ),
        stat(
            "Degraded share (10 min)",
            degraded_ratio,
            "percentunit",
            thresholds((None, GOOD), (0.005, WARNING), (0.01, CRITICAL)),
            6,
            "SLO: under 1% of decisions degraded over 10 minutes (alert DegradedDecisions).",
        ),
        stat(
            "Decisions per second",
            "sum(rate(fraud_decisions_total[1m]))",
            "reqps",
            thresholds((None, "#3987e5")),
            12,
            "All actions, 1-minute rate.",
        ),
        stat(
            "Oldest unsent outbox row",
            "max(fraud_outbox_oldest_unsent_age_seconds)",
            "s",
            thresholds((None, GOOD), (30, WARNING), (60, CRITICAL)),
            18,
            "SLO: under 60 s (alert OutboxUnsentAge).",
        ),
        series(
            "Score latency",
            [
                target(quantile(q), name, ref)
                for (name, q), ref in zip((("p50", 0.5), ("p95", 0.95), ("p99", 0.99)), "ABC", strict=True)
            ],
            "s",
            (0, 4, 12),
            "End-to-end Score latency quantiles; dashed line: the 50 ms p95 SLO.",
            QUANTILES,
            (0.05, CRITICAL),
            soft_max=0.06,
        ),
        series(
            "Decisions by action",
            [target("sum by (action) (rate(fraud_decisions_total[$__rate_interval]))", "{{action}}")],
            "reqps",
            (12, 4, 12),
            "Decisions per second by action.",
            ACTIONS,
        ),
        series(
            "Degraded decisions by reason",
            [target("sum by (reason) (rate(fraud_degraded_total[$__rate_interval]))", "{{reason}}")],
            "reqps",
            (0, 12, 12),
            "Rules-only decisions: model or features unavailable.",
            {"model": SLOTS[5], "features": SLOTS[4]},
        ),
        series(
            "Feature updater lag",
            [
                target("max(fraud_updater_lag_seconds)", "slowest partition", "A"),
                target("quantile(0.5, fraud_updater_lag_seconds)", "median partition", "B"),
            ],
            "s",
            (12, 12, 12),
            "Age of the last event applied: slowest and median of the 12 partitions.",
            {"slowest partition": SLOTS[0], "median partition": SLOTS[1]},
        ),
        series(
            "Outbox and spool backlog",
            [
                target("max(fraud_outbox_unsent)", "outbox unsent", "A"),
                target("max(fraud_spool_depth)", "spool depth", "B"),
            ],
            "short",
            (0, 20, 12),
            "Decisions waiting to be published (outbox) or written to Postgres (disk spool).",
            {"outbox unsent": SLOTS[0], "spool depth": SLOTS[1]},
        ),
        series(
            "Drops and errors",
            [
                target("sum(rate(fraud_dlq_total[$__rate_interval]))", "dead-lettered", "A"),
                target("sum(rate(fraud_rule_errors_total[$__rate_interval]))", "rule errors", "B"),
                target("sum(rate(fraud_shadow_dropped_total[$__rate_interval]))", "shadow dropped", "C"),
                target("sum(rate(fraud_publish_dropped_total[$__rate_interval]))", "publish dropped", "D"),
            ],
            "reqps",
            (12, 20, 12),
            "Per-second rates of messages or evaluations lost or diverted.",
            dict(
                zip(
                    ("dead-lettered", "rule errors", "shadow dropped", "publish dropped"), SLOTS, strict=False
                )
            ),
        ),
        series(
            "LLM spend",
            [target("sum by (model) (increase(fraud_llm_cost_usd_total[1h]))", "{{model}}")],
            "currencyUSD",
            (0, 28, 8),
            "USD per hour at list price, by model (offline runs report 0).",
        ),
        series(
            "Case summaries by status",
            [target("sum by (status) (rate(fraud_summary_status_total[$__rate_interval]))", "{{status}}")],
            "reqps",
            (8, 28, 8),
            "ok, fallback (templated, providers failed) or unfaithful (no evidence survived).",
            {"ok": GOOD, "fallback": WARNING, "unfaithful": CRITICAL},  # states, named in the legend
        ),
        series(
            "LLM calls by outcome",
            [target("sum by (outcome) (rate(fraud_llm_calls_total[$__rate_interval]))", "{{outcome}}")],
            "reqps",
            (16, 28, 8),
            "Provider attempts per second, ok or error.",
            {"ok": GOOD, "error": CRITICAL},
        ),
    ]
    for i, p in enumerate(panels, start=1):
        p["id"] = i
    return {
        "uid": "fraud",
        "title": "Fraud scoring",
        "tags": ["fraud"],
        "timezone": "utc",
        "schemaVersion": 39,
        "version": 1,
        "editable": False,
        "refresh": "10s",
        "time": {"from": "now-30m", "to": "now"},
        "panels": panels,
        "links": [
            {"title": "Traces (Jaeger)", "type": "link", "url": "http://localhost:8592/search?service=scorer"}
        ],
    }


def render() -> str:
    return json.dumps(dashboard(), indent=2) + "\n"


def main() -> int:
    OUT.write_text(render(), encoding="utf-8")
    print(f"wrote {OUT.relative_to(ROOT)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
