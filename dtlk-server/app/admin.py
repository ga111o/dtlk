"""Admin API for the Streamlit dashboard. Served on an internal-only port."""

from typing import Any

from fastapi import FastAPI, Response
from pydantic import BaseModel, Field

from .service import ApiError, Service
from .web import install_error_handler


class SettingsUpdate(BaseModel):
    storage_cap_bytes: int | None = Field(default=None, ge=1)
    lookup_fail_max: int | None = Field(default=None, ge=1)
    lookup_fail_window_sec: int | None = Field(default=None, ge=1)
    lookup_block_sec: int | None = Field(default=None, ge=1)
    upload_concurrent_max: int | None = Field(default=None, ge=1)
    upload_hourly_max: int | None = Field(default=None, ge=1)


def create_admin_app(svc: Service) -> FastAPI:
    app = FastAPI(title="dtlk admin", docs_url=None, redoc_url=None)
    install_error_handler(app)

    @app.get("/admin/health")
    def health() -> dict[str, bool]:
        return {"ok": True}

    @app.get("/admin/overview")
    def overview() -> dict[str, Any]:
        return svc.overview()

    @app.get("/admin/settings")
    def get_settings() -> dict[str, int]:
        return svc.settings()

    @app.put("/admin/settings")
    def put_settings(body: SettingsUpdate) -> dict[str, int]:
        return svc.update_settings(body.model_dump(exclude_none=True))

    @app.delete("/admin/files/{code}", status_code=204)
    def delete_file(code: str) -> Response:
        if not svc.delete_file(code):
            raise ApiError(404, "그런 파일이 없습니다")
        return Response(status_code=204)

    @app.delete("/admin/uploads/{upload_id}", status_code=204)
    def abort_upload(upload_id: str) -> Response:
        if not svc.abort_upload(upload_id):
            raise ApiError(404, "그런 업로드가 없습니다")
        return Response(status_code=204)

    @app.get("/admin/blocks")
    def blocks() -> dict[str, Any]:
        return svc.blocks()

    @app.delete("/admin/blocks/{ip}", status_code=204)
    def unblock(ip: str) -> Response:
        svc.unblock(ip)
        return Response(status_code=204)

    return app
