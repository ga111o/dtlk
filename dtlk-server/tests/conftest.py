import io
import os
import tarfile
import time
from dataclasses import dataclass
from pathlib import Path

import boto3
import pytest
from compression import zstd
from fastapi.testclient import TestClient
from moto import mock_aws

from app.admin import create_admin_app
from app.config import Config, MiB
from app.db import DB
from app.public import create_public_app
from app.service import Service
from app.storage import Storage

CHUNK = 5 * MiB  # S3's minimum multipart part size


class Clock:
    def __init__(self) -> None:
        self.t = time.time()

    def __call__(self) -> float:
        return self.t

    def advance(self, seconds: float) -> None:
        self.t += seconds


@dataclass
class Env:
    svc: Service
    clock: Clock
    cfg: Config
    s3: object
    public: TestClient
    admin: TestClient

    def client(self, ip: str) -> TestClient:
        return TestClient(self.public.app, client=(ip, 50000))


@pytest.fixture
def env(tmp_path: Path):
    os.environ.setdefault("AWS_ACCESS_KEY_ID", "test")
    os.environ.setdefault("AWS_SECRET_ACCESS_KEY", "test")
    static = tmp_path / "static"
    (static / "dl").mkdir(parents=True)
    (static / "install.sh").write_text('ORIGIN="__DTLK_ORIGIN__"\n')
    (static / "dl" / "dtlk-linux-amd64").write_bytes(b"binary")
    with mock_aws():
        s3 = boto3.client("s3", region_name="us-east-1")
        cfg = Config(
            db_path=str(tmp_path / "dtlk.db"),
            static_dir=static,
            storage_path=str(tmp_path),
            public_url="https://dtlk.example.test",
            max_upload_bytes=20 * MiB,
            chunk_size=CHUNK,
        )
        storage = Storage(cfg, client=s3)
        storage.ensure_bucket()
        clock = Clock()
        svc = Service(cfg, DB(cfg.db_path), storage, clock)
        yield Env(svc, clock, cfg, s3, TestClient(create_public_app(svc)), TestClient(create_admin_app(svc)))


def make_archive(entries: dict[str, bytes | None], links: dict[str, str] | None = None) -> bytes:
    """tar.zst like the Go client makes: names ending in / are directories."""
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.PAX_FORMAT) as tf:
        for name, data in entries.items():
            info = tarfile.TarInfo(name.rstrip("/"))
            info.mtime = 1_700_000_000
            if data is None:
                info.type, info.mode = tarfile.DIRTYPE, 0o755
                tf.addfile(info)
            else:
                info.size, info.mode = len(data), 0o644
                tf.addfile(info, io.BytesIO(data))
        for name, target in (links or {}).items():
            info = tarfile.TarInfo(name)
            info.type, info.linkname, info.mode = tarfile.SYMTYPE, target, 0o777
            tf.addfile(info)
    return zstd.compress(raw.getvalue())


def upload(
    client: TestClient, data: bytes, name: str = "test1", kind: str = "dir", hours: int = 24, tag: str | None = None
) -> dict:
    body = {"name": name, "kind": kind, "estimated_size": len(data), "hours": hours, "tag": tag}
    r = client.post("/api/uploads", json=body)
    assert r.status_code == 200, r.text
    upload_id = r.json()["upload_id"]
    for i in range(0, len(data), CHUNK):
        part = client.put(f"/api/uploads/{upload_id}/parts/{i // CHUNK + 1}", content=data[i : i + CHUNK])
        assert part.status_code == 200, part.text
    r = client.post(f"/api/uploads/{upload_id}/complete")
    assert r.status_code == 200, r.text
    return r.json()
