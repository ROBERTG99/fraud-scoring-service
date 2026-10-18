"""Summarizer consumer (spec FR-11, T-29): dedupe, fallback, skip and DLQ handling without Kafka."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any

import pytest
from aiokafka import ConsumerRecord, TopicPartition

from fraud_summarizer import events
from fraud_summarizer.consumer import (
    CONSUMER_NAME,
    MAX_PAID_ATTEMPTS,
    MAX_RETRIES,
    AttemptsExhaustedError,
    DecisionHandler,
    KafkaDecisionConsumer,
    dlq_record,
)
from fraud_summarizer.llm import LLMGateway, ProviderError, ProviderRequest, ProviderResponse
from fraud_summarizer.llm.fake import FakeLLM
from fraud_summarizer.llm.routes import load
from fraud_summarizer.pipeline import SummaryPipeline
from tests.unit.test_pipeline import MemoryRepo, journey_case

DECISION_ID = "0192f000-0000-7000-8000-0000000000d1"


def decision_msg(action: str = "REVIEW", decision_id: str = DECISION_ID) -> bytes:
    return json.dumps(
        {
            "schema": "fraud.decision.v1",
            "decision_id": decision_id,
            "transaction_id": "0192f000-0000-7000-8000-0000000000e1",
            "account_id": "acc_00000042",
            "action": action,
            "score": 0.62,
            "reason_codes": ["NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", "MODEL_SCORE_ELEVATED"],
            "model_version": "fraud-gbdt/1",
            "rules_version": "2026.10.1",
            "degraded": False,
            "decided_at": "2026-09-20T10:03:00.000Z",
        }
    ).encode()


@dataclass
class CountingProvider:
    inner: Any
    name: str = "fake"
    calls: int = 0

    async def complete(self, req: ProviderRequest) -> ProviderResponse:
        self.calls += 1
        result: ProviderResponse = await self.inner.complete(req)
        return result


@dataclass
class DownProvider:
    name: str = "fake"
    calls: int = 0

    async def complete(self, req: ProviderRequest) -> ProviderResponse:
        self.calls += 1
        raise ProviderError("503 overloaded", status=503, retryable=True, kind="http")


@dataclass
class Processed:
    keys: set[tuple[str, str]] = field(default_factory=set)
    attempts: dict[tuple[str, str], int] = field(default_factory=dict)

    async def begin_attempt(self, consumer: str, key: str) -> int:
        self.attempts[(consumer, key)] = self.attempts.get((consumer, key), 0) + 1
        return self.attempts[(consumer, key)]

    async def is_processed(self, consumer: str, key: str) -> bool:
        return (consumer, key) in self.keys

    async def mark_processed(self, consumer: str, key: str) -> bool:
        if (consumer, key) in self.keys:
            return False
        self.keys.add((consumer, key))
        return True


def handler(provider: Any, repo: MemoryRepo) -> tuple[DecisionHandler, Processed]:
    async def no_sleep(_: float) -> None:
        return None

    gw = LLMGateway(load(env={"FRAUD_SUMMARY_PROVIDER": "fake"}), {"fake": provider}, sleep=no_sleep)
    processed = Processed()
    return DecisionHandler(SummaryPipeline(gw, repo), processed), processed


async def test_duplicate_decision_processed_once() -> None:
    repo = MemoryRepo({DECISION_ID: journey_case()})
    provider = CountingProvider(FakeLLM())
    h, processed = handler(provider, repo)
    first = await h.handle(decision_msg())
    second = await h.handle(decision_msg())
    assert (first.outcome, first.status) == ("summarized", "ok")
    assert second.outcome == "duplicate"
    assert provider.calls == 1
    assert len(repo.saved) == 1
    assert len({s.run_id for s in repo.steps}) == 1
    assert processed.keys == {(CONSUMER_NAME, DECISION_ID)}


async def test_provider_failure_writes_fallback() -> None:
    repo = MemoryRepo({DECISION_ID: journey_case()})
    provider = DownProvider()
    h, processed = handler(provider, repo)
    res = await h.handle(decision_msg())
    assert (res.outcome, res.status) == ("summarized", "fallback")
    assert provider.calls == 3  # route attempts, then no fallback provider configured
    saved = repo.saved[0]
    assert saved.summary.status == "fallback" and saved.summary.model == "none"
    assert saved.summary.recommended_checks[0].startswith("AI summary unavailable")
    assert (CONSUMER_NAME, DECISION_ID) in processed.keys


async def test_non_review_decisions_are_skipped() -> None:
    repo = MemoryRepo({DECISION_ID: journey_case()})
    provider = CountingProvider(FakeLLM())
    h, processed = handler(provider, repo)
    for action in ("APPROVE", "DECLINE"):
        assert (await h.handle(decision_msg(action))).outcome == "skipped"
    assert provider.calls == 0 and not repo.saved and not processed.keys


async def test_missing_case_is_recorded_and_not_retried() -> None:
    h, _ = handler(CountingProvider(FakeLLM()), MemoryRepo({}))
    assert (await h.handle(decision_msg())).outcome == "missing_case"
    assert (await h.handle(decision_msg())).outcome == "duplicate"


@pytest.mark.parametrize(
    "raw",
    [b"not json", b"[]", json.dumps({"schema": "fraud.event.login.v1"}).encode(), decision_msg("BLOCK")],
)
async def test_invalid_messages_raise(raw: bytes) -> None:
    h, _ = handler(CountingProvider(FakeLLM()), MemoryRepo({}))
    with pytest.raises(events.InvalidEventError):
        await h.handle(raw)


def record(offset: int, value: bytes) -> ConsumerRecord[Any, Any]:
    return ConsumerRecord(
        topic="decisions",
        partition=3,
        offset=offset,
        timestamp=0,
        timestamp_type=0,
        key=b"acc_00000042",
        value=value,
        checksum=None,
        serialized_key_size=12,
        serialized_value_size=len(value),
        headers=(),
    )


@dataclass
class FakeConsumer:
    commits: list[dict[TopicPartition, int]] = field(default_factory=list)
    seeks: list[tuple[TopicPartition, int]] = field(default_factory=list)

    async def commit(self, offsets: dict[TopicPartition, int]) -> None:
        self.commits.append(dict(offsets))

    def seek(self, partition: TopicPartition, offset: int) -> None:
        self.seeks.append((partition, offset))


@dataclass
class FakeProducer:
    sent: list[tuple[str, bytes]] = field(default_factory=list)

    async def send_and_wait(self, topic: str, value: bytes, key: bytes | None = None) -> None:
        self.sent.append((topic, value))


async def test_invalid_message_goes_to_dlq_and_offset_advances() -> None:
    h, _ = handler(CountingProvider(FakeLLM()), MemoryRepo({DECISION_ID: journey_case()}))
    kc = KafkaDecisionConsumer(h, "unused:9092", retry_backoff_s=0)
    tp = TopicPartition("decisions", 3)
    consumer, producer = FakeConsumer(), FakeProducer()
    await kc.process_partition(consumer, producer, tp, [record(10, b"{bad"), record(11, decision_msg())])
    assert consumer.commits == [{tp: 12}] and not consumer.seeks
    topic, body = producer.sent[0]
    dlq = json.loads(body)
    assert topic == "events.dlq" and dlq["offset"] == 10 and dlq["source_topic"] == "decisions"
    events.validate(body)  # the DLQ record itself matches fraud.dlq.v1


async def test_transient_failure_commits_prefix_and_seeks_back() -> None:
    class FlakyRepo(MemoryRepo):
        async def load_case(self, decision_id: str) -> Any:
            if decision_id.endswith("d2"):
                raise ConnectionError("postgres down")
            return await super().load_case(decision_id)

    second = "0192f000-0000-7000-8000-0000000000d2"
    h, processed = handler(CountingProvider(FakeLLM()), FlakyRepo({DECISION_ID: journey_case()}))
    kc = KafkaDecisionConsumer(h, "unused:9092", retry_backoff_s=0)
    tp = TopicPartition("decisions", 3)
    consumer = FakeConsumer()
    msgs = [
        record(20, decision_msg()),
        record(21, decision_msg(decision_id=second)),
        record(22, decision_msg()),
    ]
    await kc.process_partition(consumer, FakeProducer(), tp, msgs)
    assert consumer.commits == [{tp: 21}]
    assert consumer.seeks == [(tp, 21)]
    assert (CONSUMER_NAME, second) not in processed.keys


def test_dlq_record_matches_schema() -> None:
    body = dlq_record(record(5, b"\x00\x01"), "x" * 5000)
    name, doc = events.validate(body)
    assert name == "fraud.dlq.v1" and len(doc["error"]) == 2000


class PersistFails(MemoryRepo):
    async def save_summary(self, result: Any, *, replace: bool = False) -> None:
        raise RuntimeError("persist failed")


async def test_paid_attempts_per_decision_are_capped() -> None:
    """Audit M6 M7: a failure after the paid call must not buy another summary forever. The attempt is
    recorded before the call, so the cap also holds across restarts (the store is Postgres)."""
    provider = CountingProvider(FakeLLM())
    h, processed = handler(provider, PersistFails({DECISION_ID: journey_case()}))
    for _ in range(MAX_PAID_ATTEMPTS):
        with pytest.raises(RuntimeError, match="persist failed"):
            await h.handle(decision_msg())
    with pytest.raises(AttemptsExhaustedError):
        await h.handle(decision_msg())
    assert provider.calls == MAX_PAID_ATTEMPTS
    assert (CONSUMER_NAME, DECISION_ID) not in processed.keys


async def test_an_exhausted_decision_goes_to_the_dlq() -> None:
    h, processed = handler(CountingProvider(FakeLLM()), MemoryRepo({DECISION_ID: journey_case()}))
    processed.attempts[(CONSUMER_NAME, DECISION_ID)] = MAX_PAID_ATTEMPTS
    kc = KafkaDecisionConsumer(h, "unused:9092", retry_backoff_s=0)
    tp = TopicPartition("decisions", 3)
    consumer, producer = FakeConsumer(), FakeProducer()
    await kc.process_partition(consumer, producer, tp, [record(40, decision_msg())])
    assert consumer.commits == [{tp: 41}] and not consumer.seeks
    assert "paid attempts" in json.loads(producer.sent[0][1])["error"]


async def test_a_message_that_keeps_failing_goes_to_the_dlq() -> None:
    """A bug in our own code must not stall the partition: after MAX_RETRIES the message is parked."""

    class Buggy(MemoryRepo):
        async def load_case(self, decision_id: str) -> Any:
            raise KeyError("facts")

    h, _ = handler(CountingProvider(FakeLLM()), Buggy({}))
    kc = KafkaDecisionConsumer(h, "unused:9092", retry_backoff_s=0)
    tp = TopicPartition("decisions", 3)
    consumer, producer = FakeConsumer(), FakeProducer()
    for _ in range(MAX_RETRIES - 1):
        await kc.process_partition(consumer, producer, tp, [record(50, decision_msg())])
    assert consumer.seeks == [(tp, 50)] * (MAX_RETRIES - 1) and not producer.sent
    await kc.process_partition(
        consumer, producer, tp, [record(50, decision_msg()), record(51, decision_msg("APPROVE"))]
    )
    assert consumer.commits == [{tp: 52}]
    topic, body = producer.sent[0]
    assert topic == "events.dlq" and json.loads(body)["offset"] == 50
    assert "KeyError" in json.loads(body)["error"]


async def test_transient_failures_retry_without_a_cap() -> None:
    """Postgres being down costs nothing (no attempt is recorded, no call is made) and is not poison."""

    class Down(MemoryRepo):
        async def load_case(self, decision_id: str) -> Any:
            raise ConnectionError("postgres down")

    h, _ = handler(CountingProvider(FakeLLM()), Down({}))
    kc = KafkaDecisionConsumer(h, "unused:9092", retry_backoff_s=0)
    tp = TopicPartition("decisions", 3)
    consumer, producer = FakeConsumer(), FakeProducer()
    for _ in range(MAX_RETRIES * 2):
        await kc.process_partition(consumer, producer, tp, [record(60, decision_msg())])
    assert not producer.sent and not consumer.commits


async def test_transient_retries_back_off_exponentially() -> None:
    """Round 2 (N11): the delay doubles per retry of the same message, transient or not, up to 30 s."""

    class Down(MemoryRepo):
        async def load_case(self, decision_id: str) -> Any:
            raise ConnectionError("postgres down")

    slept: list[float] = []

    async def sleep(s: float) -> None:
        slept.append(s)

    h, _ = handler(CountingProvider(FakeLLM()), Down({}))
    kc = KafkaDecisionConsumer(h, "unused:9092", retry_backoff_s=1.0, sleep=sleep)
    tp = TopicPartition("decisions", 3)
    for _ in range(7):
        await kc.process_partition(FakeConsumer(), FakeProducer(), tp, [record(70, decision_msg())])
    assert slept == [1.0, 2.0, 4.0, 8.0, 16.0, 30.0, 30.0]


async def test_a_statement_timeout_is_not_transient() -> None:
    """A query that always times out would otherwise stall its partition forever."""
    import psycopg

    class Slow(MemoryRepo):
        async def load_case(self, decision_id: str) -> Any:
            raise psycopg.errors.QueryCanceled("canceling statement due to statement timeout")

    h, _ = handler(CountingProvider(FakeLLM()), Slow({}))
    kc = KafkaDecisionConsumer(h, "unused:9092", retry_backoff_s=0)
    tp = TopicPartition("decisions", 3)
    producer = FakeProducer()
    for _ in range(MAX_RETRIES):
        await kc.process_partition(FakeConsumer(), producer, tp, [record(80, decision_msg())])
    assert producer.sent and json.loads(producer.sent[0][1])["offset"] == 80
