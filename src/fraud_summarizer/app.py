"""FastAPI application for the case summarizer (port 8513)."""

from __future__ import annotations

from fastapi import FastAPI

app = FastAPI(title="fraud case-summarizer", version="0.1.0")


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"status": "ok"}
