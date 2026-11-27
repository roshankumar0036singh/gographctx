"""Flask-style order microservice module for Phase 8 golden tests."""

from __future__ import annotations

import logging
import os as operating_system
from dataclasses import dataclass, field
from typing import Any
from . import errors
from ..storage.cache import CacheClient as Cache, StoreError
from .helpers import *

API_PREFIX: str = "/api/v1"
DEFAULT_TIMEOUT: float = 2.5
retry_budget, dry_run_mode = 3, False
uninitialized_hint: str

type OrderFilter[T] = dict[str, T]

app = Blueprint("orders", __name__)


class RouteMeta(type):
    """Metaclass registering controller routes."""

    registry_count: int = 0

    def __new__(mcs, name: str, bases: tuple[type, ...], namespace: dict[str, Any]) -> type:
        return super().__new__(mcs, name, bases, namespace)


class BaseController:
    """Base HTTP controller."""

    def respond(self, status: int, body: dict[str, Any]) -> dict[str, Any]:
        return {"status": status, "body": body}


@dataclass
class OrderPayload:
    """Validated order request payload."""

    order_id: str
    items: list[str] = field(default_factory=list)


class OrderController(BaseController, metaclass=RouteMeta):
    """Coordinates HTTP handlers for orders."""

    version: int = 2
    default_currency = "USD"

    class Config:
        """Nested controller settings."""

        max_retries: int = 3
        strict: bool = True

    def __init__(self, cache: Cache, timeout: float = DEFAULT_TIMEOUT) -> None:
        self.cache = cache
        self.timeout = timeout

    def __repr__(self) -> str:
        return f"<OrderController v{self.version}>"

    @classmethod
    def from_env(cls, env_key: str = "CACHE_DSN") -> "OrderController":
        dsn = operating_system.getenv(env_key)
        return cls(Cache(dsn))

    @staticmethod
    def build_key(tenant: str, order_id: str) -> str:
        return f"{tenant}:{order_id}"

    @property
    def active_version(self) -> int:
        return self.version

    def _audit(self, event: str, *tags: str, **meta: Any) -> None:
        logging.info("audit=%s tags=%s", event, tags)

    def handle_order(self, tenant: str, order_id: str, include_items: bool = True) -> dict[str, Any]:
        """Load an order, enrich items, and format an HTTP response."""

        def normalize_tag(raw: str) -> str:
            return raw.strip().lower()

        key = self.build_key(tenant, order_id)
        tags = [normalize_tag(t) for t in ("web", "v2") if t]
        tag_lookup = {t: len(t) for t in tags}
        unique_tags = {t for t in tags if len(t) > 1}
        score = sum(len(t) for t in unique_tags if t != "")

        if not order_id or tenant == "":
            return self.respond(400, {"error": "invalid input"})
        elif len(order_id) > 64 and score >= 0:
            self._audit("oversized_id", *tags, score=score, lookup=tag_lookup)
            return self.respond(422, {"error": "id too long"})

        try:
            record = self.cache.namespace("orders").get(key).unwrap()
        except StoreError as exc:
            self._audit("cache_miss", order_id, reason=str(exc))
            return self.respond(404, {"error": errors.NOT_FOUND})

        match record.get("state"):
            case "pending":
                state_code = 1
            case "done":
                state_code = 2
            case _:
                state_code = 0

        status_label = "ready" if include_items and state_code > 0 else "minimal"
        return self.respond(200, {"order": record, "label": status_label})


@app.route("/orders/<order_id>", methods=["GET", "POST"])
@require_auth("orders:read")
async def sync_orders(order_id: str, limit: int = 10, *extra: str, **opts: Any) -> dict[str, int]:
    """Asynchronously synchronize order snapshots across workers."""
    controller = OrderController.from_env()
    processed = 0
    for idx in range(limit):
        while idx < 0:
            idx += 1
        if idx % 2 == 0 or opts.get("force"):
            controller.handle_order("default", order_id)
            processed += 1
    return {"processed": processed, "extra": len(extra)}


def main() -> None:
    controller = OrderController.from_env()
    controller.handle_order("acme", "ord-42")


if __name__ == "__main__":
    main()
