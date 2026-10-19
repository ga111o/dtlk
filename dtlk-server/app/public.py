import logging
import mimetypes
from collections.abc import Iterable, Iterator
from typing import Any

from fastapi import FastAPI, Header, Request, Response
from fastapi.responses import FileResponse, PlainTextResponse, StreamingResponse
from pydantic import BaseModel
from starlette.concurrency import run_in_threadpool

from . import convert, listing
from .web import RANGE_RE, client_ip, install_error_handler
from .service import ApiError, Service, iso

log = logging.getLogger("dtlk")

DL_NAMES = {f"dtlk-linux-{arch}{ext}" for arch in ("amd64", "arm64") for ext in ("", ".sha256")}


class CreateUpload(BaseModel):
    name: str
    kind: str
    estimated_size: int
    hours: int = 24
    tag: str | None = None


def _body_chunks(body: Any, chunks: Iterable[bytes] | None = None) -> Iterator[bytes]:
    """Yield from chunks (or the raw S3 body) and always release the S3 connection."""
    try:
        yield from chunks if chunks is not None else body.iter_chunks(convert.CHUNK)
    finally:
        body.close()


def create_public_app(svc: Service) -> FastAPI:
    app = FastAPI(title="dtlk", docs_url=None, redoc_url=None, openapi_url=None)
    install_error_handler(app)
    cfg = svc.cfg

    @app.get("/api/health")
    def health() -> dict[str, bool]:
        return {"ok": True}

    @app.post("/api/uploads")
    def create_upload(body: CreateUpload, request: Request) -> dict[str, Any]:
        return svc.create_upload(client_ip(request), body.name, body.kind, body.estimated_size, body.hours, body.tag)

    @app.put("/api/uploads/{upload_id}/parts/{n:int}")
    async def put_part(upload_id: str, n: int, request: Request) -> dict[str, bool]:
        length = request.headers.get("content-length", "")
        if not length.isdigit():
            raise ApiError(411, "Content-Length is required")
        if int(length) > cfg.chunk_size:
            raise ApiError(413, "Part is too large")
        data = bytearray()
        async for chunk in request.stream():
            data += chunk
            if len(data) > cfg.chunk_size:
                raise ApiError(413, "Part is too large")
        if len(data) != int(length):
            raise ApiError(400, "Incomplete part body")
        await run_in_threadpool(svc.put_part, upload_id, n, bytes(data))
        return {"ok": True}

    @app.post("/api/uploads/{upload_id}/complete")
    def complete(upload_id: str) -> dict[str, Any]:
        return svc.complete_upload(upload_id)

    @app.delete("/api/uploads/{upload_id}", status_code=204)
    def abort(upload_id: str) -> Response:
        svc.abort_upload(upload_id)
        return Response(status_code=204)

    @app.get("/api/files/{code:code}")
    def file_info(code: str, request: Request) -> dict[str, Any]:
        row = svc.lookup(client_ip(request), code)
        return {"name": row["name"], "kind": row["kind"], "size": row["size"], "expires_at": iso(row["expires_at"])}

    @app.get("/api/files/{code:code}/download")
    def download(code: str, request: Request, range: str | None = Header(default=None)) -> Response:
        row = svc.lookup(client_ip(request), code)
        size = row["size"]
        byte_range = None
        if range and (m := RANGE_RE.match(range.strip())):
            start = int(m[1])
            end = int(m[2]) if m[2] else size - 1
            if start >= size or end < start:
                return Response(status_code=416, headers={"Content-Range": f"bytes */{size}"})
            byte_range = f"bytes={start}-{min(end, size - 1)}"
        obj = svc.storage.get(row["object_key"], byte_range)
        headers = {"Content-Length": str(obj["ContentLength"]), "Accept-Ranges": "bytes"}
        status = 200
        if byte_range:
            status = 206
            headers["Content-Range"] = obj["ContentRange"]
        return StreamingResponse(
            _body_chunks(obj["Body"]), status_code=status, headers=headers, media_type="application/octet-stream"
        )

    @app.get("/files/{tag:tag}")
    def file_list(tag: str, request: Request) -> Response:
        as_html = "text/html" in request.headers.get("accept", "")
        tag = tag.lower()
        try:
            rows = svc.list_tag(client_ip(request), tag)
        except ApiError as exc:
            return listing.render_error(exc, as_html)
        return listing.render(tag, rows, as_html)

    @app.get("/install.sh")
    def install_sh() -> PlainTextResponse:
        script = (cfg.static_dir / "install.sh").read_text()
        return PlainTextResponse(script.replace("__DTLK_ORIGIN__", cfg.public_url), media_type="text/x-shellscript")

    @app.get("/dl/{name}")
    def dl(name: str) -> Response:
        path = cfg.static_dir / "dl" / name
        if name not in DL_NAMES or not path.is_file():
            return PlainTextResponse("Not Found", status_code=404)
        return FileResponse(path, media_type="application/octet-stream", filename=name)

    @app.get("/{code:code}")
    def browser_download(code: str, request: Request) -> StreamingResponse:
        row = svc.lookup(client_ip(request), code)
        body = svc.storage.get(row["object_key"])["Body"]
        try:
            tf = convert.open_tar(body)
            if row["kind"] == "file":
                size, chunks = convert.single_file(tf)
                return StreamingResponse(
                    _body_chunks(body, chunks),
                    headers={"Content-Length": str(size), "Content-Disposition": convert.content_disposition(row["name"])},
                    media_type=mimetypes.guess_type(row["name"])[0] or "application/octet-stream",
                )
            return StreamingResponse(
                _body_chunks(body, convert.zip_stream(tf)),
                headers={"Content-Disposition": convert.content_disposition(row["name"] + ".zip")},
                media_type="application/zip",
            )
        except Exception:
            body.close()
            log.exception("browser download of %s failed", code)
            raise ApiError(500, "Failed to open archive")

    return app
