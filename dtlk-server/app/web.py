import re

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from starlette.convertors import Convertor, register_url_convertor

from .service import ApiError


class CodeConvertor(Convertor[str]):
    """Matches exactly six digits, so other paths fall through to a plain 404."""

    regex = r"[0-9]{6}"

    def convert(self, value: str) -> str:
        return value

    def to_string(self, value: str) -> str:
        return value


register_url_convertor("code", CodeConvertor())

RANGE_RE = re.compile(r"^bytes=(\d+)-(\d*)$")


def client_ip(request: Request) -> str:
    # uvicorn's proxy-headers middleware has already applied X-Forwarded-For
    # from trusted proxies (DTLK_TRUSTED_PROXY_IPS).
    return request.client.host if request.client else "unknown"


def install_error_handler(app: FastAPI) -> None:
    @app.exception_handler(ApiError)
    async def _api_error(_: Request, exc: ApiError) -> JSONResponse:
        headers = {"Retry-After": str(exc.retry_after)} if exc.retry_after else None
        return JSONResponse({"detail": exc.detail}, status_code=exc.status, headers=headers)
