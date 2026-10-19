import sqlite3
import threading
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path

SCHEMA = """
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS uploads (
    id           TEXT PRIMARY KEY,
    ip           TEXT NOT NULL,
    name         TEXT NOT NULL,
    kind         TEXT NOT NULL,
    items        TEXT,
    hours        INTEGER NOT NULL,
    reserved     INTEGER NOT NULL,
    received     INTEGER NOT NULL DEFAULT 0,
    object_key   TEXT NOT NULL,
    tag          TEXT,
    s3_upload_id TEXT,
    state        TEXT NOT NULL DEFAULT 'open',
    created_at   REAL NOT NULL,
    updated_at   REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS uploads_ip ON uploads (ip);
CREATE TABLE IF NOT EXISTS upload_parts (
    upload_id TEXT NOT NULL,
    n         INTEGER NOT NULL,
    etag      TEXT NOT NULL,
    size      INTEGER NOT NULL,
    PRIMARY KEY (upload_id, n)
);
CREATE TABLE IF NOT EXISTS files (
    code       TEXT PRIMARY KEY,
    object_key TEXT NOT NULL,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL,
    items      TEXT,
    size       INTEGER NOT NULL,
    ip         TEXT NOT NULL,
    tag        TEXT,
    created_at REAL NOT NULL,
    expires_at REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS upload_starts (
    ip TEXT NOT NULL,
    ts REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS upload_starts_ip ON upload_starts (ip, ts);
CREATE TABLE IF NOT EXISTS lookup_failures (
    ip TEXT NOT NULL,
    ts REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS lookup_failures_ip ON lookup_failures (ip, ts);
CREATE TABLE IF NOT EXISTS ip_blocks (
    ip    TEXT PRIMARY KEY,
    until REAL NOT NULL
);
"""


class DB:
    """One SQLite connection shared by all threads, serialized by a lock."""

    def __init__(self, path: str):
        if path != ":memory:":
            Path(path).parent.mkdir(parents=True, exist_ok=True)
        self.conn = sqlite3.connect(path, check_same_thread=False, isolation_level=None)
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.conn.execute("PRAGMA busy_timeout=5000")
        self.conn.executescript(SCHEMA)
        self._migrate()
        self.lock = threading.RLock()

    def _migrate(self) -> None:
        """Bring databases created by older versions up to the current SCHEMA."""
        for table in ("uploads", "files"):
            columns = {r["name"] for r in self.conn.execute(f"PRAGMA table_info({table})")}
            for column in ("tag", "items"):  # items: a bundle's [{name, kind}] as JSON
                if column not in columns:
                    self.conn.execute(f"ALTER TABLE {table} ADD COLUMN {column} TEXT")
        self.conn.execute("CREATE INDEX IF NOT EXISTS files_tag ON files (tag, expires_at)")

    @contextmanager
    def tx(self) -> Iterator[sqlite3.Connection]:
        with self.lock:
            self.conn.execute("BEGIN IMMEDIATE")
            try:
                yield self.conn
            except BaseException:
                self.conn.execute("ROLLBACK")
                raise
            self.conn.execute("COMMIT")
