"""Route-level tests for the action invocation audit surface."""

from __future__ import annotations

import httpx

from deepnoodle.mobius import (
    Client,
    ClientOptions,
    ListActionInvocationsOptions,
)


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


def test_list_encodes_every_filter() -> None:
    seen: list[dict[str, str]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/v1/projects/test-project/action-invocations"
        seen.append(dict(request.url.params))
        return httpx.Response(200, json={"items": [], "has_more": False})

    client = _client_with(handler)
    client.list_action_invocations(
        ListActionInvocationsOptions(
            run_id="run_1",
            job_id="job_1",
            environment_id="env_1",
            action_name="crm.sync",
            action_id="act_1",
            definition_scope="organization",
            secret_version=2,
            delivery_id="dlv_1",
            correlation_id="corr_1",
            status="failed",
            cursor="cur_1",
            limit=25,
        )
    )
    client.close()

    assert seen == [
        {
            "run_id": "run_1",
            "job_id": "job_1",
            "environment_id": "env_1",
            "action_name": "crm.sync",
            "action_id": "act_1",
            "definition_scope": "organization",
            "secret_version": "2",
            "delivery_id": "dlv_1",
            "correlation_id": "corr_1",
            "status": "failed",
            "cursor": "cur_1",
            "limit": "25",
        }
    ]


def test_list_preserves_provenance_fields() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert not dict(request.url.params), "no options must mean no query params"
        return httpx.Response(
            200,
            json={
                "items": [
                    {
                        "id": "inv_1",
                        "action_name": "crm.sync",
                        "action_id": "act_1",
                        "definition_scope": "organization",
                        "secret_version": 2,
                        "delivery_id": "dlv_1",
                        "correlation_id": "corr_1",
                        "status": "success",
                        "source": "loop",
                        "retry_count": 0,
                        "started_at": "2026-07-17T00:00:00Z",
                        "finished_at": "2026-07-17T00:00:01Z",
                    }
                ],
                "next_cursor": "cur_2",
                "has_more": True,
            },
        )

    client = _client_with(handler)
    page = client.list_action_invocations()
    client.close()

    assert page.has_more
    entry = page.items[0]
    assert entry.action_id == "act_1"
    assert entry.definition_scope == "organization"
    assert entry.secret_version == 2
    assert entry.delivery_id == "dlv_1"
    assert entry.correlation_id == "corr_1"
