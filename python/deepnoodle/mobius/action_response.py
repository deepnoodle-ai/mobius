"""Helpers for responses from customer-owned signed HTTP actions."""

from typing import Any, TypedDict

from ._api.models import RuntimeContextItem

MOBIUS_ACTION_CONTENT_TYPE = "application/vnd.mobius.action+json"


class ActionResponseEnvelope(TypedDict, total=False):
    """Canonical response body for a signed HTTP action endpoint."""

    output: Any
    context: list[RuntimeContextItem]
