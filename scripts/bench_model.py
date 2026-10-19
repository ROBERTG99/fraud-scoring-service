"""make bench-model: single-row latency of the BentoML model server (NFR-4: p95 < 8 ms).

Uses FRAUD_MODEL_URL when a server is already running (compose), otherwise starts
`bentoml serve` on 127.0.0.1:8514 with FRAUD_MODEL_DIR=artifacts/model. Writes an evalgate.report.v1
JSON to evals/reports/model_latency.json and exits 1 when the budget is missed.
"""

from __future__ import annotations

import json
import os
import statistics
import subprocess
import sys
import time
from datetime import UTC, datetime
from pathlib import Path

import httpx

ROOT = Path(__file__).resolve().parents[1]
BUDGET_P95_MS = 8.0
WARMUP, N = 200, 2000


def wait_ready(url: str, deadline_s: float) -> bool:
    end = time.monotonic() + deadline_s
    while time.monotonic() < end:
        try:
            if httpx.get(f"{url}/readyz", timeout=1).status_code == 200:
                return True
        except httpx.HTTPError:
            pass
        time.sleep(0.25)
    return False


def percentile(values: list[float], q: float) -> float:
    s = sorted(values)
    return s[min(len(s) - 1, round(q / 100 * (len(s) - 1)))]


def main() -> int:
    url = os.environ.get("FRAUD_MODEL_URL", "http://127.0.0.1:8514")
    proc = None
    if not wait_ready(url, 1):
        model_dir = ROOT / "artifacts" / "model"
        if not (model_dir / "model.onnx").is_file():
            subprocess.run(["make", "train"], cwd=ROOT, check=True)
        env = {
            **os.environ,
            "FRAUD_MODEL_DIR": str(model_dir),
            "FRAUD_MODEL_CACHE": str(ROOT / ".cache" / "model-cache"),
            # A local benchmark serves the just-trained, unpinned model: development mode only (D-063).
            "FRAUD_ENV": "dev",
        }
        env.pop("MLFLOW_TRACKING_URI", None)
        proc = subprocess.Popen(
            [
                sys.executable,
                "-m",
                "bentoml",
                "serve",
                "serving.bentoml.service:FraudModel",
                "--host",
                "127.0.0.1",
                "--port",
                "8514",
            ],
            cwd=ROOT,
            env=env,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        url = "http://127.0.0.1:8514"
        if not wait_ready(url, 90):
            proc.terminate()
            print("bench-model: model server did not become ready")
            return 1
    golden = json.loads((ROOT / "tests/fixtures/model/golden.json").read_text(encoding="utf-8"))["vectors"]
    body = {"inputs": [golden[2]["features"]], "alias": "production"}
    latencies: list[float] = []
    try:
        with httpx.Client(base_url=url, timeout=2) as client:
            for i in range(WARMUP + N):
                t0 = time.perf_counter()
                resp = client.post("/predict", json=body)
                dt = (time.perf_counter() - t0) * 1000
                resp.raise_for_status()
                if i >= WARMUP:
                    latencies.append(dt)
            version = resp.json()["model_version"]
    finally:
        if proc is not None:
            proc.terminate()
            proc.wait(timeout=20)
    p50, p95, p99 = (percentile(latencies, q) for q in (50, 95, 99))
    passed = p95 < BUDGET_P95_MS
    report = {
        "schema": "evalgate.report.v1",
        "suite": "model_latency",
        "run_id": datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ"),
        "created_at": datetime.now(UTC).isoformat(),
        "git_sha": subprocess.run(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, capture_output=True, text=True, check=False
        ).stdout.strip(),
        "mode": "offline",
        "models": [version],
        "prompt_hashes": {},
        "n_cases": len(latencies),
        "metrics": {
            "p95_ms": {
                "value": p95,
                "threshold": BUDGET_P95_MS,
                "direction": "lower",
                "passed": passed,
                "ci_low": None,
                "ci_high": None,
            }
        },
        "cost_usd_total": 0.0,
        "cost_usd_per_case": 0.0,
        "latency_p50_ms": p50,
        "latency_p95_ms": p95,
        "cases": [],
        "extensions": {
            "p99_ms": p99,
            "mean_ms": statistics.fmean(latencies),
            "requests": N,
            "warmup": WARMUP,
            "url": url,
        },
    }
    out = ROOT / "evals" / "reports" / "model_latency.json"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(
        f"bench-model: {N} single-row requests p50={p50:.2f} ms p95={p95:.2f} ms p99={p99:.2f} ms (budget p95 < {BUDGET_P95_MS} ms) -> {'PASS' if passed else 'FAIL'}"
    )
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
