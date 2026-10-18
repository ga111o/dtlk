import asyncio
import logging
import time

from .service import Service

log = logging.getLogger("dtlk")


async def cleanup_loop(svc: Service, stop: asyncio.Event) -> None:
    """Delete expired files / stale uploads every minute, reconcile orphans every 10 minutes."""
    last_reconcile = float("-inf")
    while not stop.is_set():
        try:
            await asyncio.to_thread(svc.cleanup_once)
        except Exception:
            log.exception("cleanup failed")
        if time.monotonic() - last_reconcile >= svc.cfg.reconcile_interval_sec:
            last_reconcile = time.monotonic()
            try:
                await asyncio.to_thread(svc.reconcile)
            except Exception:
                log.exception("reconcile failed")
        try:
            await asyncio.wait_for(stop.wait(), timeout=svc.cfg.cleanup_interval_sec)
        except TimeoutError:
            pass
