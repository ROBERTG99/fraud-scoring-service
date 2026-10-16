"""Kafka consumer: REVIEW decisions → case summaries (spec FR-11, §4.3).

Delivery is at least once. A decision is skipped when `processed_messages` already holds its
`decision_id`; it is recorded only after the summary is persisted, so a crash mid-call re-summarizes
instead of losing the case (docs/DECISIONS.md D-022). The summary insert is `ON CONFLICT DO NOTHING`, so a
rare duplicate after a rebalance cannot overwrite the first summary.
"""

from __future__ import annotations

import asyncio
import base64
import json
import logging
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any, Literal, Protocol

from aiokafka import AIOKafkaConsumer, AIOKafkaProducer, ConsumerRecord, TopicPartition
from aiokafka.errors import CommitFailedError

from fraud_summarizer import events
from fraud_summarizer.metrics import MESSAGES, SUMMARY_STATUS
from fraud_summarizer.pipeline import SummaryPipeline

log = logging.getLogger(__name__)

CONSUMER_NAME = "case-summarizer"
DECISIONS_TOPIC = "decisions"
DLQ_TOPIC = "events.dlq"

Outcome = Literal["skipped", "duplicate", "summarized", "missing_case"]


class ProcessedStore(Protocol):
    async def is_processed(self, consumer: str, key: str) -> bool: ...

    async def mark_processed(self, consumer: str, key: str) -> bool: ...


@dataclass(frozen=True)
class HandleResult:
    outcome: Outcome
    decision_id: str | None = None
    status: str | None = None


class DecisionHandler:
    """Handles one `decisions` message. Raises events.InvalidEventError for messages that never parse."""

    def __init__(self, pipeline: SummaryPipeline, processed: ProcessedStore) -> None:
        self.pipeline = pipeline
        self.processed = processed

    async def handle(self, raw: bytes) -> HandleResult:
        name, doc = events.validate(raw)
        if name != "fraud.decision.v1":
            raise events.InvalidEventError(f"unexpected schema {name} on {DECISIONS_TOPIC}")
        decision_id = str(doc["decision_id"])
        if doc["action"] != "REVIEW":
            return self._done(HandleResult("skipped", decision_id))
        if await self.processed.is_processed(CONSUMER_NAME, decision_id):
            return self._done(HandleResult("duplicate", decision_id))
        result = await self.pipeline.run(decision_id)
        await self.processed.mark_processed(CONSUMER_NAME, decision_id)
        if result is None:
            # The case row is written with the decision, so this means it was deleted (retention).
            log.warning("no review case for decision", extra={"decision_id": decision_id})
            return self._done(HandleResult("missing_case", decision_id))
        SUMMARY_STATUS.labels(status=result.status).inc()
        return self._done(HandleResult("summarized", decision_id, result.status))

    @staticmethod
    def _done(r: HandleResult) -> HandleResult:
        MESSAGES.labels(outcome=r.outcome).inc()
        return r


def dlq_record(msg: ConsumerRecord[Any, Any], error: str) -> bytes:
    doc = {
        "schema": "fraud.dlq.v1",
        "source_topic": msg.topic,
        "partition": msg.partition,
        "offset": msg.offset,
        "error": error[:2000],
        "raw_b64": base64.b64encode(msg.value or b"").decode("ascii"),
        "failed_at": datetime.now(UTC).isoformat(timespec="milliseconds").replace("+00:00", "Z"),
    }
    return json.dumps(doc).encode()


class _Consumer(Protocol):
    async def commit(self, offsets: dict[TopicPartition, int]) -> None: ...

    def seek(self, partition: TopicPartition, offset: int) -> None: ...


class _Producer(Protocol):
    async def send_and_wait(self, topic: str, value: bytes, key: bytes | None = None) -> Any: ...


class KafkaDecisionConsumer:
    """Consumes `decisions`; partitions run concurrently, messages within a partition in order."""

    def __init__(
        self,
        handler: DecisionHandler,
        brokers: str,
        *,
        topic: str = DECISIONS_TOPIC,
        group: str = CONSUMER_NAME,
        dlq_topic: str = DLQ_TOPIC,
        retry_backoff_s: float = 1.0,
    ) -> None:
        self.handler = handler
        self.brokers = brokers
        self.topic = topic
        self.group = group
        self.dlq_topic = dlq_topic
        self.retry_backoff_s = retry_backoff_s
        self.ready = asyncio.Event()

    async def run(self, stop: asyncio.Event) -> None:
        consumer = AIOKafkaConsumer(
            self.topic,
            bootstrap_servers=self.brokers,
            group_id=self.group,
            enable_auto_commit=False,
            auto_offset_reset="earliest",
            max_poll_interval_ms=300_000,
        )
        producer = AIOKafkaProducer(bootstrap_servers=self.brokers, request_timeout_ms=10_000)
        await consumer.start()
        await producer.start()
        self.ready.set()
        try:
            while not stop.is_set():
                batch = await consumer.getmany(timeout_ms=500, max_records=64)
                if batch:
                    await asyncio.gather(
                        *(self.process_partition(consumer, producer, tp, msgs) for tp, msgs in batch.items())
                    )
        finally:
            self.ready.clear()
            await producer.stop()
            await consumer.stop()

    async def process_partition(
        self,
        consumer: _Consumer,
        producer: _Producer,
        tp: TopicPartition,
        msgs: list[ConsumerRecord[Any, Any]],
    ) -> None:
        """Handle msgs in order and commit what succeeded. On a transient failure (Postgres down), commit
        the prefix, seek back to the failed message and back off so it is retried."""
        next_offset: int | None = None
        for msg in msgs:
            try:
                await self.handler.handle(msg.value or b"")
            except events.InvalidEventError as exc:
                MESSAGES.labels(outcome="invalid").inc()
                await producer.send_and_wait(self.dlq_topic, dlq_record(msg, str(exc)), key=msg.key)
            except Exception:
                MESSAGES.labels(outcome="retry").inc()
                log.exception(
                    "summarizer failed; will retry", extra={"partition": tp.partition, "offset": msg.offset}
                )
                await self._commit(consumer, tp, next_offset)
                consumer.seek(tp, msg.offset)
                await asyncio.sleep(self.retry_backoff_s)
                return
            next_offset = msg.offset + 1
        await self._commit(consumer, tp, next_offset)

    @staticmethod
    async def _commit(consumer: _Consumer, tp: TopicPartition, offset: int | None) -> None:
        if offset is None:
            return
        try:
            await consumer.commit({tp: offset})
        except CommitFailedError:
            # Rebalanced away: the new owner re-reads from the last commit; processed_messages dedupes.
            log.warning("offset commit failed after rebalance", extra={"partition": tp.partition})
