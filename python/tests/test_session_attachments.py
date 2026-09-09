"""Route-level tests for session attachment upload and deletion."""

from __future__ import annotations

import io

import httpx
import pytest

from deepnoodle.mobius import Client, ClientOptions, SessionAttachmentResponse

from .test_artifacts import _parse_multipart


def _client_with(handler) -> Client:
    return Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="https://api.example.invalid",
        ),
        transport=httpx.MockTransport(handler),
    )


def _attachment_body() -> dict:
    return {
        "artifact": {
            "id": "art_1",
            "name": "brief.md",
            "mime_type": "text/markdown",
            "size_bytes": 7,
            "owner": {"kind": "team"},
            "visibility": "private",
            "posture": "team",
            "created_at": "2026-07-17T00:00:00Z",
        },
        "content_block": {
            "type": "document",
            "source": {"type": "artifact", "artifact_id": "art_1"},
            "title": "brief.md",
            "media_type": "text/markdown",
        },
    }


def test_create_session_attachment_posts_to_session_scoped_path(tmp_path):
    path = tmp_path / "brief.md"
    path.write_bytes(b"# brief")
    seen: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["method"] = request.method
        seen["idempotency"] = request.headers.get("Idempotency-Key")
        seen["parts"] = _parse_multipart(request)
        return httpx.Response(201, json=_attachment_body())

    client = _client_with(handler)
    resp = client.create_session_attachment(
        "sess_1", path, mime="text/markdown", idempotency_key="turn-1:brief"
    )

    assert isinstance(resp, SessionAttachmentResponse)
    assert resp.artifact.id == "art_1"
    assert seen["method"] == "POST"
    assert seen["url"] == "https://api.example.invalid/v1/sessions/sess_1/attachments"
    assert seen["idempotency"] == "turn-1:brief"
    parts = seen["parts"]
    # name defaults to the file's base name for a path source
    assert parts["name"] == b"brief.md"
    assert parts["mime"] == b"text/markdown"
    assert parts["size_bytes"] == b"7"
    assert parts["file"] == b"# brief"
    # Attachments carry no caller metadata; the artifact endpoint does.
    assert "metadata" not in parts


def test_create_session_attachment_from_bytes_requires_a_name():
    def handler(request: httpx.Request) -> httpx.Response:  # pragma: no cover
        raise AssertionError("no request expected")

    client = _client_with(handler)
    with pytest.raises(ValueError, match="attachment name is required"):
        client.create_session_attachment("sess_1", b"# brief")


def test_create_session_attachment_rejects_an_overlong_retry_key():
    def handler(request: httpx.Request) -> httpx.Response:  # pragma: no cover
        raise AssertionError("no request expected")

    client = _client_with(handler)
    with pytest.raises(ValueError, match="at most 255 characters"):
        client.create_session_attachment(
            "sess_1", b"x", name="brief.md", idempotency_key="k" * 256
        )


def test_create_session_attachment_accepts_a_file_object():
    seen: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["parts"] = _parse_multipart(request)
        return httpx.Response(200, json=_attachment_body())

    client = _client_with(handler)
    client.create_session_attachment(
        "sess_1", io.BytesIO(b"# brief"), name="notes/brief.md"
    )

    parts = seen["parts"]
    assert parts["name"] == b"notes/brief.md"
    assert parts["file"] == b"# brief"


def test_delete_session_attachment_escapes_both_identifiers():
    seen: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["method"] = request.method
        return httpx.Response(204)

    client = _client_with(handler)
    assert client.delete_session_attachment("sess/1", "art 1") is None
    assert seen["method"] == "DELETE"
    assert (
        seen["url"]
        == "https://api.example.invalid/v1/sessions/sess%2F1/attachments/art%201"
    )
