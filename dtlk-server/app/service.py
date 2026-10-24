import json
import logging
import math
import re
import secrets
import shutil
import sqlite3
import time
import uuid
from collections.abc import Callable
from datetime import UTC, datetime
from typing import Any

from .config import DEFAULT_SETTINGS, Config
from .db import DB
from .storage import Storage

log = logging.getLogger("dtlk")

KINDS = ("file", "dir", "bundle")  # a bundle is a folder of several uploaded items, unpacked on download
MAX_PARTS = 10000
MAX_ITEMS = 10000
TAG_RE = re.compile(r"[a-z0-9_-]{1,64}")


class ApiError(Exception):
    def __init__(self, status: int, detail: str, retry_after: float | None = None):
        super().__init__(detail)
        self.status = status
        self.detail = detail
        self.retry_after = None if retry_after is None else max(1, math.ceil(retry_after))


def human(n: int) -> str:
    v = float(n)
    for unit in ("B", "KB", "MB", "GB"):
        if v < 1024 or unit == "GB":
            return f"{v:.0f} {unit}" if unit == "B" else f"{v:.1f} {unit}"
        v /= 1024
    return f"{n} B"


def iso(ts: float) -> str:
    return datetime.fromtimestamp(ts, UTC).isoformat(timespec="seconds")


def valid_name(name: str) -> bool:
    return (
        0 < len(name.encode()) <= 255
        and name not in (".", "..")
        and "/" not in name
        and "\x00" not in name
    )


def check_items(items: list[dict[str, str]] | None) -> str:
    """A bundle's items (its top-level entries, [{name, kind}]) as stored JSON."""
    if not items or not 2 <= len(items) <= MAX_ITEMS:
        raise ApiError(400, f"A bundle needs 2-{MAX_ITEMS} items")
    names = set()
    for it in items:
        if not valid_name(it["name"]) or it["name"] in names:
            raise ApiError(400, "Invalid or duplicate item name")
        if it["kind"] not in ("file", "dir"):
            raise ApiError(400, "Item kind must be 'file' or 'dir'")
        names.add(it["name"])
    return json.dumps([{"name": it["name"], "kind": it["kind"]} for it in items], ensure_ascii=False)


class Service:
    """All upload/download/limit logic. Methods are synchronous and thread-safe."""

    def __init__(self, cfg: Config, db: DB, storage: Storage, clock: Callable[[], float] = time.time):
        self.cfg = cfg
        self.db = db
        self.storage = storage
        self.clock = clock
        self._inflight: dict[str, dict[int, int]] = {}  # upload id -> {part number: size}

    # ---- settings -------------------------------------------------------

    def settings(self) -> dict[str, int]:
        with self.db.lock:
            rows = self.db.conn.execute("SELECT key, value FROM settings").fetchall()
        stored = {r["key"]: r["value"] for r in rows if r["key"] in DEFAULT_SETTINGS}
        return {**DEFAULT_SETTINGS, **stored}

    def update_settings(self, changes: dict[str, int]) -> dict[str, int]:
        with self.db.tx() as c:
            for key, value in changes.items():
                if key not in DEFAULT_SETTINGS or not isinstance(value, int) or value < 1:
                    raise ApiError(400, f"잘못된 설정값: {key}={value}")
                c.execute("INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)", (key, value))
        return self.settings()

    # ---- accounting -----------------------------------------------------

    def _used(self, c: sqlite3.Connection, now: float) -> tuple[int, int]:
        files = c.execute("SELECT COALESCE(SUM(size), 0) FROM files WHERE expires_at > ?", (now,)).fetchone()[0]
        reserved = c.execute("SELECT COALESCE(SUM(MAX(reserved, received)), 0) FROM uploads").fetchone()[0]
        return files, reserved

    def _check_cap(self, c: sqlite3.Connection, now: float, extra: int, cap: int) -> None:
        files, reserved = self._used(c, now)
        used = files + reserved
        if used + extra > cap:
            raise ApiError(507, f"Server storage full ({human(used)} / {human(cap)} used); please try again later")

    # ---- uploads --------------------------------------------------------

    def create_upload(
        self,
        ip: str,
        name: str,
        kind: str,
        estimated_size: int,
        hours: int,
        tag: str | None = None,
        items: list[dict[str, str]] | None = None,
    ) -> dict[str, Any]:
        if not valid_name(name):
            raise ApiError(400, "Invalid file name")
        if kind not in KINDS:
            raise ApiError(400, "kind must be 'file', 'dir' or 'bundle'")
        if kind == "bundle":
            items_json: str | None = check_items(items)
        elif items is not None:
            raise ApiError(400, "Only a bundle has items")
        else:
            items_json = None
        if tag is not None:
            tag = tag.lower()
            if not TAG_RE.fullmatch(tag):
                raise ApiError(400, "Invalid list name: use 1-64 letters, digits, '-' or '_'")
        if not 1 <= hours <= self.cfg.max_hours:
            raise ApiError(400, f"Retention hours must be an integer between 1 and {self.cfg.max_hours}")
        if estimated_size < 0:
            raise ApiError(400, "Invalid estimated_size")

        reserve = min(estimated_size, self.cfg.max_upload_bytes)
        s = self.settings()
        now = self.clock()
        upload_id = secrets.token_urlsafe(24)
        key = f"objects/{uuid.uuid4().hex}"

        error: ApiError | None = None
        with self.db.tx() as c:
            active = c.execute("SELECT COUNT(*) FROM uploads WHERE ip = ?", (ip,)).fetchone()[0]
            recent = c.execute(
                "SELECT COUNT(*), MIN(ts) FROM upload_starts WHERE ip = ? AND ts > ?", (ip, now - 3600)
            ).fetchone()
            if active >= s["upload_concurrent_max"]:
                error = ApiError(429, f"Too many concurrent uploads (max {s['upload_concurrent_max']}); wait for active uploads to finish")
            elif recent[0] >= s["upload_hourly_max"]:
                error = ApiError(429, f"Hourly upload limit reached (max {s['upload_hourly_max']} per hour)", retry_after=recent[1] + 3600 - now)
            else:
                self._check_cap(c, now, reserve, s["storage_cap_bytes"])
                c.execute(
                    "INSERT INTO uploads (id, ip, name, kind, items, hours, reserved, object_key, tag, created_at, updated_at)"
                    " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (upload_id, ip, name, kind, items_json, hours, reserve, key, tag, now, now),
                )
                c.execute("INSERT INTO upload_starts (ip, ts) VALUES (?, ?)", (ip, now))
        if error:
            raise error

        try:
            s3_id = self.storage.create_multipart(key)
        except Exception:
            log.exception("create_multipart failed")
            with self.db.tx() as c:
                c.execute("DELETE FROM uploads WHERE id = ?", (upload_id,))
            raise ApiError(503, "Storage unavailable; please try again shortly")
        with self.db.tx() as c:
            c.execute("UPDATE uploads SET s3_upload_id = ? WHERE id = ?", (s3_id, upload_id))
        log.info("upload %s started by %s: %s (%s, %dh, list %s)", upload_id[:8], ip, name, kind, hours, tag or "-")
        return {"upload_id": upload_id, "chunk_size": self.cfg.chunk_size, "max_size": self.cfg.max_upload_bytes}

    def _open_upload(self, c: sqlite3.Connection, upload_id: str) -> sqlite3.Row:
        row = c.execute("SELECT * FROM uploads WHERE id = ?", (upload_id,)).fetchone()
        if row is None:
            raise ApiError(404, "Upload not found (canceled or expired)")
        if row["state"] != "open" or row["s3_upload_id"] is None:
            raise ApiError(409, "Upload is not accepting parts")
        return row

    def put_part(self, upload_id: str, n: int, data: bytes) -> None:
        size = len(data)
        if not 1 <= n <= MAX_PARTS:
            raise ApiError(400, "Invalid part number")
        if size == 0:
            raise ApiError(400, "Empty part is not allowed")
        if size > self.cfg.chunk_size:
            raise ApiError(413, "Part is too large")

        s = self.settings()
        now = self.clock()
        with self.db.tx() as c:
            row = self._open_upload(c, upload_id)
            if n in self._inflight.get(upload_id, {}):
                raise ApiError(409, "Part is already being uploaded")
            stored = c.execute(
                "SELECT COALESCE(SUM(size), 0) FROM upload_parts WHERE upload_id = ? AND n != ?", (upload_id, n)
            ).fetchone()[0]
            total = stored + sum(self._inflight.get(upload_id, {}).values()) + size
            if total > self.cfg.max_upload_bytes:
                raise ApiError(413, f"Compressed size exceeds the {human(self.cfg.max_upload_bytes)} limit")
            if total > row["reserved"]:
                self._check_cap(c, now, total - row["reserved"], s["storage_cap_bytes"])
                c.execute("UPDATE uploads SET reserved = ? WHERE id = ?", (total, upload_id))
            c.execute("UPDATE uploads SET updated_at = ? WHERE id = ?", (now, upload_id))
            inflight = self._inflight.setdefault(upload_id, {})
            inflight[n] = size

        try:
            etag = self.storage.upload_part(row["object_key"], row["s3_upload_id"], n, data)
        except Exception:
            log.exception("upload_part failed")
            raise ApiError(503, "Failed to write to storage; please try again shortly")
        finally:
            with self.db.lock:
                inflight.pop(n, None)
                if not inflight:
                    self._inflight.pop(upload_id, None)

        with self.db.tx() as c:
            if c.execute("SELECT 1 FROM uploads WHERE id = ?", (upload_id,)).fetchone():
                c.execute(
                    "INSERT OR REPLACE INTO upload_parts (upload_id, n, etag, size) VALUES (?, ?, ?, ?)",
                    (upload_id, n, etag, size),
                )
                c.execute(
                    "UPDATE uploads SET received = (SELECT SUM(size) FROM upload_parts WHERE upload_id = ?),"
                    " updated_at = ? WHERE id = ?",
                    (upload_id, self.clock(), upload_id),
                )

    def complete_upload(self, upload_id: str) -> dict[str, Any]:
        with self.db.tx() as c:
            row = self._open_upload(c, upload_id)
            if self._inflight.get(upload_id):
                raise ApiError(409, "Parts are still in flight")
            parts = c.execute(
                "SELECT n, etag, size FROM upload_parts WHERE upload_id = ? ORDER BY n", (upload_id,)
            ).fetchall()
            if not parts or [p["n"] for p in parts] != list(range(1, len(parts) + 1)):
                raise ApiError(400, "Missing upload parts")
            c.execute("UPDATE uploads SET state = 'completing' WHERE id = ?", (upload_id,))

        try:
            self.storage.complete_multipart(row["object_key"], row["s3_upload_id"], [(p["n"], p["etag"]) for p in parts])
        except Exception:
            log.exception("complete_multipart failed")
            with self.db.tx() as c:
                c.execute("UPDATE uploads SET state = 'open', updated_at = ? WHERE id = ?", (self.clock(), upload_id))
            raise ApiError(503, "Failed to finalize upload in storage; please try again shortly")

        size = sum(p["size"] for p in parts)
        now = self.clock()
        expires_at = now + row["hours"] * 3600
        with self.db.tx() as c:
            while True:
                code = f"{secrets.randbelow(1_000_000):06d}"
                if not c.execute("SELECT 1 FROM files WHERE code = ?", (code,)).fetchone():
                    break
            c.execute(
                "INSERT INTO files (code, object_key, name, kind, items, size, ip, tag, created_at, expires_at)"
                " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (code, row["object_key"], row["name"], row["kind"], row["items"], size, row["ip"], row["tag"], now, expires_at),
            )
            c.execute("DELETE FROM upload_parts WHERE upload_id = ?", (upload_id,))
            c.execute("DELETE FROM uploads WHERE id = ?", (upload_id,))
        log.info("upload %s complete: code %s, %d bytes", upload_id[:8], code, size)
        return {"code": code, "expires_at": iso(expires_at), "size": size}

    def abort_upload(self, upload_id: str) -> bool:
        with self.db.tx() as c:
            row = c.execute("SELECT * FROM uploads WHERE id = ?", (upload_id,)).fetchone()
            if row is None:
                return False
            c.execute("DELETE FROM upload_parts WHERE upload_id = ?", (upload_id,))
            c.execute("DELETE FROM uploads WHERE id = ?", (upload_id,))
        if row["s3_upload_id"]:
            self.storage.abort_multipart(row["object_key"], row["s3_upload_id"])
        log.info("upload %s aborted", upload_id[:8])
        return True

    # ---- downloads ------------------------------------------------------

    def lookup(self, ip: str, code: str) -> sqlite3.Row:
        """Find an active file by code, counting misses toward an IP block."""
        return self._guarded(
            ip,
            lambda c, now: c.execute("SELECT * FROM files WHERE code = ? AND expires_at > ?", (code, now)).fetchone(),
            "Code not found or expired",
        )

    def list_tag(self, ip: str, tag: str) -> list[sqlite3.Row]:
        """Active files uploaded with `-n tag`, newest first. Misses count like code lookups."""
        return self._guarded(
            ip,
            lambda c, now: c.execute(
                "SELECT code, name, kind, size, expires_at FROM files WHERE tag = ? AND expires_at > ? ORDER BY created_at DESC",
                (tag, now),
            ).fetchall(),
            "List not found or all its files have expired",
        )

    def _guarded[T](self, ip: str, fetch: Callable[[sqlite3.Connection, float], T], not_found: str) -> T:
        """Run fetch unless ip is blocked; an empty result is a failed lookup that may block ip."""
        s = self.settings()
        now = self.clock()
        error: ApiError
        with self.db.tx() as c:
            block = c.execute("SELECT until FROM ip_blocks WHERE ip = ? AND until > ?", (ip, now)).fetchone()
            if block:
                error = ApiError(429, "Too many failed lookups; temporarily blocked", retry_after=block["until"] - now)
            else:
                found = fetch(c, now)
                if found:
                    return found
                c.execute("INSERT INTO lookup_failures (ip, ts) VALUES (?, ?)", (ip, now))
                fails = c.execute(
                    "SELECT COUNT(*) FROM lookup_failures WHERE ip = ? AND ts > ?",
                    (ip, now - s["lookup_fail_window_sec"]),
                ).fetchone()[0]
                if fails >= s["lookup_fail_max"]:
                    c.execute("INSERT OR REPLACE INTO ip_blocks (ip, until) VALUES (?, ?)", (ip, now + s["lookup_block_sec"]))
                    c.execute("DELETE FROM lookup_failures WHERE ip = ?", (ip,))
                    log.warning("blocked %s for %ds after %d failed lookups", ip, s["lookup_block_sec"], fails)
                error = ApiError(404, not_found)
        raise error

    def delete_file(self, code: str) -> bool:
        with self.db.tx() as c:
            row = c.execute("SELECT object_key FROM files WHERE code = ?", (code,)).fetchone()
            if row is None:
                return False
            c.execute("DELETE FROM files WHERE code = ?", (code,))
        self._delete_object(row["object_key"])
        log.info("file %s deleted", code)
        return True

    def _delete_object(self, key: str) -> None:
        try:
            self.storage.delete(key)
        except Exception:
            log.exception("delete %s failed (reconcile will retry)", key)

    # ---- maintenance ----------------------------------------------------

    def cleanup_once(self) -> None:
        s = self.settings()
        now = self.clock()
        with self.db.tx() as c:
            expired = c.execute("SELECT code, object_key FROM files WHERE expires_at <= ?", (now,)).fetchall()
            c.execute("DELETE FROM files WHERE expires_at <= ?", (now,))
            stale = [
                r
                for r in c.execute(
                    "SELECT id, object_key, s3_upload_id FROM uploads WHERE updated_at < ?",
                    (now - self.cfg.stale_upload_sec,),
                ).fetchall()
                if r["id"] not in self._inflight
            ]
            for r in stale:
                c.execute("DELETE FROM upload_parts WHERE upload_id = ?", (r["id"],))
                c.execute("DELETE FROM uploads WHERE id = ?", (r["id"],))
            c.execute("DELETE FROM lookup_failures WHERE ts <= ?", (now - s["lookup_fail_window_sec"],))
            c.execute("DELETE FROM upload_starts WHERE ts <= ?", (now - 3600,))
            c.execute("DELETE FROM ip_blocks WHERE until <= ?", (now,))
        for r in expired:
            self._delete_object(r["object_key"])
        for r in stale:
            if r["s3_upload_id"]:
                self.storage.abort_multipart(r["object_key"], r["s3_upload_id"])
        if expired or stale:
            log.info("cleanup: %d expired files, %d stale uploads", len(expired), len(stale))

    def reconcile(self) -> None:
        """Remove objects and multipart uploads the database no longer knows about."""
        cutoff = self.clock() - 3600
        with self.db.lock:
            keys = {r[0] for r in self.db.conn.execute("SELECT object_key FROM files UNION SELECT object_key FROM uploads")}
            upload_ids = {r[0] for r in self.db.conn.execute("SELECT s3_upload_id FROM uploads WHERE s3_upload_id IS NOT NULL")}
        removed = 0
        for key, modified in self.storage.list_objects("objects/"):
            if key not in keys and modified.timestamp() < cutoff:
                self._delete_object(key)
                removed += 1
        for key, up_id, initiated in self.storage.list_multipart_uploads("objects/"):
            if up_id not in upload_ids and initiated.timestamp() < cutoff:
                self.storage.abort_multipart(key, up_id)
                removed += 1
        if removed:
            log.info("reconcile: removed %d orphans", removed)

    # ---- admin views ----------------------------------------------------

    def overview(self) -> dict[str, Any]:
        now = self.clock()
        with self.db.lock:
            c = self.db.conn
            files = c.execute(
                "SELECT code, name, kind, size, ip, tag, created_at, expires_at FROM files"
                " WHERE expires_at > ? ORDER BY created_at DESC",
                (now,),
            ).fetchall()
            uploads = c.execute(
                "SELECT id, name, kind, ip, hours, reserved, received, state, created_at, updated_at"
                " FROM uploads ORDER BY created_at DESC"
            ).fetchall()
            used, reserved = self._used(c, now)
        try:
            du = shutil.disk_usage(self.cfg.storage_path)
            disk = {"total": du.total, "used": du.used, "free": du.free}
        except OSError:
            disk = None
        return {
            "now": now,
            "used_bytes": used,
            "reserved_bytes": reserved,
            "settings": self.settings(),
            "disk": disk,
            "files": [dict(r) for r in files],
            "uploads": [dict(r) for r in uploads],
        }

    def blocks(self) -> dict[str, Any]:
        s = self.settings()
        now = self.clock()
        with self.db.lock:
            c = self.db.conn
            blocked = c.execute("SELECT ip, until FROM ip_blocks WHERE until > ? ORDER BY until DESC", (now,)).fetchall()
            failures = c.execute(
                "SELECT ip, COUNT(*) AS count, MAX(ts) AS last FROM lookup_failures WHERE ts > ?"
                " GROUP BY ip ORDER BY count DESC",
                (now - s["lookup_fail_window_sec"],),
            ).fetchall()
        return {"now": now, "blocked": [dict(r) for r in blocked], "failures": [dict(r) for r in failures]}

    def unblock(self, ip: str) -> None:
        with self.db.tx() as c:
            c.execute("DELETE FROM ip_blocks WHERE ip = ?", (ip,))
            c.execute("DELETE FROM lookup_failures WHERE ip = ?", (ip,))
        log.info("unblocked %s", ip)
