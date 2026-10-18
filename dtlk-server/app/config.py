import os
from dataclasses import dataclass
from pathlib import Path

MiB = 1024**2
GiB = 1024**3

# Limits the admin dashboard can change at runtime (stored in the settings table).
DEFAULT_SETTINGS: dict[str, int] = {
    "storage_cap_bytes": 32 * GiB,
    "lookup_fail_max": 20,
    "lookup_fail_window_sec": 10 * 60,
    "lookup_block_sec": 30 * 60,
    "upload_concurrent_max": 3,
    "upload_hourly_max": 30,
}


@dataclass(frozen=True)
class Config:
    s3_endpoint: str = "http://rustfs:9000"
    s3_access_key: str = ""
    s3_secret_key: str = ""
    s3_bucket: str = "dtlk"
    s3_region: str = "us-east-1"
    db_path: str = "/data/dtlk.db"
    public_url: str = "https://dtlk.ga111o.com"
    static_dir: Path = Path(__file__).resolve().parent.parent / "static"
    storage_path: str = "/rustfs-data"
    max_upload_bytes: int = 10 * GiB
    chunk_size: int = 16 * MiB
    default_hours: int = 24
    max_hours: int = 168
    stale_upload_sec: int = 3600
    cleanup_interval_sec: int = 60
    reconcile_interval_sec: int = 600

    @classmethod
    def from_env(cls) -> "Config":
        env = os.environ.get
        return cls(
            s3_endpoint=env("DTLK_S3_ENDPOINT", cls.s3_endpoint),
            s3_access_key=env("RUSTFS_ACCESS_KEY", ""),
            s3_secret_key=env("RUSTFS_SECRET_KEY", ""),
            s3_bucket=env("DTLK_S3_BUCKET", cls.s3_bucket),
            db_path=env("DTLK_DB_PATH", cls.db_path),
            public_url=env("DTLK_PUBLIC_URL", cls.public_url).rstrip("/"),
            storage_path=env("DTLK_STORAGE_PATH", cls.storage_path),
        )
