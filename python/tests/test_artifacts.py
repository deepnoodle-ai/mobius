"""Route-level tests for project-authorized artifact upload."""

from __future__ import annotations

import io
import json

import httpx
import pytest

from deepnoodle.mobius import Artifact, Client, ClientOptions


def _client_with(handler) -> Client:
    return Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="https://api.example.invalid",
            project="test-project",
        ),
        transport=httpx.MockTransport(handler),
    )


def _artifact_body(**overrides) -> dict:
    body = {
        "id": "art_1",
        "name": "renders/report.html",
        "mime_type": "text/html",
        "size_bytes": 15,
        "visibility": "private",
        "created_at": "2026-07-17T00:00:00Z",
    }
    body.update(overrides)
    return body


def _parse_multipart(request: httpx.Request) -> dict[str, bytes]:
    content_type = request.headers["Content-Type"]
    assert content_type.startswith("multipart/form-data; boundary=")
    boundary = content_type.split("boundary=", 1)[1].encode()
    parts: dict[str, bytes] = {}
    for chunk in request.read().split(b"--" + boundary):
        chunk = chunk.strip()
        if not chunk or chunk == b"--":
            continue
        headers, _, value = chunk.partition(b"\r\n\r\n")
        for header_line in headers.split(b"\r\n"):
            if header_line.lower().startswith(b"content-disposition"):
                name = header_line.split(b'name="', 1)[1].split(b'"', 1)[0]
                parts[name.decode()] = value
    return parts


def test_create_artifact_sends_contract_multipart_fields() -> None:
    captured: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["method"] = request.method
        captured["path"] = request.url.path
        captured["idempotency"] = request.headers.get("Idempotency-Key")
        captured["parts"] = _parse_multipart(request)
        return httpx.Response(201, json=_artifact_body(metadata={"renderer": "omni"}))

    client = _client_with(handler)
    artifact = client.create_artifact(
        b"<h1>report</h1>",
        name="renders/report.html",
        mime="text/html",
        metadata={"renderer": "omni"},
        idempotency_key="delivery-1:report",
    )
    client.close()

    assert isinstance(artifact, Artifact)
    assert artifact.id == "art_1"
    assert artifact.metadata == {"renderer": "omni"}
    assert captured["method"] == "POST"
    assert captured["path"] == "/v1/projects/test-project/artifacts"
    assert captured["idempotency"] == "delivery-1:report"
    parts = captured["parts"]
    assert parts["name"] == b"renders/report.html"
    assert parts["mime"] == b"text/html"
    assert parts["size_bytes"] == b"15"
    assert json.loads(parts["metadata"]) == {"renderer": "omni"}
    assert parts["file"] == b"<h1>report</h1>"
    for banned in ("run_id", "step_id", "visibility", "tags"):
        assert banned not in parts


def test_create_artifact_streams_path_source_and_defaults_name(tmp_path) -> None:
    source = tmp_path / "report.txt"
    source.write_bytes(b"report bytes")
    captured: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["parts"] = _parse_multipart(request)
        return httpx.Response(
            201, json=_artifact_body(name="report.txt", mime_type="text/plain", size_bytes=12)
        )

    client = _client_with(handler)
    artifact = client.create_artifact(source)
    client.close()

    assert artifact.name == "report.txt"
    parts = captured["parts"]
    assert parts["name"] == b"report.txt"
    assert parts["size_bytes"] == b"12"
    assert parts["file"] == b"report bytes"


def test_create_artifact_accepts_binary_file_object() -> None:
    captured: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["parts"] = _parse_multipart(request)
        captured["idempotency"] = request.headers.get("Idempotency-Key")
        return httpx.Response(200, json=_artifact_body())

    client = _client_with(handler)
    client.create_artifact(io.BytesIO(b"stream bytes"), name="stream.bin")
    client.close()

    parts = captured["parts"]
    assert parts["file"] == b"stream bytes"
    assert parts["name"] == b"stream.bin"
    # Unsized streams omit the declared length instead of guessing it.
    assert "size_bytes" not in parts
    assert captured["idempotency"] is None


def test_create_artifact_validates_inputs() -> None:
    client = _client_with(lambda request: httpx.Response(500))

    with pytest.raises(ValueError, match="name is required"):
        client.create_artifact(b"bytes")
    with pytest.raises(ValueError, match="idempotency_key"):
        client.create_artifact(b"bytes", name="x", idempotency_key="k" * 256)
    client.close()
