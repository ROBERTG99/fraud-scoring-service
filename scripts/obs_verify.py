"""Check that the observability profile shows data: every dashboard query, Grafana, cross-service traces.

Run by scripts/obs_host.sh after traffic. For each query of deploy/grafana/fraud.json it reports whether
Prometheus returns samples. Event-only series (a labelled counter that appears on its first event, such as
dead-lettered messages) may legitimately be absent and are reported as "no events", not hidden. It then
checks that Grafana loaded the dashboard and its datasources, and that Jaeger holds traces in which the
summarizer's span shares the trace of the scorer's `fraud.score` span (the Kafka `traceparent` header).
Exit 1 when a required query is empty, Grafana lacks the dashboard, or no cross-service trace is found.
"""

from __future__ import annotations

import argparse
import json
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
DASHBOARD = ROOT / "deploy" / "grafana" / "fraud.json"
# Labelled counters registered lazily: absent until their first event.
EVENT_ONLY = ("fraud_dlq_total", "fraud_rule_errors_total", "fraud_degraded_total")


def get(url: str) -> Any:
    if urllib.parse.urlsplit(url).scheme not in ("http", "https"):
        raise ValueError(f"only http(s) URLs: {url!r}")
    # The scheme is checked above, so urlopen cannot read file: or custom-scheme URLs.
    with urllib.request.urlopen(url, timeout=10) as resp:  # noqa: S310  # nosec B310
        return json.loads(resp.read())


def check_queries(prom: str) -> list[str]:
    missing = []
    dashboard = json.loads(DASHBOARD.read_text(encoding="utf-8"))
    for panel in dashboard["panels"]:
        for t in panel["targets"]:
            expr = t["expr"].replace("$__rate_interval", "1m")
            q = urllib.parse.urlencode({"query": expr})
            result = get(f"{prom}/api/v1/query?{q}")["data"]["result"]
            if result:
                values = ", ".join(f"{r['metric'] or ''} {float(r['value'][1]):.4g}" for r in result[:4])
                print(f"ok        {panel['title']} [{t['legendFormat']}]: {values}")
            elif any(m in expr for m in EVENT_ONLY):
                print(f"no events {panel['title']} [{t['legendFormat']}]")
            else:
                print(f"EMPTY     {panel['title']} [{t['legendFormat']}]: {expr}")
                missing.append(panel["title"])
    return missing


def check_grafana(grafana: str) -> bool:
    """The provisioned dashboard is loaded (anonymous viewer access, as in the profile)."""
    d = get(f"{grafana}/api/dashboards/uid/fraud")["dashboard"]
    ok = d["title"] == "Fraud scoring" and len(d["panels"]) > 0
    print(f"grafana   dashboard {d['title']!r}: {len(d['panels'])} panels" + ("" if ok else " MISSING"))
    return ok


def check_traces(jaeger: str) -> bool:
    # Only summarized decisions have a case, and so a Score span, in this stack: on a shared test broker the
    # summarizer also drains other runs' decisions (outcome missing_case), whose traces were never here.
    tags = urllib.parse.quote(json.dumps({"app.outcome": "summarized"}))
    q = f"service=case-summarizer&operation=summarizer.decision&tags={tags}&limit=50&lookback=1h"
    data = get(f"{jaeger}/api/traces?{q}")["data"]
    joined = 0
    for tr in data:
        services = {p["serviceName"] for p in tr["processes"].values()}
        ops = {s["operationName"] for s in tr["spans"]}
        if {"scorer", "case-summarizer"} <= services and {"fraud.score", "summarizer.decision"} <= ops:
            joined += 1
            if joined == 1:
                print(f"jaeger    trace {tr['traceID']}: {len(tr['spans'])} spans, operations {sorted(ops)}")
    print(f"jaeger    {joined} of {len(data)} decision traces include the scorer's fraud.score span")
    return joined > 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prometheus", required=True)
    parser.add_argument("--grafana", required=True)
    parser.add_argument("--jaeger", required=True)
    args = parser.parse_args(argv)
    missing = check_queries(args.prometheus)
    grafana_ok = check_grafana(args.grafana)
    traces_ok = check_traces(args.jaeger)
    ok = not missing and grafana_ok and traces_ok
    print(
        "obs-verify:", "PASS" if ok else f"FAIL (empty: {missing}, grafana {grafana_ok}, traces {traces_ok})"
    )
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
