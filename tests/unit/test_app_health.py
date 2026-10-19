from __future__ import annotations

from fastapi.testclient import TestClient

from fraud_summarizer.app import app


def test_healthz_ok() -> None:
    client = TestClient(app)
    response = client.get("/healthz")
    assert response.status_code == 200
    assert response.json() == {"status": "ok"}
