"""Entry point: public API (:8000), admin API (:8001) and the cleanup loop in one process."""

import asyncio
import contextlib
import logging
import os
import signal
import time

import uvicorn

from .admin import create_admin_app
from .cleanup import cleanup_loop
from .config import Config
from .db import DB
from .public import create_public_app
from .service import Service
from .storage import Storage

log = logging.getLogger("dtlk")


class _Server(uvicorn.Server):
    # Two servers share one process, so signals are handled once in main().
    @contextlib.contextmanager
    def capture_signals(self):
        yield


def _wait_for_storage(storage: Storage, timeout: float = 120) -> None:
    deadline = time.monotonic() + timeout
    while True:
        try:
            storage.ensure_bucket()
            return
        except Exception as exc:
            if time.monotonic() > deadline:
                raise
            log.warning("waiting for storage: %s", exc)
            time.sleep(2)


async def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    cfg = Config.from_env()
    storage = Storage(cfg)
    await asyncio.to_thread(_wait_for_storage, storage)
    svc = Service(cfg, DB(cfg.db_path), storage)

    trusted = os.environ.get("DTLK_TRUSTED_PROXY_IPS", "").strip() or "127.0.0.1"
    public = _Server(
        uvicorn.Config(
            create_public_app(svc),
            host="0.0.0.0",
            port=8000,
            proxy_headers=True,
            forwarded_allow_ips=trusted,
            timeout_keep_alive=75,
            server_header=False,
            lifespan="off",
        )
    )
    admin = _Server(uvicorn.Config(create_admin_app(svc), host="0.0.0.0", port=8001, access_log=False, lifespan="off"))

    stop = asyncio.Event()

    def shutdown() -> None:
        stop.set()
        public.should_exit = admin.should_exit = True

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, shutdown)

    log.info("trusted proxies: %s", trusted)
    await asyncio.gather(public.serve(), admin.serve(), cleanup_loop(svc, stop))


if __name__ == "__main__":
    asyncio.run(main())
