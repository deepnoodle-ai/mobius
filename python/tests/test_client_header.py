"""Every request names the SDK in X-Mobius-Client."""

from __future__ import annotations

import re

import httpx

from deepnoodle.mobius import Client
from deepnoodle.mobius.client import MOBIUS_CLIENT, MOBIUS_CLIENT_HEADER

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
