"""Route-level tests for the OAuth return-origin allowlist wrappers."""

from __future__ import annotations

import json

import httpx

from deepnoodle.mobius import Client, ClientOptions


def _client_with(handler) -> Client:
    return Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="https://api.example.invalid",
            project="test-project",
            retry=0,
        ),
        transport=httpx.MockTransport(handler),
    )


def test_oauth_return_origin_routes() -> None:
    seen: list[tuple[str, str]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append((request.method, request.url.path))
        if request.method == "PUT":
            assert json.loads(request.content) == {
                "origins": ["https://app.partner.example"]
            }
        return httpx.Response(200, json={"origins": ["https://app.partner.example"]})

    client = _client_with(handler)
    current = client.get_oauth_return_origins()
    assert current.origins == ["https://app.partner.example"]
    replaced = client.replace_oauth_return_origins(["https://app.partner.example"])
    assert replaced.origins == ["https://app.partner.example"]
    client.close()

    assert seen == [
        ("GET", "/v1/organization/oauth-return-origins"),
        ("PUT", "/v1/organization/oauth-return-origins"),
    ]


def test_replace_oauth_return_origins_empty_list_disables_embedded_return() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.method == "PUT"
        # An empty list is the documented way to disable embedded return;
        # the wrapper must pass it through without client-side validation.
        assert json.loads(request.content) == {"origins": []}
        return httpx.Response(200, json={"origins": []})

    client = _client_with(handler)
    replaced = client.replace_oauth_return_origins([])
    assert replaced.origins == []
    client.close()
