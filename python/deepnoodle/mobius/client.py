from __future__ import annotations

import json
import re
import time
from dataclasses import dataclass
from collections.abc import Iterator
from typing import Any
from urllib.parse import quote, urlencode, urlparse, urlunparse

import httpx

from ._api.models import (
    AgentRef,
    CancelLoopRunRequest,
    ChannelContext,
    CreateLoopRequest,
    InlineAgentConfig,
    InvokeAgentRequest,
    InvokeInput,
    InvokeSessionSpec,
    Loop,
    LoopListResponse,
    LoopRun,
    LoopRunEvent,
    LoopRunListResponse,
    LoopRunSource,
    LoopRunStatus,
    LoopStatus,
    SessionTranscriptSnapshot,
    SignalLoopRunRequest,
    StartLoopRunRequest,
    TagMap,
    TurnAck,
    UpdateLoopRequest,
)
from .errors import AuthRevokedError, RateLimitError
from .retry import DEFAULT_MAX_RETRIES, RetryingTransport
from .transcript import (
    GetSessionTranscriptOptions,
    SessionTranscriptReducer,
    StreamSessionTranscriptOptions,
    TranscriptStreamEvent,
    WatchSessionTranscriptOptions,
    is_terminal_turn_status,
)

DEFAULT_BASE_URL = "https://api.mobiusops.ai"
_HANDLE_RE = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")


@dataclass
class ClientOptions:
    api_key: str
    base_url: str = DEFAULT_BASE_URL
    project: str = "default"
    namespace: str | None = None
    timeout: float = 60.0
    retry: int = DEFAULT_MAX_RETRIES

    def __post_init__(self) -> None:
        if self.namespace and self.project == "default":
            self.project = self.namespace
        self.namespace = self.project


@dataclass
class LoopOptions:
    name: str
    description: str | None = None
    agent_id: str | None = None
    default_config: dict[str, Any] | None = None
    settings: dict[str, Any] | None = None
    tags: TagMap | dict[str, str] | None = None
    # Authoring definition for the loop. Recognised keys are schema_version,
    # steps, event, config, triggers, defaults, limits, output, repositories,
    # cleanup, .... When it carries steps the loop is runnable immediately.
    # Keys are merged into the create request; explicit fields above take
    # precedence.
    spec: dict[str, Any] | None = None


@dataclass
class UpdateLoopOptions:
    name: str | None = None
    description: str | None = None
    agent_id: str | None = None
    default_config: dict[str, Any] | None = None
    settings: dict[str, Any] | None = None
    status: LoopStatus | None = None
    tags: TagMap | dict[str, str] | None = None
    # Replacement authoring definition. See LoopOptions.spec.
    spec: dict[str, Any] | None = None


@dataclass
class ListLoopsOptions:
    status: LoopStatus | None = None
    cursor: str | None = None
    limit: int | None = None


@dataclass
class StartRunOptions:
    # Exact event object that starts the run, reachable in templates at
    # ``event.*``. ``config`` holds optional static or caller-provided
    # configuration (``config.*``); ``meta`` holds optional caller-supplied
    # event metadata (Mobius adds its own provenance).
    event: dict[str, Any] | None = None
    config: dict[str, Any] | None = None
    meta: dict[str, Any] | None = None
    source: LoopRunSource | None = None
    external_id: str | None = None


@dataclass
class ListRunsOptions:
    status: LoopRunStatus | None = None
    loop_id: str | None = None
    cursor: str | None = None
    limit: int | None = None


@dataclass
class WaitRunOptions:
    since: int = 0
    reconnect_delay: float = 1.0
    timeout: float | None = None


@dataclass
class RunEvent:
    id: str
    run_id: str
    event_type: str
    sequence: int
    payload: dict[str, Any] | None = None


@dataclass
class InvokeAgentOptions:
    # Ordered content blocks (text, images, ...) for the caller's input
    # message. Required.
    content: list[dict[str, Any]]
    # Agent identifier. Mutually exclusive with agent_name.
    agent_id: str | None = None
    # Project-unique agent name. Mutually exclusive with agent_id.
    agent_name: str | None = None
    # Dedup key scoped to the resolved session. A repeat call with the same
    # key resolves the same session and resumes the existing turn rather
    # than starting a second one — derive it from the provider event id for
    # Slack/Telegram webhook retries.
    idempotency_key: str | None = None
    # Free-form caller metadata attached to the input message.
    input_metadata: dict[str, Any] | None = None
    # How to resolve or create the session this invocation runs in. Omit to
    # use a single default session per agent in continue_or_create mode. Set
    # session.thinking_effort to override the agent's reasoning-effort default
    # for this session.
    session: InvokeSessionSpec | None = None
    # Inline agent definition (instructions, model, effort, timeout, toolkits,
    # skills) sent with the invocation instead of using the agent stored in
    # Mobius. Set fields replace the agent's values; omitted fields keep them.
    # Mobius remembers the config on the session and reuses it on later turns
    # until a new one is sent. Omit to run the agent on its stored definition.
    config: InlineAgentConfig | None = None
    # Optional messaging provider/channel routing context (Slack, Telegram,
    # ...) recorded on the started turn.
    channel_context: ChannelContext | None = None


@dataclass
class SessionStreamEvent:
    # event_type is the authoritative SSE `event:` name (e.g.
    # "turn.completed"); validate data with the payload model matching it —
    # SessionStreamFrame is a reference-only union, ambiguous from data alone.
    event_type: str
    data: dict[str, Any]


class LeaseLostError(Exception):
    def __init__(self, job_id: str) -> None:
        super().__init__(f"lease lost for job {job_id}")
        self.job_id = job_id


class PayloadTooLargeError(Exception):
    def __init__(self, job_id: str) -> None:
        super().__init__(f"custom event payload too large for job {job_id}")
        self.job_id = job_id


class RateLimitedError(RateLimitError):
    def __init__(self, job_id: str, retry_after: int | None = None) -> None:
        msg = f"custom event rate limited for job {job_id}"
        if retry_after:
            msg += f" (retry after {retry_after}s)"
        super().__init__(retry_after=float(retry_after or 0), message=msg)
        self.job_id = job_id
        self.retry_after = retry_after


class Client:
    """Mobius public API client for loops, runs, and workers."""

    def __init__(
        self,
        opts: ClientOptions | str,
        *,
        base_url: str | None = None,
        project: str | None = None,
        namespace: str | None = None,
        timeout: float = 60.0,
        retry: int = DEFAULT_MAX_RETRIES,
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        if isinstance(opts, str):
            opts = ClientOptions(
                api_key=opts,
                base_url=base_url or DEFAULT_BASE_URL,
                project=project or namespace or "default",
                timeout=timeout,
                retry=retry,
            )
        elif base_url is not None:
            opts.base_url = base_url
        handle = _extract_handle_from_api_key(opts.api_key)
        explicit = opts.project or opts.namespace
        if handle:
            if explicit and explicit != "default" and explicit != handle:
                raise ValueError(
                    f"project={explicit!r} conflicts with the handle embedded in the API key ({handle!r})"
                )
            self.project = handle
        else:
            self.project = explicit or "default"
        self.base_url = opts.base_url.rstrip("/")
        self.api_key = opts.api_key
        base_transport = transport or httpx.HTTPTransport()
        self._client = httpx.Client(
            base_url=self.base_url,
            timeout=opts.timeout,
            transport=RetryingTransport(base_transport, max_retries=opts.retry),
            headers={
                "Authorization": f"Bearer {opts.api_key}",
                "Content-Type": "application/json",
            },
        )

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()

    def worker_socket_url(self) -> str:
        parsed = urlparse(self.base_url)
        scheme = "wss" if parsed.scheme == "https" else "ws"
        base_path = parsed.path.rstrip("/")
        path = f"{base_path}/v1/projects/{quote(self.project, safe='')}/workers/socket"
        return urlunparse((scheme, parsed.netloc, path, "", "", ""))

    def list_loops(self, opts: ListLoopsOptions | None = None) -> LoopListResponse:
        resp = self._request("GET", "/v1/projects/{project}/loops", params=_params(opts))
        return LoopListResponse.model_validate(resp.json())

    def get_loop(self, loop_id: str) -> Loop:
        resp = self._request("GET", f"/v1/projects/{{project}}/loops/{quote(loop_id, safe='')}")
        return Loop.model_validate(resp.json())

    def create_loop(self, opts: LoopOptions) -> Loop:
        body = CreateLoopRequest(**_merge_loop_fields(opts))
        resp = self._request("POST", "/v1/projects/{project}/loops", json=body)
        return Loop.model_validate(resp.json())

    def update_loop(self, loop_id: str, opts: UpdateLoopOptions) -> Loop:
        body = UpdateLoopRequest(**_merge_loop_fields(opts))
        resp = self._request("PATCH", f"/v1/projects/{{project}}/loops/{quote(loop_id, safe='')}", json=body)
        return Loop.model_validate(resp.json())

    def delete_loop(self, loop_id: str) -> None:
        self._request("DELETE", f"/v1/projects/{{project}}/loops/{quote(loop_id, safe='')}")

    def start_run(self, loop_id: str, opts: StartRunOptions | None = None) -> LoopRun:
        opts = opts or StartRunOptions()
        values = _drop_none(opts.__dict__)
        if "external_id" in values:
            values["idempotency_key"] = values.pop("external_id")
        body = StartLoopRunRequest(**values)
        resp = self._request("POST", f"/v1/projects/{{project}}/loops/{quote(loop_id, safe='')}/runs", json=body)
        return LoopRun.model_validate(resp.json())

    def list_runs(self, opts: ListRunsOptions | None = None) -> LoopRunListResponse:
        resp = self._request("GET", "/v1/projects/{project}/runs", params=_params(opts))
        return LoopRunListResponse.model_validate(resp.json())

    def get_run(self, run_id: str) -> LoopRun:
        resp = self._request("GET", f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}")
        return LoopRun.model_validate(resp.json())

    def cancel_run(self, run_id: str, reason: str | None = None) -> LoopRun:
        body = CancelLoopRunRequest(reason=reason)
        resp = self._request("POST", f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}/cancel", json=body)
        return LoopRun.model_validate(resp.json())

    def signal_run(
        self,
        run_id: str,
        step_key: str,
        result: dict[str, Any] | None = None,
    ) -> LoopRun:
        body = SignalLoopRunRequest(step_key=step_key, result=result)
        resp = self._request("POST", f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}/signals", json=body)
        return LoopRun.model_validate(resp.json())

    # Resolves (or creates) a session, appends opts.content as the caller's
    # input message, and starts an agent turn in one retryable call. Returns
    # once the turn is accepted; use invoke_agent_stream to observe the
    # turn's activity inline on the same connection instead.
    def invoke_agent(self, opts: InvokeAgentOptions) -> TurnAck:
        body = _invoke_agent_request(opts)
        resp = self._request("POST", "/v1/projects/{project}/agents/invoke", json=body)
        return TurnAck.model_validate(resp.json())

    # Behaves like invoke_agent but streams the turn's activity inline on
    # the same connection instead of waiting for a TurnAck.
    def invoke_agent_stream(self, opts: InvokeAgentOptions) -> Iterator[SessionStreamEvent]:
        body = _invoke_agent_request(opts)
        payload = _model_dump(body)
        path = self._path("/v1/projects/{project}/agents/invoke")
        with self._client.stream(
            "POST", path, json=payload, headers={"Accept": "text/event-stream"}
        ) as resp:
            resp.raise_for_status()
            buf = ""
            for chunk in resp.iter_text():
                buf += chunk
                while "\n\n" in buf:
                    raw, buf = buf.split("\n\n", 1)
                    lines = raw.splitlines()
                    data = "\n".join(
                        line.removeprefix("data:").lstrip()
                        for line in lines
                        if line.startswith("data:")
                    )
                    event_type = next(
                        (
                            line.removeprefix("event:").lstrip()
                            for line in lines
                            if line.startswith("event:")
                        ),
                        None,
                    )
                    if not data or not event_type:
                        continue
                    yield SessionStreamEvent(event_type=event_type, data=json.loads(data))

    # Fetch a session transcript snapshot (session-stream v2). Without a cursor
    # this is a bootstrap tail (latest final page + all live rows and turns);
    # with a cursor it drains everything after it toward a fixed upper cut —
    # continue with the returned next_page_token until has_more is false. Fold
    # each page into a SessionTranscriptReducer with apply_snapshot.
    def get_session_transcript(
        self,
        session_id: str,
        opts: GetSessionTranscriptOptions | None = None,
    ) -> SessionTranscriptSnapshot:
        params: dict[str, Any] = {}
        if opts is not None:
            if opts.cursor:
                params["cursor"] = opts.cursor
            if opts.page_token:
                params["page_token"] = opts.page_token
            if opts.limit:
                params["limit"] = opts.limit
        path = f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/transcript"
        resp = self._request("GET", path, params=params)
        return SessionTranscriptSnapshot.model_validate(resp.json())

    # Open one session-transcript SSE connection and yield each decoded frame
    # with its resume cursor (TranscriptStreamEvent.id). Apply the frames to a
    # SessionTranscriptReducer, or use watch_session_transcript for the managed
    # connection loop (reconnect on rotate, stop on idle).
    def stream_session_transcript(
        self,
        session_id: str,
        opts: StreamSessionTranscriptOptions | None = None,
    ) -> Iterator[TranscriptStreamEvent]:
        params = {"cursor": opts.cursor} if opts and opts.cursor else None
        path = f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/transcript/stream"
        with self._client.stream(
            "GET", self._path(path, params=params), headers={"Accept": "text/event-stream"}
        ) as resp:
            _raise_for_stream_status(resp)
            yield from _iter_transcript_frames(resp)

    # Follow a session-transcript stream across the full connection lifecycle,
    # yielding every decoded frame. Reconnects with the current cursor on a
    # stream.end rotate (and after a dropped connection), and returns on a
    # stream.end idle. Apply the frames to a SessionTranscriptReducer; reconnect
    # is the same code path as the first connect. On idle the caller can poll
    # get_session_transcript and reopen when resume_cursor moves.
    def watch_session_transcript(
        self,
        session_id: str,
        opts: WatchSessionTranscriptOptions | None = None,
    ) -> Iterator[TranscriptStreamEvent]:
        cursor = opts.cursor if opts else None
        delay = opts.reconnect_delay if opts is not None else 1.0
        yield from self._transcript_loop(session_id, cursor, None, delay)

    # Behaves like invoke_agent but returns the started turn's transcript stream
    # (session-stream v2) alongside the ack. Returns (ack, iterator): the
    # iterator opens the transcript stream from the ack's resume cursor,
    # forwards every frame across reconnects, and stops when this turn reaches a
    # terminal turn.upsert (or on idle). Seed a SessionTranscriptReducer with
    # the ack's user_message and turn, then apply the iterator's frames; filter
    # with messages_for_turn(ack.turn.id) to render only this turn.
    def invoke_agent_transcript(
        self,
        opts: InvokeAgentOptions,
    ) -> tuple[TurnAck, Iterator[TranscriptStreamEvent]]:
        ack = self.invoke_agent(opts)
        stream = self._transcript_loop(ack.session.id, ack.resume_cursor, ack.turn.id, 1.0)
        return ack, stream

    def _transcript_loop(
        self,
        session_id: str,
        cursor: str | None,
        stop_turn_id: str | None,
        delay: float,
    ) -> Iterator[TranscriptStreamEvent]:
        path = f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/transcript/stream"
        while True:
            params = {"cursor": cursor} if cursor else None
            rotate = False
            try:
                with self._client.stream(
                    "GET",
                    self._path(path, params=params),
                    headers={"Accept": "text/event-stream"},
                ) as resp:
                    _raise_for_stream_status(resp)
                    for event in _iter_transcript_frames(resp):
                        if event.id:
                            cursor = event.id
                        frame = event.frame
                        if frame.get("event_type") == "stream.ready":
                            cursor = frame.get("resume_cursor", cursor)
                        yield event
                        event_type = frame.get("event_type")
                        if event_type == "stream.end":
                            if frame.get("reason") == "idle":
                                return
                            rotate = True  # reconnect immediately
                            break
                        if (
                            event_type == "turn.upsert"
                            and stop_turn_id
                            and frame.get("id") == stop_turn_id
                            and is_terminal_turn_status(frame.get("status", ""))
                        ):
                            return
            except httpx.HTTPStatusError as exc:
                # A permanent status (401/403/404, or a 5xx other than 503) is
                # not transient — surface it instead of reconnecting forever.
                if not _is_retryable_stream_status(exc.response.status_code):
                    raise
                rotate = False  # 429/503 — reconnect after the delay
            except httpx.TransportError:
                rotate = False  # dropped connection / EOF — reconnect after delay
            if not rotate:
                time.sleep(delay)

    def watch_run(self, run_id: str, since: int = 0) -> Iterator[RunEvent]:
        params = {"after_sequence": since} if since > 0 else None
        path = f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}/events.stream"
        with self._client.stream("GET", self._path(path, params=params)) as resp:
            resp.raise_for_status()
            buf = ""
            for chunk in resp.iter_text():
                buf += chunk
                while True:
                    sep = _SSE_FRAME_SEP.search(buf)
                    if sep is None:
                        break
                    raw, buf = buf[: sep.start()], buf[sep.end() :]
                    data = "\n".join(
                        line.removeprefix("data:").lstrip()
                        for line in raw.splitlines()
                        if line.startswith("data:")
                    )
                    if not data:
                        continue
                    event = LoopRunEvent.model_validate_json(data)
                    yield RunEvent(
                        id=event.id,
                        run_id=event.run_id,
                        event_type=event.event_type,
                        sequence=event.sequence,
                        payload=event.payload,
                    )

    def wait_run(self, run_id: str, opts: WaitRunOptions | None = None) -> LoopRun:
        opts = opts or WaitRunOptions()
        since = opts.since
        deadline = time.monotonic() + opts.timeout if opts.timeout else None
        while True:
            run = self.get_run(run_id)
            if is_terminal_run_status(run.status):
                return run
            for event in self.watch_run(run_id, since=since):
                since = max(since, event.sequence)
                status = (event.payload or {}).get("status")
                if isinstance(status, str) and is_terminal_run_status(status):
                    return self.get_run(run_id)
                if deadline is not None and time.monotonic() >= deadline:
                    raise TimeoutError(f"timed out waiting for run {run_id}")
            if deadline is not None and time.monotonic() >= deadline:
                raise TimeoutError(f"timed out waiting for run {run_id}")
            time.sleep(opts.reconnect_delay)

    def _request(
        self,
        method: str,
        path: str,
        *,
        json: Any | None = None,
        params: dict[str, Any] | None = None,
    ) -> httpx.Response:
        payload = _model_dump(json) if json is not None else None
        resp = self._client.request(method, self._path(path, params=params), json=payload)
        if resp.status_code == 401:
            raise AuthRevokedError()
        resp.raise_for_status()
        return resp

    def _path(self, path: str, params: dict[str, Any] | None = None) -> str:
        out = path.replace("{project}", quote(self.project, safe=""))
        if params:
            clean = {k: v for k, v in params.items() if v not in (None, "")}
            if clean:
                out = f"{out}?{urlencode(clean)}"
        return out


# SSE events are separated by a blank line, which may be framed with LF or
# CRLF — both are valid per the spec. Match either so a CRLF-framed stream
# still splits into frames instead of buffering forever.
_SSE_FRAME_SEP = re.compile(r"\r?\n\r?\n")


def _raise_for_stream_status(resp: httpx.Response) -> None:
    # Mirror _request: a revoked credential is surfaced as AuthRevokedError so
    # callers get the same typed error whether they stream or not.
    if resp.status_code == 401:
        raise AuthRevokedError()
    resp.raise_for_status()


def _is_retryable_stream_status(status: int) -> bool:
    # Mirror the transport retry policy (docs/retries.md): only 429 and 503 are
    # transient. Every other status — including 401/403/404 and the other 5xx —
    # is surfaced to the caller instead of triggering an endless reconnect.
    return status in (429, 503)


def _iter_transcript_frames(resp: httpx.Response) -> Iterator[TranscriptStreamEvent]:
    """Parse a transcript SSE body into TranscriptStreamEvents.

    Per the SSE spec the last-event-id persists across events until an ``id:``
    line changes it — the transcript stream relies on this so a frame that
    repeats already-delivered state can omit ``id:`` without regressing the
    resume cursor.
    """
    buf = ""
    last_id: str | None = None
    for chunk in resp.iter_text():
        buf += chunk
        while True:
            sep = _SSE_FRAME_SEP.search(buf)
            if sep is None:
                break
            raw, buf = buf[: sep.start()], buf[sep.end() :]
            lines = raw.splitlines()
            data = "\n".join(
                line.removeprefix("data:").lstrip()
                for line in lines
                if line.startswith("data:")
            )
            if not data:
                continue
            event_type = next(
                (line.removeprefix("event:").lstrip() for line in lines if line.startswith("event:")),
                None,
            )
            id_line = next(
                (line.removeprefix("id:").lstrip() for line in lines if line.startswith("id:")),
                None,
            )
            if id_line is not None:
                last_id = id_line
            frame = json.loads(data)
            yield TranscriptStreamEvent(
                event_type=event_type or frame.get("event_type", ""),
                frame=frame,
                id=last_id,
            )


def _extract_handle_from_api_key(api_key: str) -> str | None:
    if not (api_key.startswith("mbx_") or api_key.startswith("mbc_")):
        return None
    if "." not in api_key:
        return None
    handle = api_key.rsplit(".", 1)[1]
    if not handle:
        return None
    if not _HANDLE_RE.match(handle):
        raise ValueError(f"invalid project handle suffix in API key: {handle!r}")
    return handle


def _model_dump(value: Any) -> Any:
    if hasattr(value, "model_dump"):
        return value.model_dump(mode="json", exclude_none=True)
    if isinstance(value, dict):
        return {k: _model_dump(v) for k, v in value.items() if v is not None}
    if isinstance(value, list):
        return [_model_dump(v) for v in value]
    return value


def _drop_none(values: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in values.items() if v is not None}


def _invoke_agent_request(opts: InvokeAgentOptions) -> InvokeAgentRequest:
    if not opts.agent_id and not opts.agent_name:
        raise ValueError("mobius: invoke agent: agent_id or agent_name is required")
    if not opts.content:
        raise ValueError("mobius: invoke agent: content is required")
    return InvokeAgentRequest(
        agent_ref=AgentRef(id=opts.agent_id, name=opts.agent_name),
        input=InvokeInput(
            content=opts.content,
            idempotency_key=opts.idempotency_key,
            metadata=opts.input_metadata,
        ),
        session=opts.session,
        config=opts.config,
        channel_context=opts.channel_context,
    )


def _merge_loop_fields(opts: Any) -> dict[str, Any]:
    """Flatten loop options into loop request fields.

    The loop spec (steps, event, config, triggers, ...) lives inline on the
    loop, so the ``spec`` mapping is merged into the top-level request fields.
    Explicit option fields take precedence over the same keys in ``spec``.
    """
    fields = dict(opts.__dict__)
    spec = fields.pop("spec", None) or {}
    return {**spec, **_drop_none(fields)}


def _params(opts: Any | None) -> dict[str, Any] | None:
    if opts is None:
        return None
    if hasattr(opts, "__dict__"):
        values = opts.__dict__
    else:
        values = dict(opts)
    return {k: _query_value(v) for k, v in _drop_none(values).items()}


def _query_value(value: Any) -> Any:
    return value.value if hasattr(value, "value") else value


def is_terminal_run_status(status: LoopRunStatus | str) -> bool:
    value = status.value if hasattr(status, "value") else str(status)
    return value in {"completed", "failed", "cancelled"}
