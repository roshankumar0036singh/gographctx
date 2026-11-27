"""Package marker for order services."""

from .flask_service import OrderController, sync_orders

__all__ = ["OrderController", "sync_orders"]
PACKAGE_READY = True


def create_app() -> OrderController:
    return OrderController.from_env()
