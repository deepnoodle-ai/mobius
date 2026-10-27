"""Every request names the SDK in X-Mobius-Client."""

from __future__ import annotations

import contextlib
import re

import httpx
import pytest
import websockets

from deepnoodle.mobius import Client
from deepnoodle.mobius.client import MOBIUS_CLIENT, MOBIUS_CLIENT_HEADER, sdk_version
from deepnoodle.mobius.worker import Worker, WorkerConfig

# The server's rules: a name of up to 32 [a-z0-9._-], then an optional version
# of up to 64 [A-Za-z0-9._+-].
SERVER_CLIENT_VALUE = re.compile(r"^[a-z0-9._-]{1,32}(/[A-Za-z0-9._+-]{1,64})?$")


def test_requests_send_client_header() -> None:
    seen: list[str | None] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request.headers.get(MOBIUS_CLIENT_HEADER))
        return httpx.Response(200, json={})

    client = Client("key", base_url="http://test", transport=httpx.MockTransport(handler))
    client._client.get("/v1/anything")

    assert MOBIUS_CLIENT_HEADER == "X-Mobius-Client"
    assert MOBIUS_CLIENT.startswith("sdk-python/")
    assert SERVER_CLIENT_VALUE.match(MOBIUS_CLIENT)
    assert seen == [MOBIUS_CLIENT]


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("version", "want"),
    [("v1.2.3", "worker/v1.2.3"), (None, f"worker/{sdk_version()}")],
)
async def test_worker_socket_sends_worker_client_header(
    monkeypatch: pytest.MonkeyPatch, version: str | None, want: str
) -> None:
    seen: dict[str, str] = {}

    @contextlib.asynccontextmanager
    async def fake_connect(url: str, *, additional_headers: dict[str, str]):
        seen.update(additional_headers)
        yield object()

    async def serve(_ws: object) -> None:
        return None

    monkeypatch.setattr(websockets, "connect", fake_connect)
    worker = Worker(Client("key", base_url="http://test"), WorkerConfig(version=version))
    worker._serve_socket = serve  # type: ignore[method-assign]

    await worker._run_socket()

    assert seen[MOBIUS_CLIENT_HEADER] == want
    assert SERVER_CLIENT_VALUE.match(want)
    assert seen["Authorization"] == "Bearer key"
