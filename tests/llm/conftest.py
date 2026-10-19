"""LLM tests run with zero network: TCP/UDP sockets are disabled (no loopback); AF_UNIX stays for asyncio."""

from __future__ import annotations

from collections.abc import Iterator

import pytest
from pytest_socket import disable_socket, enable_socket


@pytest.fixture(autouse=True)
def _no_network() -> Iterator[None]:
    disable_socket(allow_unix_socket=True)
    yield
    enable_socket()
