"""Shared helpers for the Python SDK test suite."""

from __future__ import annotations

import json
from datetime import datetime
from pathlib import Path
from typing import Any

CONTRACT_DIR = Path(__file__).resolve().parents[2] / "testdata" / "contract"


def load_manifest() -> list[dict[str, str]]:
    with (CONTRACT_DIR / "manifest.json").open() as f:
        return json.load(f)["fixtures"]


def load_fixture(name: str) -> Any:
    with (CONTRACT_DIR / name).open() as f:
        return json.load(f)


def _maybe_datetime(v: str) -> datetime | str:
    """Parse ISO 8601 strings containing 'T' as datetimes, leave others alone.

    Round-trips through different languages emit UTC as 'Z' (Go) or '+00:00'
    (Pydantic). Comparing as aware datetimes normalizes the difference.
    """
    if "T" not in v:
        return v
    try:
        return datetime.fromisoformat(v.replace("Z", "+00:00"))
    except ValueError:
        return v


def canonicalize(value: Any) -> Any:
    """Recursively convert ISO 8601 timestamp strings to datetime objects."""
    if isinstance(value, dict):
        return {k: canonicalize(v) for k, v in value.items()}
    if isinstance(value, list):
        return [canonicalize(v) for v in value]
    if isinstance(value, str):
        return _maybe_datetime(value)
    return value
