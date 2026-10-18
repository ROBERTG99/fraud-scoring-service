"""Postgres access for the summarizer: case facts in, summaries and step records out (spec §4.5).

The summarizer only writes `case_summaries`, `step_records`, `processed_messages` and `summary_attempts`; it
never writes `decisions`.
"""

from __future__ import annotations

import json
from dataclasses import asdict
from datetime import datetime
from typing import Any

from psycopg.rows import dict_row
from psycopg_pool import AsyncConnectionPool

from fraud_summarizer.pipeline import MAX_EVENTS, CaseContext, PipelineResult, StepRecord

STATEMENT_TIMEOUT_MS = 5_000

_LOAD_CASE = """
SELECT rc.case_id::text AS case_id, d.decision_id::text AS decision_id, d.transaction_id, d.account_id,
       d.action, d.score, d.reason_codes, d.model_version, d.rules_version, d.degraded, d.decided_at,
       d.features, d.tx
FROM decisions d JOIN review_cases rc ON rc.decision_id = d.decision_id
WHERE d.decision_id = %s
"""

# The transaction itself comes from decisions.tx (written with the decision); account_events is filled
# asynchronously by the feature-updater, so it may not have it yet. Only events up to the transaction count.
_LOAD_EVENTS = """
SELECT kind, event_ts, payload FROM account_events
WHERE account_id = %s AND event_id <> %s AND event_ts <= %s::timestamptz
ORDER BY event_ts DESC LIMIT %s
"""

_INSERT_SUMMARY = """
INSERT INTO case_summaries (case_id, status, summary, pattern, faithfulness, model, prompt_version, cost_usd)
VALUES (%(case_id)s, %(status)s, %(summary)s, %(pattern)s, %(faithfulness)s, %(model)s, %(prompt_version)s,
        %(cost_usd)s)
ON CONFLICT (case_id) DO NOTHING
"""

_UPSERT_SUMMARY = """
INSERT INTO case_summaries (case_id, status, summary, pattern, faithfulness, model, prompt_version, cost_usd)
VALUES (%(case_id)s, %(status)s, %(summary)s, %(pattern)s, %(faithfulness)s, %(model)s, %(prompt_version)s,
        %(cost_usd)s)
ON CONFLICT (case_id) DO UPDATE SET status = EXCLUDED.status, summary = EXCLUDED.summary,
  pattern = EXCLUDED.pattern, faithfulness = EXCLUDED.faithfulness, model = EXCLUDED.model,
  prompt_version = EXCLUDED.prompt_version, cost_usd = EXCLUDED.cost_usd, created_at = now()
"""

_INSERT_STEP = """
INSERT INTO step_records (run_id, task_id, step, node, kind, model, prompt_version, input_tokens, output_tokens,
                          cost_usd, latency_ms, status, error, started_at)
VALUES (%(run_id)s, %(task_id)s, %(step)s, %(node)s, %(kind)s, %(model)s, %(prompt_version)s, %(input_tokens)s,
        %(output_tokens)s, %(cost_usd)s, %(latency_ms)s, %(status)s, %(error)s, %(started_at)s)
ON CONFLICT (run_id, step) DO NOTHING
"""


def _event_ts(value: Any) -> str:
    if isinstance(value, datetime):
        return value.isoformat().replace("+00:00", "Z")
    return str(value)


def _tx_event(row: dict[str, Any]) -> dict[str, Any] | None:
    tx = row.get("tx") or {}
    if not tx:
        return None
    payload = {k: v for k, v in tx.items() if v not in (None, "")}
    payload.pop("merchant_id", None)
    return {"kind": "transaction", "event_ts": payload.get("event_ts", ""), "payload": payload}


class PostgresRepository:
    """CaseRepository and processed-message store on a psycopg async pool."""

    def __init__(self, pool: AsyncConnectionPool) -> None:
        self.pool = pool

    @classmethod
    def create_pool(cls, dsn: str, *, max_size: int = 8) -> AsyncConnectionPool:
        return AsyncConnectionPool(
            dsn,
            min_size=1,
            max_size=max_size,
            open=False,
            timeout=5.0,
            kwargs={"options": f"-c statement_timeout={STATEMENT_TIMEOUT_MS}", "connect_timeout": 5},
        )

    async def ping(self) -> None:
        async with self.pool.connection() as conn:
            await conn.execute("SELECT 1")

    async def load_case(self, decision_id: str) -> CaseContext | None:
        async with self.pool.connection() as conn, conn.cursor(row_factory=dict_row) as cur:
            await cur.execute(_LOAD_CASE, (decision_id,))
            row = await cur.fetchone()
            if row is None:
                return None
            events: list[dict[str, Any]] = []
            own = _tx_event(row)
            if own is not None:
                events.append(own)
            as_of = (row.get("tx") or {}).get("event_ts") or row["decided_at"]
            await cur.execute(
                _LOAD_EVENTS, (row["account_id"], row["transaction_id"], as_of, MAX_EVENTS - len(events))
            )
            for ev in await cur.fetchall():
                events.append(
                    {"kind": ev["kind"], "event_ts": _event_ts(ev["event_ts"]), "payload": ev["payload"]}
                )
        decision = {
            "decision_id": row["decision_id"],
            "transaction_id": row["transaction_id"],
            "account_id": row["account_id"],
            "action": row["action"],
            "score": row["score"],
            "reason_codes": list(row["reason_codes"] or []),
            "model_version": row["model_version"],
            "rules_version": row["rules_version"],
            "degraded": row["degraded"],
            "decided_at": _event_ts(row["decided_at"]),
        }
        features = {k: float(v) for k, v in (row["features"] or {}).items()}
        return CaseContext(case_id=row["case_id"], decision=decision, features=features, events=events)

    async def save_summary(self, result: PipelineResult, *, replace: bool = False) -> None:
        s = result.summary
        params = {
            "case_id": result.case_id,
            "status": result.status,
            "summary": json.dumps(s.model_dump(mode="json"), ensure_ascii=False),
            "pattern": s.pattern,
            "faithfulness": s.faithfulness,
            "model": s.model,
            "prompt_version": s.prompt_version,
            "cost_usd": round(result.cost_usd, 6),
        }
        async with self.pool.connection() as conn:
            await conn.execute(_UPSERT_SUMMARY if replace else _INSERT_SUMMARY, params)

    async def save_steps(self, steps: list[StepRecord]) -> None:
        if not steps:
            return
        rows = []
        for s in steps:
            r = asdict(s)
            r["cost_usd"] = round(float(r["cost_usd"]), 6)
            rows.append(r)
        async with self.pool.connection() as conn, conn.cursor() as cur:
            await cur.executemany(_INSERT_STEP, rows)

    async def is_processed(self, consumer: str, key: str) -> bool:
        async with self.pool.connection() as conn:
            cur = await conn.execute(
                "SELECT 1 FROM processed_messages WHERE consumer = %s AND message_key = %s", (consumer, key)
            )
            return await cur.fetchone() is not None

    async def mark_processed(self, consumer: str, key: str) -> bool:
        """Record a message; False when it was already recorded."""
        async with self.pool.connection() as conn:
            cur = await conn.execute(
                "INSERT INTO processed_messages (consumer, message_key) VALUES (%s, %s) "
                "ON CONFLICT DO NOTHING RETURNING 1",
                (consumer, key),
            )
            return await cur.fetchone() is not None

    async def begin_attempt(self, consumer: str, key: str) -> int:
        """Record one more paid summary attempt for a message; return the count so far (D-048)."""
        async with self.pool.connection() as conn:
            cur = await conn.execute(
                "INSERT INTO summary_attempts (consumer, message_key) VALUES (%s, %s) "
                "ON CONFLICT (consumer, message_key) DO UPDATE "
                "SET attempts = summary_attempts.attempts + 1, last_attempt_at = now() RETURNING attempts",
                (consumer, key),
            )
            row = await cur.fetchone()
            if row is None:
                raise RuntimeError("summary_attempts upsert returned no row")
            return int(row[0])

    async def decision_for_case(self, case_id: str) -> str | None:
        async with self.pool.connection() as conn:
            cur = await conn.execute(
                "SELECT decision_id::text FROM review_cases WHERE case_id = %s", (case_id,)
            )
            row = await cur.fetchone()
            return str(row[0]) if row else None

    async def get_summary(self, case_id: str) -> dict[str, Any] | None:
        async with self.pool.connection() as conn, conn.cursor(row_factory=dict_row) as cur:
            await cur.execute(
                "SELECT case_id::text AS case_id, status, summary, cost_usd, created_at "
                "FROM case_summaries WHERE case_id = %s",
                (case_id,),
            )
            row = await cur.fetchone()
        if row is None:
            return None
        summary = dict(row["summary"])
        summary.pop("facts", None)  # facts are served with the case (CaseService.GetCase)
        return {
            "case_id": row["case_id"],
            "status": row["status"],
            "summary": summary,
            "cost_usd": float(row["cost_usd"]),
            "created_at": _event_ts(row["created_at"]),
        }
