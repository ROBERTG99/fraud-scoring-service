"""Model eval report.

Scores the temporal test split of the eval data (`make gen-data SIZE=eval`, 2M events) with the serving
artifact: the ONNX graph loaded through `serving/bentoml/runtime.py`, so the probabilities are exactly what
the scorer receives. Rule flags come from the real Go rule engine (`cmd/evaldecide`, model score 0: any
REVIEW or DECLINE is a rule flag). Writes an `evalgate.report.v1`: metric values are computed on the whole
test split; `cases` are 20 account shards with the same metrics per shard, the paired units
`evalgate check` compares against a baseline (D-033).

No threshold is fit on test (D-037): the gated combined recall uses the threshold chosen on valid, the band
metrics use the active rule set's review and decline thresholds (what the scorer applies), and the per-band
calibration gap checks the probabilities those bands rely on. The test-oracle recall is reported only.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import zlib
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import numpy as np
import numpy.typing as npt
import yaml

from evalgate.contracts import CaseReport, MetricSummary, Report
from training.dataset import load_events, load_labels
from training.features import compute_features, feature_names
from training.metrics import expected_calibration_error, pr_auc, recall_at_fpr, recall_at_fpr_with_flags

ROOT = Path(__file__).resolve().parents[1]
SUITE = ROOT / "evals" / "suites" / "model.yaml"
N_SHARDS = 20
GEN_VERSION = 2  # cmd/gen/generator.go GenVersion: data from another generator version is refused
CHANNELS = {1: "CARD_PRESENT", 2: "ECOMMERCE", 3: "P2P"}
F64 = npt.NDArray[np.float64]


@dataclass
class EvalData:
    y: npt.NDArray[np.int8]
    p: F64  # calibrated model probability
    flags: npt.NDArray[np.bool_]  # a rule decided REVIEW or DECLINE
    accounts: npt.NDArray[np.str_]


@dataclass(frozen=True)
class Bands:
    version: str
    review: float
    decline: float


def active_bands(rules_dir: Path = ROOT / "rules") -> Bands:
    """Thresholds of the highest-versioned rules.v*.yaml, the set internal/rules.NewEngine loads."""

    def version(path: Path) -> tuple[int, ...]:
        return tuple(int(x) for x in path.name.removeprefix("rules.v").removesuffix(".yaml").split("."))

    path = max(rules_dir.glob("rules.v*.yaml"), key=version)
    doc = yaml.safe_load(path.read_text(encoding="utf-8"))
    t = doc["thresholds"]
    return Bands(version=str(doc["version"]), review=float(t["review"]), decline=float(t["decline"]))


def band_calibration_gap(y: npt.ArrayLike, prob: npt.ArrayLike, review: float, decline: float) -> float:
    """Largest |mean probability - fraud rate| over the review [review, decline) and decline [decline, 1] bands.

    ECE is dominated by the ~99% legitimate mass near 0 and cannot fail at a 1% base rate; the bands are
    where a decision depends on the probability's value. An empty band gives 1.0 (nothing to act on).
    """
    y_arr = np.asarray(y, dtype=np.float64)
    p = np.asarray(prob, dtype=np.float64)
    gap = 0.0
    for mask in ((p >= review) & (p < decline), p >= decline):
        if not mask.any():
            return 1.0
        gap = max(gap, abs(float(p[mask].mean()) - float(y_arr[mask].mean())))
    return gap


@dataclass(frozen=True)
class MetricDef:
    name: str
    direction: str
    gate: bool
    threshold: float | None


def load_thresholds(path: Path = SUITE) -> dict[str, MetricDef]:
    spec = yaml.safe_load(path.read_text(encoding="utf-8"))
    assert spec["schema"] == "fraud.model_suite.v1"
    return {
        m["name"]: MetricDef(
            m["name"], m.get("direction", "higher"), bool(m.get("gate", True)), m.get("threshold")
        )
        for m in spec["metrics"]
    }


def shard_of(accounts: npt.ArrayLike) -> npt.NDArray[np.int64]:
    """Stable shard per account (CRC-32), so every run pairs the same accounts."""
    return np.asarray([zlib.crc32(str(a).encode()) % N_SHARDS for a in np.asarray(accounts)], dtype=np.int64)


def _rate(flagged: npt.NDArray[np.bool_], mask: npt.NDArray[np.bool_]) -> float:
    return float(flagged[mask].mean()) if mask.any() else 0.0


def metrics_of(d: EvalData, max_fpr: float, t_valid: float, bands: Bands) -> dict[str, float]:
    """Metrics at fixed thresholds (t_valid from valid, bands from the rule set), so shards are comparable."""
    y = d.y.astype(bool)
    combined = d.flags | (d.p >= t_valid)
    review = d.flags | (d.p >= bands.review)
    return {
        "recall_at_1pct_fpr_combined": _rate(combined, y),
        "fpr_combined": _rate(combined, ~y),
        "recall_review_band": _rate(review, y),
        "fpr_review_band": _rate(review, ~y),
        "recall_decline_band": _rate(d.p >= bands.decline, y),
        "band_calibration_gap": band_calibration_gap(d.y, d.p, bands.review, bands.decline),
        "pr_auc": pr_auc(d.y, d.p) if y.any() else 0.0,
        "ece": expected_calibration_error(d.y, d.p),
        "recall_at_1pct_fpr_combined_test_oracle": recall_at_fpr_with_flags(d.y, d.p, d.flags, max_fpr)[0],
        "recall_at_1pct_fpr_model": recall_at_fpr(d.y, d.p, max_fpr)[0],
        "recall_at_1pct_fpr_rules": _rate(d.flags, y),
    }


def _subset(d: EvalData, mask: npt.NDArray[np.bool_]) -> EvalData:
    return EvalData(y=d.y[mask], p=d.p[mask], flags=d.flags[mask], accounts=d.accounts[mask])


def operating_point_from_valid(valid: EvalData, test: EvalData, max_fpr: float) -> dict[str, float]:
    """Choose the combined threshold on valid (as a deployment would), then measure it on test."""
    _, _, t = recall_at_fpr_with_flags(valid.y, valid.p, valid.flags, max_fpr)
    y = test.y.astype(bool)
    flagged = test.flags | (test.p >= t)
    return {
        "threshold": float(t),
        "test_recall": float(flagged[y].mean()) if y.any() else 0.0,
        "test_fpr": float(flagged[~y].mean()) if (~y).any() else 0.0,
    }


def build_report(
    test: EvalData,
    *,
    valid: EvalData,
    thresholds: dict[str, MetricDef],
    model_version: str,
    extensions: dict[str, Any],
    max_fpr: float = 0.01,
    bands: Bands | None = None,
) -> Report:
    bands = bands or active_bands()
    op = operating_point_from_valid(valid, test, max_fpr)
    overall = metrics_of(test, max_fpr, op["threshold"], bands)
    shards = shard_of(test.accounts)
    cases = []
    for k in range(N_SHARDS):
        scores = metrics_of(_subset(test, shards == k), max_fpr, op["threshold"], bands)
        # ~30 transactions per band per shard: a per-shard gap is noise, so each shard carries the pooled
        # value and the paired check compares the whole split (D-037).
        scores["band_calibration_gap"] = overall["band_calibration_gap"]
        cases.append(
            CaseReport(
                id=f"shard-{k:02d}",
                split="test",
                scores={name: float(min(1.0, max(0.0, v))) for name, v in scores.items()},
                passed=True,
                cost_usd=0.0,
                latency_ms=0.0,
            )
        )
    summaries = {}
    for name, value in overall.items():
        m = thresholds[name]
        ok = m.threshold is None or (
            value >= m.threshold if m.direction == "higher" else value <= m.threshold
        )
        summaries[name] = MetricSummary(value=value, threshold=m.threshold, direction=m.direction, passed=ok)  # type: ignore[arg-type]
    y = test.y.astype(bool)
    now = datetime.now(UTC)
    ext = {
        "split": "test",
        "gates": {name: thresholds[name].gate for name in overall},
        "budget": {},
        "target_errors": 0,
        "metric_errors": {},
        "run_health": "ok",
        "error_rate": 0.0,
        "n_transactions": len(test.y),
        "n_fraud": int(y.sum()),
        "rules_fpr": float(test.flags[~y].mean()) if (~y).any() else 0.0,
        "operating_point_from_valid": op,
        "bands": {"rules_version": bands.version, "review": bands.review, "decline": bands.decline},
        "case_unit": f"{N_SHARDS} account shards (CRC-32 of account_id)",
        **extensions,
    }
    return Report(
        schema="evalgate.report.v1",
        suite="model",
        run_id=now.strftime("%Y%m%dT%H%M%SZ"),
        created_at=now.isoformat(),
        git_sha=os.environ.get("GITHUB_SHA") or _git_sha(),
        mode="offline",
        models=[model_version],
        n_cases=len(cases),
        metrics=summaries,
        cost_usd_total=0.0,
        cost_usd_per_case=0.0,
        latency_p50_ms=0.0,
        latency_p95_ms=0.0,
        cases=cases,
        extensions=ext,
    )


def _git_sha() -> str | None:
    try:
        out = subprocess.run(
            ["git", "rev-parse", "HEAD"],  # noqa: S607
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=True,
        )
    except (OSError, subprocess.CalledProcessError):
        return None
    return out.stdout.strip() or None


def rule_flags(rows: list[dict[str, Any]]) -> npt.NDArray[np.bool_]:
    """Run each row through `cmd/evaldecide` with model score 0; REVIEW or DECLINE means a rule fired."""
    binary = ROOT / "artifacts" / "bin" / "evaldecide"
    env = {**os.environ, "GOTOOLCHAIN": "auto"}
    build = ["go", "build", "-o", str(binary), "./cmd/evaldecide"]
    subprocess.run(build, cwd=ROOT, env=env, check=True)  # noqa: S603 (fixed arguments)
    payload = "".join(json.dumps(r, separators=(",", ":")) + "\n" for r in rows)
    out = subprocess.run([str(binary)], cwd=ROOT, input=payload, capture_output=True, text=True, check=True)  # noqa: S603
    decided = [json.loads(line) for line in out.stdout.splitlines()]
    if len(decided) != len(rows) or any(d.get("error") for d in decided):
        bad = next((d for d in decided if d.get("error")), None)
        raise RuntimeError(f"evaldecide failed: {len(decided)} of {len(rows)} rows, first error {bad}")
    return np.asarray([d["action"] != "APPROVE" for d in decided], dtype=bool)


def _rows(x: F64, idx: npt.NDArray[np.int64], events: Any, tx_ids: list[str]) -> list[dict[str, Any]]:
    names = feature_names()
    rows = []
    for row, i in zip(x, idx.tolist(), strict=True):
        ts = datetime.fromtimestamp(int(events.ts[i]) / 1000, tz=UTC)
        rows.append(
            {
                "id": events.event_id[i],
                "input": {
                    "transaction": {
                        "transaction_id": events.event_id[i],
                        "account_id": events.accounts[events.account[i]],
                        "amount_minor": int(events.amount[i]),
                        "currency": "USD",
                        "merchant_id": "",
                        "merchant_name": "",
                        "mcc": events.mccs[events.mcc[i]],
                        "channel": CHANNELS[int(events.channel[i])],
                        "country": "US",
                        "ip_prefix": "10.0.0.0/24",
                        "device_id": events.devices[events.device[i]],
                        "event_ts": ts.strftime("%Y-%m-%dT%H:%M:%S.") + f"{ts.microsecond // 1000:03d}Z",
                    },
                    "features": dict(zip(names, (float(v) for v in row), strict=True)),
                    "model": {"score": 0.0},
                },
            }
        )
    return rows


def go_gen_version() -> int:
    """GenVersion as declared in cmd/gen/generator.go."""
    for line in (ROOT / "cmd" / "gen" / "generator.go").read_text(encoding="utf-8").splitlines():
        if line.startswith("const GenVersion"):
            return int(line.split("=")[1].split("//")[0])
    raise RuntimeError("GenVersion not found in cmd/gen/generator.go")


def check_data(data_dir: Path) -> dict[str, Any]:
    manifest: dict[str, Any] = json.loads((data_dir / "manifest.json").read_text(encoding="utf-8"))
    if manifest.get("gen_version") != GEN_VERSION:
        raise ValueError(
            f"{data_dir} holds gen_version {manifest.get('gen_version')}, expected {GEN_VERSION}: "
            "regenerate with `make gen-data SIZE=eval`"
        )
    return manifest


def evaluate(data_dir: Path, model_dir: Path, max_fpr: float = 0.01) -> Report:
    from serving.bentoml.runtime import ModelRuntime

    manifest = check_data(data_dir)
    events = load_events(data_dir / "events.jsonl")
    labels = load_labels(data_dir / "labels.csv")
    fm = compute_features(events)
    pos = {tx: i for i, tx in enumerate(labels.transaction_id)}
    order = np.asarray([pos[tx] for tx in fm.transaction_id], dtype=np.int64)
    split = np.asarray(labels.split)[order]
    y_all = labels.is_fraud[order]
    # The artifact under evaluation is the one given here, checked against its model_meta.json (D-063).
    model = ModelRuntime(model_dir=str(model_dir), use_mlflow=False, require_pin=False).load("production")

    def part(name: str) -> EvalData:
        mask = split == name
        x = fm.values[mask]
        idx = fm.event_index[mask]
        p = np.asarray(model.predict(x.astype(np.float32)), dtype=np.float64)
        flags = rule_flags(_rows(x, idx, events, fm.transaction_id))
        accounts = np.asarray([events.accounts[a] for a in events.account[idx]])
        return EvalData(y=y_all[mask], p=p, flags=flags, accounts=accounts)

    test, valid = part("test"), part("valid")
    meta = json.loads((model_dir / "model_meta.json").read_text(encoding="utf-8"))
    extensions = {
        "data": {k: manifest[k] for k in ("events_sha256", "gen_version", "seed", "size", "events")},
        "model_dir": str(model_dir),
        "onnx_sha256": hashlib.sha256((model_dir / "model.onnx").read_bytes()).hexdigest(),
        "feature_spec": meta.get("feature_spec"),
    }
    return build_report(
        test,
        valid=valid,
        thresholds=load_thresholds(),
        model_version=model.version,
        extensions=extensions,
        max_fpr=max_fpr,
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Model eval report (evalgate.report.v1)")
    parser.add_argument("--data", type=Path, default=ROOT / "artifacts" / "data" / "eval")
    parser.add_argument("--model", type=Path, default=ROOT / "artifacts" / "model")
    parser.add_argument("--out", type=Path, default=ROOT / "evals" / "reports" / "model.json")
    args = parser.parse_args(argv)
    report = evaluate(args.data, args.model)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(
        json.dumps(report.model_dump(mode="json", by_alias=True), indent=2) + "\n", encoding="utf-8"
    )
    for name, s in report.metrics.items():
        bound = "" if s.threshold is None else f" (threshold {s.threshold}, {'pass' if s.passed else 'FAIL'})"
        print(f"{name} = {s.value:.4f}{bound}")
    print(f"wrote {args.out}")
    return 0 if all(s.passed for s in report.metrics.values()) else 1


if __name__ == "__main__":
    raise SystemExit(main())
