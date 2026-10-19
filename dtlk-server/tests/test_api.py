import io
import os
import re
import sqlite3
import stat
import zipfile
from datetime import datetime

import pytest
from botocore.exceptions import ClientError

from app.config import MiB
from app.convert import content_disposition
from app.db import DB

from .conftest import CHUNK, make_archive, upload


def test_upload_download_roundtrip(env):
    data = make_archive({"test1/": None, "test1/big.bin": os.urandom(11 * MiB)})
    res = upload(env.public, data)
    assert re.fullmatch(r"\d{6}", res["code"])
    assert res["size"] == len(data)

    info = env.public.get(f"/api/files/{res['code']}").json()
    assert info["name"] == "test1" and info["kind"] == "dir" and info["size"] == len(data)
    expires = datetime.fromisoformat(info["expires_at"]).timestamp()
    assert abs(expires - (env.clock() + 24 * 3600)) < 2

    r = env.public.get(f"/api/files/{res['code']}/download")
    assert r.status_code == 200 and r.content == data

    r = env.public.get(f"/api/files/{res['code']}/download", headers={"Range": "bytes=1000-"})
    assert r.status_code == 206
    assert r.headers["content-range"] == f"bytes 1000-{len(data) - 1}/{len(data)}"
    assert r.content == data[1000:]

    r = env.public.get(f"/api/files/{res['code']}/download", headers={"Range": f"bytes={len(data)}-"})
    assert r.status_code == 416


@pytest.mark.parametrize("hours", [0, 169, -1])
def test_hours_out_of_range(env, hours):
    r = env.public.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": hours})
    assert r.status_code == 400
    assert "168" in r.json()["detail"]


def test_hours_must_be_integer(env):
    r = env.public.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1.5})
    assert r.status_code == 422


def test_custom_hours_and_expiry_cleanup(env):
    res = upload(env.public, make_archive({"a.txt": b"hi"}), name="a.txt", kind="file", hours=3)
    code = res["code"]
    key = env.svc.db.conn.execute("SELECT object_key FROM files WHERE code = ?", (code,)).fetchone()[0]
    assert abs(datetime.fromisoformat(res["expires_at"]).timestamp() - (env.clock() + 3 * 3600)) < 2

    env.clock.advance(3 * 3600 - 10)
    assert env.public.get(f"/api/files/{code}").status_code == 200
    env.clock.advance(20)
    assert env.public.get(f"/api/files/{code}").status_code == 404

    env.svc.cleanup_once()
    assert env.svc.db.conn.execute("SELECT COUNT(*) FROM files").fetchone()[0] == 0
    with pytest.raises(ClientError):
        env.s3.head_object(Bucket=env.cfg.s3_bucket, Key=key)


def test_bad_names_rejected(env):
    for name in ["", ".", "..", "a/b", "x" * 300]:
        r = env.public.post("/api/uploads", json={"name": name, "kind": "dir", "estimated_size": 1, "hours": 1})
        assert r.status_code == 400, name


def test_max_upload_size(env):
    r = env.public.post("/api/uploads", json={"name": "big", "kind": "file", "estimated_size": 1, "hours": 1})
    upload_id = r.json()["upload_id"]
    chunk = b"\0" * CHUNK
    for n in range(1, 5):  # 4 x 5 MiB = 20 MiB = the test limit
        assert env.public.put(f"/api/uploads/{upload_id}/parts/{n}", content=chunk).status_code == 200
    r = env.public.put(f"/api/uploads/{upload_id}/parts/5", content=b"x")
    assert r.status_code == 413
    # retrying an existing part with the same size is still fine
    assert env.public.put(f"/api/uploads/{upload_id}/parts/4", content=chunk).status_code == 200


def test_oversized_part_rejected(env):
    upload_id = env.public.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1}).json()["upload_id"]
    assert env.public.put(f"/api/uploads/{upload_id}/parts/1", content=b"x" * (CHUNK + 1)).status_code == 413


def test_complete_requires_contiguous_parts(env):
    upload_id = env.public.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1}).json()["upload_id"]
    assert env.public.post(f"/api/uploads/{upload_id}/complete").status_code == 400
    env.public.put(f"/api/uploads/{upload_id}/parts/2", content=b"x")
    assert env.public.post(f"/api/uploads/{upload_id}/complete").status_code == 400


def test_storage_cap(env):
    env.admin.put("/admin/settings", json={"storage_cap_bytes": 8 * MiB})
    r = env.public.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 9 * MiB, "hours": 1})
    assert r.status_code == 507

    # reservation grows with the data actually sent and hits the cap too
    upload_id = env.public.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1}).json()["upload_id"]
    assert env.public.put(f"/api/uploads/{upload_id}/parts/1", content=b"\0" * CHUNK).status_code == 200
    assert env.public.put(f"/api/uploads/{upload_id}/parts/2", content=b"\0" * CHUNK).status_code == 507

    # aborting frees the reservation
    assert env.public.delete(f"/api/uploads/{upload_id}").status_code == 204
    r = env.public.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 7 * MiB, "hours": 1})
    assert r.status_code == 200


def test_concurrent_upload_limit(env):
    c = env.client("10.0.0.1")
    ids = [c.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1}).json()["upload_id"] for _ in range(3)]
    r = c.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1})
    assert r.status_code == 429
    # other IPs are unaffected
    assert env.client("10.0.0.2").post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1}).status_code == 200
    c.delete(f"/api/uploads/{ids[0]}")
    assert c.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1}).status_code == 200


def test_hourly_upload_limit(env):
    c = env.client("10.0.0.3")
    for _ in range(30):
        upload_id = c.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1}).json()["upload_id"]
        c.delete(f"/api/uploads/{upload_id}")
        env.clock.advance(1)
    r = c.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1})
    assert r.status_code == 429
    assert int(r.headers["retry-after"]) > 3000
    env.clock.advance(3600)
    assert c.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1}).status_code == 200


def test_lookup_failures_block_ip(env):
    code = upload(env.public, make_archive({"a.txt": b"hi"}), name="a.txt", kind="file")["code"]
    bad = "000000" if code != "000000" else "000001"
    attacker = env.client("10.9.9.9")
    for _ in range(20):
        assert attacker.get(f"/api/files/{bad}").status_code == 404
    r = attacker.get(f"/api/files/{code}")
    assert r.status_code == 429
    assert 1700 < int(r.headers["retry-after"]) <= 1800
    assert attacker.get(f"/{code}").status_code == 429
    # everyone else still gets through
    assert env.client("10.1.1.1").get(f"/api/files/{code}").status_code == 200

    blocks = env.admin.get("/admin/blocks").json()
    assert [b["ip"] for b in blocks["blocked"]] == ["10.9.9.9"]

    env.clock.advance(1801)
    assert attacker.get(f"/api/files/{code}").status_code == 200

    for _ in range(20):
        attacker.get(f"/api/files/{bad}")
    assert attacker.get(f"/api/files/{code}").status_code == 429
    assert env.admin.delete("/admin/blocks/10.9.9.9").status_code == 204
    assert attacker.get(f"/api/files/{code}").status_code == 200


def test_failures_outside_window_do_not_count(env):
    c = env.client("10.2.2.2")
    for _ in range(19):
        c.get("/api/files/123456")
    env.clock.advance(601)
    for _ in range(19):
        assert c.get("/api/files/123456").status_code == 404


def test_stale_upload_is_aborted(env):
    upload_id = env.public.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1}).json()["upload_id"]
    env.public.put(f"/api/uploads/{upload_id}/parts/1", content=b"x")
    env.clock.advance(3601)
    env.svc.cleanup_once()
    assert env.public.put(f"/api/uploads/{upload_id}/parts/2", content=b"x").status_code == 404
    assert env.s3.list_multipart_uploads(Bucket=env.cfg.s3_bucket).get("Uploads", []) == []


def test_reconcile_removes_orphans(env):
    code = upload(env.public, make_archive({"a.txt": b"hi"}), name="a.txt", kind="file")["code"]
    env.s3.put_object(Bucket=env.cfg.s3_bucket, Key="objects/orphan", Body=b"x")
    env.clock.advance(7200)
    env.svc.reconcile()
    keys = [o["Key"] for o in env.s3.list_objects_v2(Bucket=env.cfg.s3_bucket).get("Contents", [])]
    assert "objects/orphan" not in keys and len(keys) == 1
    assert env.public.get(f"/api/files/{code}").status_code == 200


def test_browser_download_single_file(env):
    content = os.urandom(3 * MiB)
    code = upload(env.public, make_archive({"report final.pdf": content}), name="report final.pdf", kind="file")["code"]
    r = env.public.get(f"/{code}")
    assert r.status_code == 200
    assert r.content == content
    assert r.headers["content-length"] == str(len(content))
    assert r.headers["content-type"] == "application/pdf"
    assert r.headers["content-disposition"].startswith('attachment; filename="report final.pdf";')


def test_content_disposition_non_ascii():
    header = content_disposition('보고서 "최종".pdf')
    assert header.endswith("filename*=UTF-8''%EB%B3%B4%EA%B3%A0%EC%84%9C%20%22%EC%B5%9C%EC%A2%85%22.pdf")
    # curl -OJ / wget read filename=, which carries the raw UTF-8 bytes on the wire
    assert 'filename="보고서 _최종_.pdf"'.encode() in header.encode("latin-1")


def test_browser_download_folder_as_zip(env):
    big = os.urandom(2 * MiB)
    data = make_archive(
        {
            "test1/": None,
            "test1/hello/": None,
            "test1/hello/world/": None,
            "test1/hello/world/text1.txt": b"hello",
            "test1/empty/": None,
            "test1/한글.bin": big,
        },
        links={"test1/link": "hello/world/text1.txt"},
    )
    code = upload(env.public, data)["code"]
    r = env.public.get(f"/{code}")
    assert r.status_code == 200
    assert r.headers["content-type"] == "application/zip"
    assert 'filename="test1.zip"' in r.headers["content-disposition"]

    zf = zipfile.ZipFile(io.BytesIO(r.content))
    assert zf.testzip() is None
    names = set(zf.namelist())
    assert {"test1/", "test1/hello/world/text1.txt", "test1/empty/", "test1/한글.bin", "test1/link"} <= names
    assert zf.read("test1/hello/world/text1.txt") == b"hello"
    assert zf.read("test1/한글.bin") == big
    link = zf.getinfo("test1/link")
    assert stat.S_ISLNK(link.external_attr >> 16) and zf.read(link) == b"hello/world/text1.txt"


def test_unknown_paths_are_404_and_not_counted(env):
    c = env.client("10.3.3.3")
    for _ in range(25):
        assert c.get("/12345").status_code == 404
        assert c.get("/favicon.ico").status_code == 404
    assert env.svc.db.conn.execute("SELECT COUNT(*) FROM lookup_failures").fetchone()[0] == 0


def test_install_script_and_binaries(env):
    r = env.public.get("/install.sh")
    assert r.status_code == 200
    assert 'ORIGIN="https://dtlk.example.test"' in r.text
    assert env.public.get("/dl/dtlk-linux-amd64").content == b"binary"
    assert env.public.get("/dl/dtlk-linux-arm64").status_code == 404  # not built in this test
    assert env.public.get("/dl/..%2Fconftest.py").status_code == 404


def test_admin_overview_settings_and_delete(env):
    code = upload(env.public, make_archive({"a.txt": b"hi"}), name="a.txt", kind="file")["code"]
    env.public.post("/api/uploads", json={"name": "pending", "kind": "dir", "estimated_size": 4096, "hours": 1})

    ov = env.admin.get("/admin/overview").json()
    assert [f["code"] for f in ov["files"]] == [code]
    assert ov["files"][0]["tag"] is None
    assert [u["name"] for u in ov["uploads"]] == ["pending"]
    assert ov["reserved_bytes"] == 4096 and ov["used_bytes"] > 0
    assert ov["settings"]["storage_cap_bytes"] == 32 * 1024**3
    assert ov["disk"]["total"] > 0

    s = env.admin.put("/admin/settings", json={"upload_hourly_max": 5, "lookup_block_sec": 60}).json()
    assert s["upload_hourly_max"] == 5 and s["lookup_block_sec"] == 60 and s["lookup_fail_max"] == 20
    assert env.admin.put("/admin/settings", json={"upload_hourly_max": 0}).status_code == 422

    assert env.admin.delete(f"/admin/files/{code}").status_code == 204
    assert env.public.get(f"/api/files/{code}").status_code == 404
    assert env.admin.delete(f"/admin/files/{code}").status_code == 404

    upload_id = ov["uploads"][0]["id"]
    assert env.admin.delete(f"/admin/uploads/{upload_id}").status_code == 204
    assert env.admin.get("/admin/overview").json()["uploads"] == []


HTML = {"Accept": "text/html,application/xhtml+xml,*/*;q=0.8"}


def test_file_list_by_tag(env):
    a = upload(env.public, make_archive({"a.txt": b"a"}), name="a.txt", kind="file", hours=1, tag="MyProj")["code"]
    env.clock.advance(1)
    b = upload(env.public, make_archive({"photos/": None}), name="photos", tag="myproj")["code"]
    other = upload(env.public, make_archive({"x.txt": b"x"}), name="x.txt", kind="file", tag="other")["code"]
    plain = upload(env.public, make_archive({"y.txt": b"y"}), name="y.txt", kind="file")["code"]

    r = env.public.get("/files/MYPROJ", headers=HTML)
    assert r.status_code == 200 and r.headers["content-type"].startswith("text/html")
    assert r.headers["cache-control"] == "no-store"
    page = r.text
    assert "<style" not in page and "<script" not in page and "style=" not in page  # plain HTML for old browsers
    assert "a.txt" in page and "photos/" in page
    assert f'href="/{a}"' in page and f'href="/{b}"' in page
    assert other not in page and plain not in page and "x.txt" not in page
    assert page.index("photos/") < page.index("a.txt")  # newest first

    r = env.public.get("/files/myproj")
    assert r.status_code == 200 and r.headers["content-type"].startswith("text/plain")
    lines = r.text.splitlines()
    assert [line.split()[0] for line in lines] == [b, a]
    assert lines[0].endswith("  photos/") and lines[1].endswith("  a.txt")

    # a.txt expires after 1 hour and drops out; once everything is gone the list is a 404
    env.clock.advance(3600)
    assert [line.split()[0] for line in env.public.get("/files/myproj").text.splitlines()] == [b]
    env.clock.advance(24 * 3600)
    r = env.public.get("/files/myproj", headers=HTML)
    assert r.status_code == 404 and r.headers["content-type"].startswith("text/html")
    assert env.public.get("/files/myproj").text == "List not found or all its files have expired\n"


def test_file_list_escapes_names(env):
    upload(env.public, make_archive({"<script>.txt": b"x"}), name="<script>.txt", kind="file", tag="xss")
    page = env.public.get("/files/xss", headers=HTML).text
    assert "<script>.txt" not in page and "&lt;script&gt;.txt" in page


@pytest.mark.parametrize("tag", ["", "a b", "x" * 65, "한글", "a.b", "a/b"])
def test_bad_tags_rejected(env, tag):
    r = env.public.post("/api/uploads", json={"name": "a", "kind": "file", "estimated_size": 1, "hours": 1, "tag": tag})
    assert r.status_code == 400


def test_unknown_tags_count_toward_block(env):
    code = upload(env.public, make_archive({"a.txt": b"hi"}), name="a.txt", kind="file", tag="real")["code"]
    c = env.client("10.4.4.4")
    for _ in range(25):  # malformed names never reach the lookup
        assert c.get("/files/a.b").status_code == 404
        assert c.get("/files/" + "x" * 65).status_code == 404
    assert env.svc.db.conn.execute("SELECT COUNT(*) FROM lookup_failures").fetchone()[0] == 0

    for _ in range(20):
        assert c.get("/files/nope").status_code == 404
    r = c.get("/files/real")
    assert r.status_code == 429 and 1700 < int(r.headers["retry-after"]) <= 1800
    assert c.get(f"/{code}").status_code == 429
    assert env.client("10.5.5.5").get("/files/real").status_code == 200


def test_old_database_is_migrated(tmp_path):
    path = str(tmp_path / "old.db")
    old = sqlite3.connect(path)
    old.executescript(
        "CREATE TABLE uploads (id TEXT PRIMARY KEY, ip TEXT NOT NULL, name TEXT NOT NULL, kind TEXT NOT NULL,"
        " hours INTEGER NOT NULL, reserved INTEGER NOT NULL, received INTEGER NOT NULL DEFAULT 0,"
        " object_key TEXT NOT NULL, s3_upload_id TEXT, state TEXT NOT NULL DEFAULT 'open',"
        " created_at REAL NOT NULL, updated_at REAL NOT NULL);"
        "CREATE TABLE files (code TEXT PRIMARY KEY, object_key TEXT NOT NULL, name TEXT NOT NULL,"
        " kind TEXT NOT NULL, size INTEGER NOT NULL, ip TEXT NOT NULL, created_at REAL NOT NULL,"
        " expires_at REAL NOT NULL);"
        "INSERT INTO files VALUES ('123456', 'objects/x', 'a.txt', 'file', 1, '1.2.3.4', 0, 9e9);"
    )
    old.close()

    db = DB(path)
    for table in ("uploads", "files"):
        assert "tag" in {r["name"] for r in db.conn.execute(f"PRAGMA table_info({table})")}
    assert db.conn.execute("SELECT name FROM sqlite_master WHERE name = 'files_tag'").fetchone()
    assert db.conn.execute("SELECT name, tag FROM files").fetchone()[:] == ("a.txt", None)
    DB(path)  # running the migration again is a no-op
