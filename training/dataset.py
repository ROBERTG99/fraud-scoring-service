"""Load the synthetic dataset written by cmd/gen into columnar numpy arrays."""

from __future__ import annotations

import csv
from dataclasses import dataclass
from pathlib import Path

import numpy as np
import numpy.typing as npt
import orjson

KIND_LOGIN, KIND_DEVICE, KIND_TX = 0, 1, 2
CHANGE_CODES = {"device_bound": 1, "password_changed": 2, "email_changed": 3, "phone_changed": 4}
CHANNEL_CODES = {"CARD_PRESENT": 1, "ECOMMERCE": 2, "P2P": 3}
SPLITS = ("warmup", "train", "valid", "test")

I64 = npt.NDArray[np.int64]
I32 = npt.NDArray[np.int32]
I8 = npt.NDArray[np.int8]
B = npt.NDArray[np.bool_]


@dataclass
class Events:
    """Columnar events, sorted by (account, ts). String columns are dictionary-encoded."""

    account: I32
    ts: I64
    kind: I8
    device: I32
    success: B
    change: I8
    amount: I64
    mcc: I32
    channel: I8
    event_id: list[str]
    accounts: list[str]
    devices: list[str]
    mccs: list[str]

    def __len__(self) -> int:
        return len(self.ts)


@dataclass
class Labels:
    transaction_id: list[str]
    is_fraud: npt.NDArray[np.int8]
    pattern: list[str]
    split: list[str]


class _Codes:
    def __init__(self) -> None:
        self.index: dict[str, int] = {}
        self.values: list[str] = []

    def code(self, value: str) -> int:
        idx = self.index.get(value)
        if idx is None:
            idx = len(self.values)
            self.index[value] = idx
            self.values.append(value)
        return idx


def events_from_dicts(rows: list[dict[str, object]]) -> Events:
    """Build columnar events from parsed event JSON objects (any order)."""
    acc, dev, mcc = _Codes(), _Codes(), _Codes()
    n = len(rows)
    account = np.empty(n, dtype=np.int32)
    ts = np.empty(n, dtype=np.int64)
    kind = np.empty(n, dtype=np.int8)
    device = np.empty(n, dtype=np.int32)
    success = np.zeros(n, dtype=np.bool_)
    change = np.zeros(n, dtype=np.int8)
    amount = np.zeros(n, dtype=np.int64)
    mccs = np.full(n, -1, dtype=np.int32)
    channel = np.zeros(n, dtype=np.int8)
    ids: list[str] = []
    for i, row in enumerate(rows):
        schema = str(row["schema"])
        account[i] = acc.code(str(row["account_id"]))
        ts[i] = parse_ts_ms(str(row["event_ts"]))
        device[i] = dev.code(str(row["device_id"]))
        ids.append(str(row["event_id"]))
        if schema.endswith(".login.v1"):
            kind[i] = KIND_LOGIN
            success[i] = bool(row["success"])
        elif schema.endswith(".device.v1"):
            kind[i] = KIND_DEVICE
            change[i] = CHANGE_CODES[str(row["change"])]
        else:
            kind[i] = KIND_TX
            amount[i] = int(row["amount_minor"])  # type: ignore[call-overload]
            mccs[i] = mcc.code(str(row["mcc"]))
            channel[i] = CHANNEL_CODES[str(row["channel"])]
    order = np.lexsort((ts, account))
    return Events(
        account=account[order],
        ts=ts[order],
        kind=kind[order],
        device=device[order],
        success=success[order],
        change=change[order],
        amount=amount[order],
        mcc=mccs[order],
        channel=channel[order],
        event_id=[ids[i] for i in order],
        accounts=acc.values,
        devices=dev.values,
        mccs=mcc.values,
    )


_EPOCH_CACHE: dict[str, int] = {}


def parse_ts_ms(value: str) -> int:
    """Parse the generator's RFC 3339 UTC timestamps ('2026-09-01T10:00:00.123Z') to Unix ms."""
    from datetime import datetime

    day_key = value[:13]
    base = _EPOCH_CACHE.get(day_key)
    if base is None:
        base = int(datetime.fromisoformat(day_key + ":00:00+00:00").timestamp() * 1000)
        _EPOCH_CACHE[day_key] = base
    if value.endswith("Z") and len(value) == 24 and value[19] == ".":
        return base + int(value[14:16]) * 60_000 + int(value[17:19]) * 1000 + int(value[20:23])
    return int(datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp() * 1000)


def load_events(path: Path) -> Events:
    rows: list[dict[str, object]] = []
    with path.open("rb") as fh:
        rows.extend(orjson.loads(line) for line in fh)
    return events_from_dicts(rows)


def load_labels(path: Path) -> Labels:
    ids: list[str] = []
    fraud: list[int] = []
    pattern: list[str] = []
    split: list[str] = []
    with path.open(encoding="utf-8", newline="") as fh:
        for row in csv.DictReader(fh):
            ids.append(row["transaction_id"])
            fraud.append(int(row["is_fraud"]))
            pattern.append(row["pattern"])
            split.append(row["split"])
    return Labels(ids, np.asarray(fraud, dtype=np.int8), pattern, split)
