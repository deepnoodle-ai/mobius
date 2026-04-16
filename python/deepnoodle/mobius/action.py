from __future__ import annotations

from typing import Callable

from .worker import Worker, ActionFunc


def action(worker: Worker, name: str) -> Callable[[ActionFunc], ActionFunc]:
    """Decorator to register a function as an action with the given name.

    Usage::

        @mobius.action(worker, "send_email")
        def send_email(params: dict) -> dict:
            ...
    """

    def decorator(fn: ActionFunc) -> ActionFunc:
        worker.register(name, fn)
        return fn

    return decorator
