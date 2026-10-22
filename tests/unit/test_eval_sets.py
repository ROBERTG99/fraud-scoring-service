"""Eval datasets: counts, disjoint splits, labels by construction, reproducibility (seed 7)."""

from __future__ import annotations

import hashlib
import json
from collections import Counter
from pathlib import Path
from typing import Any

import pytest
import yaml

from evalgate.contracts import Case
from scripts import gen_eval_sets as gen

ROOT = Path(__file__).resolve().parents[2]
DATASETS = ROOT / "evals" / "datasets"
SETS = {"decisions": (60, 90), "summaries": (48, 72), "judge_calibration": (40, 40)}


def rows(name: str, part: str) -> list[dict[str, Any]]:
    path = DATASETS / name / f"{part}.jsonl"
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


@pytest.mark.parametrize("name", sorted(SETS))
def test_splits_disjoint_and_counts(name: str) -> None:
    dev, test = rows(name, "dev"), rows(name, "test")
    assert (len(dev), len(test)) == SETS[name]
    assert {r["split"] for r in dev} == {"dev"} and {r["split"] for r in test} == {"test"}
    dev_ids, test_ids = {r["id"] for r in dev}, {r["id"] for r in test}
    assert len(dev_ids) == len(dev) and len(test_ids) == len(test), "duplicate ids"
    assert not dev_ids & test_ids, "an id is in both splits"
    # Every case is a valid upstream evalgate row, and the suite file is exactly dev then test.
    for r in dev + test:
        Case.model_validate(r)
    assert rows(name, "cases.v1") == dev + test
    # Every template is represented in both splits (stratified), so dev tuning covers what test measures.
    assert {r["tags"][0] for r in dev} == {r["tags"][0] for r in test}


def test_manifest_pins_every_file() -> None:
    manifest = json.loads((DATASETS / "MANIFEST.json").read_text(encoding="utf-8"))
    assert manifest["seed"] == gen.SEED and manifest["generator"] == "scripts/gen_eval_sets.py"
    files = sorted(p.relative_to(DATASETS).as_posix() for p in DATASETS.rglob("*.jsonl"))
    assert sorted(manifest["files"]) == files
    for rel, digest in manifest["files"].items():
        assert hashlib.sha256((DATASETS / rel).read_bytes()).hexdigest() == digest, rel


def test_generation_is_byte_identical_with_seed_7() -> None:
    for rel, content in gen.build(gen.SEED).items():
        assert (DATASETS / rel).read_text(encoding="utf-8") == content, rel


def test_decision_cases_are_consistent_with_their_transaction() -> None:
    spec = yaml.safe_load((ROOT / "features" / "spec.yaml").read_text(encoding="utf-8"))
    names = [f["name"] for f in spec["features"]]
    codes = spec["channel_codes"]
    rules = yaml.safe_load((ROOT / "rules" / "rules.v2026.10.2.yaml").read_text(encoding="utf-8"))
    known = {r["reason_code"] for r in rules["rules"]} | {m["code"] for m in rules["model_reason_codes"]}
    known |= {"MODEL_UNAVAILABLE", "FEATURES_UNAVAILABLE"}
    templates: Counter[str] = Counter()
    for r in rows("decisions", "cases.v1"):
        tx, feats, exp = r["input"]["transaction"], r["input"]["features"], r["expected"]
        templates[r["tags"][0]] += 1
        assert set(exp["reason_codes"]) <= known and exp["reason_codes"] == sorted(exp["reason_codes"])
        assert exp["action"] in {"APPROVE", "REVIEW", "DECLINE"}
        if feats is None:
            assert exp["degraded"] and "FEATURES_UNAVAILABLE" in exp["reason_codes"]
            continue
        assert sorted(feats) == sorted(names)
        assert feats["amount_minor"] == tx["amount_minor"] and feats["channel"] == codes[tx["channel"]]
        assert feats["hour_of_day_utc"] == int(tx["event_ts"][11:13])
        assert exp["degraded"] == ("unavailable" in r["input"]["model"])
    assert len(templates) == 15 and set(templates.values()) == {10}


def test_summary_cases_plant_pii_and_cover_every_pattern() -> None:
    cases = rows("summaries", "cases.v1")
    patterns = Counter(r["expected"]["pattern"] for r in cases)
    assert patterns == {
        "account_takeover": 30,
        "card_testing": 20,
        "mule_account": 20,
        "friendly_fraud": 20,
        "unclear": 30,
    }
    planted = [r for r in cases if r["meta"]["pii"]]
    assert len(planted) >= 30
    for r in planted:
        names = [e["payload"].get("merchant_name", "") for e in r["input"]["events"]]
        assert all(any(p in n for n in names) for p in r["meta"]["pii"])
    assert all(r["input"]["decision"]["action"] == "REVIEW" for r in cases)


def test_judge_labels_are_balanced() -> None:
    for part in ("dev", "test"):
        labels = Counter(r["expected"]["useful"] for r in rows("judge_calibration", part))
        assert labels[True] == labels[False] == 20
