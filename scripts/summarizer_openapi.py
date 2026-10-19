"""Write the case-summarizer OpenAPI document (FastAPI) to gen/openapi/summarizer.json for the console types.

Usage: python scripts/summarizer_openapi.py [out.json]   (run by scripts/proto.sh)
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from fraud_summarizer.app import create_app  # noqa: E402


def main(argv: list[str]) -> int:
    out = Path(argv[1]) if len(argv) > 1 else ROOT / "gen" / "openapi" / "summarizer.json"
    doc = create_app().openapi()
    out.write_text(json.dumps(doc, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
