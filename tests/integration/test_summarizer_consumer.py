"""T-29 done-when: the consumer writes a summary for a REVIEW event (real Postgres and Redpanda, FakeLLM)."""

from __future__ import annotations

import asyncio
import json
import time
import uuid
from collections.abc import Iterator
from pathlib import Path

import psycopg
import pytest
from aiokafka import AIOKafkaProducer
from aiokafka.admin import AIOKafkaAdminClient, NewTopic

from fraud_summarizer.consumer import CONSUMER_NAME, DecisionHandler, KafkaDecisionConsumer
from fraud_summarizer.llm.factory import build_gateway
from fraud_summarizer.llm.routes import load
from fraud_summarizer.pipeline import SummaryPipeline
from fraud_summarizer.repository import PostgresRepository
from tests.support.stack import Database, env, fresh_database

pytestmark = [pytest.mark.integration, pytest.mark.enable_socket]

DECISION_ID = str(uuid.uuid4())
TX_ID = str(uuid.uuid4())
CASE_ID = str(uuid.uuid4())
ACCOUNT = "acc_00000042"

FEATURES = {
    "tx_count_10m": 0,
    "tx_count_1h": 1,
    "tx_count_24h": 3,
    "amount_sum_1h_minor": 4200,
    "amount_minor": 94000,
    "distinct_devices_24h": 2,
    "new_device": 1,
    "minutes_since_pwd_change": 43200,
    "minutes_since_email_change": 2,
    "mcc_novel_30d": 1,
    "channel": 2,
    "hour_of_day_utc": 10,
    "amount_ratio_24h": 9.4,
}
TX = {
    "amount_minor": 94000,
    "currency": "USD",
    "merchant_id": "m_00042",
    "merchant_name": "Northwind Electronics",
    "mcc": "5732",
    "channel": "ECOMMERCE",
    "country": "US",
    "ip_prefix": "203.0.113.0/24",
    "device_id": "ab" * 32,
    "event_ts": "2026-09-20T10:03:00Z",
}


@pytest.fixture(scope="module")
def database(tmp_path_factory: pytest.TempPathFactory) -> Iterator[Database]:
    work: Path = tmp_path_factory.mktemp("summarizer")
    with fresh_database(work) as db:
        with psycopg.connect(db.owner_dsn, autocommit=True) as conn:
            conn.execute(
                "INSERT INTO decisions (decision_id, transaction_id, account_id, action, score, reason_codes,"
                " model_version, rules_version, degraded, features, latency_ms, decided_at, tx)"
                " VALUES (%s, %s, %s, 'REVIEW', 0.62, %s, 'fraud-gbdt/1', '2026.10.1', false, %s, 3.1,"
                " '2026-09-20T10:03:00.040Z', %s)",
                (
                    DECISION_ID,
                    TX_ID,
                    ACCOUNT,
                    ["NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", "MODEL_SCORE_ELEVATED"],
                    json.dumps(FEATURES),
                    json.dumps(TX),
                ),
            )
            conn.execute(
                "INSERT INTO review_cases (case_id, decision_id, account_id, status, priority)"
                " VALUES (%s, %s, %s, 'open', 0.62)",
                (CASE_ID, DECISION_ID, ACCOUNT),
            )
            conn.execute(
                "INSERT INTO account_events (event_id, account_id, kind, event_ts, payload) VALUES"
                " ('ev-login-1', %s, 'login', '2026-09-20T10:02:00Z', %s),"
                " ('ev-device-1', %s, 'device', '2026-09-20T10:01:00Z', %s),"
                " ('ev-future-1', %s, 'login', '2026-09-21T00:00:00Z', %s)",
                (
                    ACCOUNT,
                    json.dumps({"success": True, "method": "password", "device_id": "ab" * 32}),
                    ACCOUNT,
                    json.dumps({"change": "email_changed", "device_id": "ab" * 32}),
                    ACCOUNT,
                    json.dumps({"success": True, "method": "password"}),
                ),
            )
        yield db


def decision_message() -> bytes:
    return json.dumps(
        {
            "schema": "fraud.decision.v1",
            "decision_id": DECISION_ID,
            "transaction_id": TX_ID,
            "account_id": ACCOUNT,
            "action": "REVIEW",
            "score": 0.62,
            "reason_codes": ["NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", "MODEL_SCORE_ELEVATED"],
            "model_version": "fraud-gbdt/1",
            "rules_version": "2026.10.1",
            "degraded": False,
            "decided_at": "2026-09-20T10:03:00.040Z",
        }
    ).encode()


async def test_consumer_writes_summary_for_review_event(database: Database) -> None:
    brokers = env("FRAUD_TEST_KAFKA_BROKERS")
    suffix = uuid.uuid4().hex[:10]
    topic, dlq = f"decisions.it.{suffix}", f"events.dlq.it.{suffix}"
    admin = AIOKafkaAdminClient(bootstrap_servers=brokers)
    await admin.start()
    try:
        await admin.create_topics([NewTopic(t, num_partitions=3, replication_factor=1) for t in (topic, dlq)])
    finally:
        await admin.close()

    # franz-go producers (scorer, seed, replay) compress batches with snappy by default.
    producer = AIOKafkaProducer(bootstrap_servers=brokers, compression_type="snappy")
    await producer.start()
    try:
        for _ in range(2):  # at-least-once delivery: the same decision twice
            await producer.send_and_wait(topic, decision_message(), key=ACCOUNT.encode())
        await producer.send_and_wait(topic, b"{not json", key=ACCOUNT.encode())
    finally:
        await producer.stop()

    pool = PostgresRepository.create_pool(database.summarizer_dsn)
    await pool.open()
    repo = PostgresRepository(pool)
    gateway = build_gateway(load(env={"FRAUD_SUMMARY_PROVIDER": "fake"}))
    consumer = KafkaDecisionConsumer(
        DecisionHandler(SummaryPipeline(gateway, repo), repo),
        brokers,
        topic=topic,
        group=f"it-{suffix}",
        dlq_topic=dlq,
    )
    stop = asyncio.Event()
    task = asyncio.create_task(consumer.run(stop))
    try:
        deadline = time.monotonic() + 60
        row = None
        while time.monotonic() < deadline and row is None:
            assert not task.done(), task.exception()
            async with pool.connection() as conn:
                cur = await conn.execute(
                    "SELECT status, pattern, summary FROM case_summaries WHERE case_id = %s", (CASE_ID,)
                )
                row = await cur.fetchone()
            if row is None:
                await asyncio.sleep(0.2)
        assert row is not None, "no summary written within 60 s"
        status, pattern, summary = row
        assert (status, pattern) == ("ok", "account_takeover")
        assert summary["faithfulness"] == 1.0 and len(summary["evidence"]) == 3
        assert summary["facts"]["event.transaction.0.merchant_name"] == "Northwind Electronics"
        assert summary["facts"]["event.login.0.method"] == "password"
        assert not any(k.startswith("event.login.1") for k in summary["facts"])  # future event excluded

        # The invalid message is last on the partition: once it reaches the DLQ, the duplicate before it
        # has been handled too.
        deadline = time.monotonic() + 30
        dlq_count = 0
        while time.monotonic() < deadline and dlq_count == 0:
            dlq_count = await _count(brokers, dlq)
        assert dlq_count == 1
        async with pool.connection() as conn:
            cur = await conn.execute(
                "SELECT count(DISTINCT run_id), count(*) FROM step_records WHERE task_id = %s", (CASE_ID,)
            )
            runs, steps = await cur.fetchone() or (0, 0)
        assert (runs, steps) == (1, 6)

        # The HTTP API reads through the same repository.
        await repo.ping()
        assert await repo.decision_for_case(CASE_ID) == DECISION_ID
        assert await repo.decision_for_case(str(uuid.uuid4())) is None
        served = await repo.get_summary(CASE_ID)
        assert served is not None and served["status"] == "ok" and "facts" not in served["summary"]
        assert await repo.get_summary(str(uuid.uuid4())) is None
        regen = repo.record_regeneration
        assert await regen("analyst-1", CASE_ID, "it-key-0001", limit=2, window_s=3600) == "accepted"
        assert await regen("risk-1", CASE_ID, "it-key-0001", limit=2, window_s=3600) == "duplicate"
        assert await regen("analyst-1", CASE_ID, "it-key-0002", limit=2, window_s=3600) == "accepted"
        assert await regen("analyst-1", CASE_ID, "it-key-0003", limit=2, window_s=3600) == "limited"
        assert await regen("risk-1", CASE_ID, "it-key-0003", limit=2, window_s=3600) == "accepted"
        assert await regen("analyst-1", CASE_ID, "it-key-0004", limit=2, window_s=0.000001) == "accepted"
        # One paid attempt recorded for the summarized decision; the counter climbs per attempt (D-048).
        assert await repo.begin_attempt(CONSUMER_NAME, DECISION_ID) == 2
        assert await repo.begin_attempt("it", "fresh") == 1
        async with pool.connection() as conn:
            cur = await conn.execute(
                "SELECT count(*) FROM processed_messages WHERE consumer = %s AND message_key = %s",
                (CONSUMER_NAME, DECISION_ID),
            )
            assert await cur.fetchone() == (1,)
    finally:
        stop.set()
        await asyncio.wait_for(task, timeout=15)
        await pool.close()


async def _count(brokers: str, topic: str) -> int:
    from aiokafka import AIOKafkaConsumer

    c = AIOKafkaConsumer(
        topic, bootstrap_servers=brokers, auto_offset_reset="earliest", enable_auto_commit=False
    )
    await c.start()
    try:
        batch = await c.getmany(timeout_ms=1000)
        return sum(len(v) for v in batch.values())
    finally:
        await c.stop()
