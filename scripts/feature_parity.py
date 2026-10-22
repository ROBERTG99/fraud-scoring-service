"""Train/serve feature parity.

1. Compute offline features (training/features.py) for tests/fixtures/parity/events.jsonl.
2. Run the Go online path (Redis Lua, in event order) via TestOnlineOfflineParity, which compares every
   transaction's 13 features with the offline values and writes the online values.
3. Compare again here and print the mismatch count. Exit 1 on any mismatch.

`--write-golden` refreshes tests/fixtures/parity/offline_features.csv (used by `go test` alone).
"""

from __future__ import annotations

import argparse
import csv
import os
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from training.dataset import load_events  # noqa: E402
from training.features import compute_features, feature_names  # noqa: E402

FIXTURE = ROOT / "tests" / "fixtures" / "parity" / "events.jsonl"
GOLDEN = ROOT / "tests" / "fixtures" / "parity" / "offline_features.csv"
OUT_DIR = ROOT / "artifacts" / "parity"


def write_offline(path: Path) -> int:
    fm = compute_features(load_events(FIXTURE))
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8", newline="") as fh:
        w = csv.writer(fh)
        w.writerow(["transaction_id", *feature_names()])
        for tx_id, row in zip(fm.transaction_id, fm.values, strict=True):
            w.writerow([tx_id, *(repr(float(v)) for v in row)])
    return len(fm.transaction_id)


def read(path: Path) -> dict[str, list[float]]:
    with path.open(encoding="utf-8", newline="") as fh:
        r = csv.reader(fh)
        next(r)
        return {row[0]: [float(v) for v in row[1:]] for row in r}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--write-golden", action="store_true")
    args = ap.parse_args()
    if args.write_golden:
        n = write_offline(GOLDEN)
        print(f"wrote {GOLDEN.relative_to(ROOT)} ({n} transactions)")
        return 0
    offline = OUT_DIR / "offline.csv"
    online = OUT_DIR / "online.csv"
    n = write_offline(offline)
    env = {**os.environ, "FRAUD_PARITY_OFFLINE": str(offline), "FRAUD_PARITY_ONLINE_OUT": str(online)}
    go = subprocess.run(
        [
            "go",
            "test",
            "-count=1",
            "-tags",
            "integration",
            "-run",
            "TestOnlineOfflineParity",
            "./tests/integration/",
        ],
        cwd=ROOT,
        env=env,
        check=False,
        capture_output=True,
        text=True,
    )
    if go.returncode != 0:
        print(go.stdout[-4000:], go.stderr[-2000:], sep="\n")
        print("feature-parity: Go online run failed")
        return 1
    off, on = read(offline), read(online)
    names = feature_names()
    mismatches = 0
    for tx_id, values in off.items():
        got = on.get(tx_id)
        if got is None:
            mismatches += 1
            continue
        for name, a, b in zip(names, values, got, strict=True):
            if a != b:
                mismatches += 1
                if mismatches <= 10:
                    print(f"mismatch {tx_id} {name}: offline={a!r} online={b!r}")
    print(f"feature-parity: {n} transactions x {len(names)} features, {mismatches} mismatches")
    return 0 if mismatches == 0 and len(on) == len(off) else 1


if __name__ == "__main__":
    raise SystemExit(main())
