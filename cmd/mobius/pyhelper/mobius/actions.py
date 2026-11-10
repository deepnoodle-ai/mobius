"""Call Mobius actions from a workflow script step.

A ``run:`` or ``python:`` step lists the actions it may call in its Actions.
Each call acts as the workflow and is recorded under the step. The script
holds no credential: the Mobius worker in the computer sends each call for
the step's own job.

    from mobius import actions

    page = actions.run("mobius.table.find_rows", {"table_name": "leads", "limit": 50})
    actions.run("slack.message.post", {"channel": "C024BE91L", "text": "Done"},
                key="digest-2026-11-10")

Standard library only, so it works in every script step with no install.
"""

import json
import os
import socket

__all__ = ["ActionError", "run", "describe"]

_TIMEOUT_SECONDS = 360


class ActionError(Exception):
    """A call that Mobius refused or that failed. ``code`` says why."""

    def __init__(self, message, code=""):
        super().__init__(message)
        self.code = code


def run(name, input=None, key=None):
    """Run the action ``name`` with ``input`` (a dict) and return its output.

    ``key`` is optional. A repeat of the key within this step, across its
    attempts in one run, returns the first call's result and runs nothing.
    Raises ``ActionError`` when the call is refused or fails.
    """
    request = {"op": "run", "action": name, "input": input or {}}
    if key:
        request["key"] = key
    return _send(request).get("output")


def describe(name):
    """Return the action's title, inputs, and output shape as a dict."""
    return _send({"op": "describe", "action": name}).get("description")


def _send(request):
    path = os.environ.get("MOBIUS_ACTION_SOCKET", "").strip()
    if not path:
        raise ActionError(
            "Actions can be called only from a workflow script step.",
            "not_script_step",
        )
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.settimeout(_TIMEOUT_SECONDS)
    try:
        sock.connect(path)
        sock.sendall(json.dumps(request).encode("utf-8") + b"\n")
        chunks = []
        while True:
            chunk = sock.recv(65536)
            if not chunk:
                break
            chunks.append(chunk)
    except OSError as err:
        raise ActionError("Could not reach the Mobius worker: %s" % err, "worker_unreachable")
    finally:
        sock.close()
    try:
        response = json.loads(b"".join(chunks).decode("utf-8"))
    except ValueError:
        raise ActionError("The Mobius worker sent an unreadable answer.", "bad_response")
    if not response.get("ok"):
        raise ActionError(response.get("error") or "The call failed.", response.get("code", ""))
    return response
