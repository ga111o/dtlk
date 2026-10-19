import base64
import io
import json
import os
import re
import shutil
import subprocess
import tarfile
import zipfile
from datetime import datetime
from pathlib import Path

import pytest
from compression import zstd

from app.config import MiB
from app.webupload import _basename, _uniq

from .conftest import CHUNK, upload

SERVER = Path(__file__).resolve().parent.parent
# dtlk-server/zstd-wasm/build.sh locally, the Dockerfile's test stage in CI
WASM = next((p for p in (SERVER / "build" / "zstd.wasm", SERVER / "static" / "zstd.wasm") if p.is_file()), None)
needs_node = pytest.mark.skipif(shutil.which("node") is None, reason="node is not installed")
COMPRESSION = [
    pytest.param(None, id="raw"),
    pytest.param(WASM, id="zstd", marks=pytest.mark.skipif(WASM is None, reason="run dtlk-server/zstd-wasm/build.sh")),
]


def test_upload_page(env):
    r = env.public.get("/")
    assert r.status_code == 200 and r.headers["content-type"].startswith("text/html")
    page = r.text
    for part in ('action="/upload"', 'enctype="multipart/form-data"', 'name="tag"', 'name="hours"', 'name="file"',
                 'value="24"', 'max="168"', "https://dtlk.example.test/install.sh"):
        assert part in page
    assert "<style" not in page and "style=" not in page  # no CSS, only the one script
    assert page.count("<script") == 1 and page.count("</script>") == 1


def test_zstd_wasm(env):
    assert env.public.get("/zstd.wasm").status_code == 404  # the JS then uploads uncompressed
    (env.cfg.static_dir / "zstd.wasm").write_bytes(b"\0asm\1\0\0\0")
    r = env.public.get("/zstd.wasm")
    assert r.status_code == 200 and r.content == b"\0asm\1\0\0\0"
    assert r.headers["content-type"] == "application/wasm" and r.headers["cache-control"] == "no-cache"


# ---- the form, for browsers without JavaScript ----


def post_form(client, files, tag="", hours="24"):
    return client.post("/upload", data={"tag": tag, "hours": hours}, files=[("file", f) for f in files])


def code_of(r) -> str:
    assert r.status_code == 200, r.text
    assert r.headers["content-type"].startswith("text/html") and "<script" not in r.text
    return re.search(r"<b>(\d{6})</b>", r.text)[1]


def test_form_single_file(env):
    content = os.urandom(MiB)
    code = code_of(post_form(env.public, [("report.pdf", content)]))
    info = env.public.get(f"/api/files/{code}").json()
    assert info["name"] == "report.pdf" and info["kind"] == "file"
    assert env.public.get(f"/{code}").content == content


def test_form_files_are_bundled(env):
    r = post_form(env.public, [("a.txt", b"one"), ("a.txt", b"two"), ("b", b"three")])
    code = code_of(r)
    info = env.public.get(f"/api/files/{code}").json()
    assert info["kind"] == "bundle" and re.fullmatch(r"dtlk-\d{6}-\d{6}", info["name"])
    assert info["items"] == [{"name": n, "kind": "file"} for n in ("a.txt", "a (2).txt", "b")]
    assert f"{info['name']}/" in r.text
    assert env.public.get(f"/{code}/a (2).txt").content == b"two"
    tf = tarfile.open(fileobj=io.BytesIO(env.public.get(f"/{code}").content))
    assert {m.name: tf.extractfile(m).read() for m in tf} == {"a.txt": b"one", "a (2).txt": b"two", "b": b"three"}


def test_form_tag_and_hours(env):
    r = post_form(env.public, [("a.txt", b"hi")], tag="MyTag", hours="3")
    code = code_of(r)
    assert 'href="/files/mytag"' in r.text
    assert env.public.get("/files/mytag").text.startswith(code)
    expires = datetime.fromisoformat(env.public.get(f"/api/files/{code}").json()["expires_at"]).timestamp()
    assert abs(expires - (env.clock() + 3 * 3600)) < 2


@pytest.mark.parametrize(
    "files,tag,hours,status",
    [
        ([], "", "24", 400),
        ([("a.txt", b"x")], "", "", 400),
        ([("a.txt", b"x")], "", "1.5", 400),
        ([("a.txt", b"x")], "", "0", 400),
        ([("a.txt", b"x")], "", "169", 400),
        ([("a.txt", b"x")], "a b", "24", 400),
        ([("..", b"x")], "", "24", 400),
        ([("big.bin", b"\0" * (20 * MiB + 1))], "", "24", 413),
    ],
)
def test_form_errors(env, files, tag, hours, status):
    r = post_form(env.public, files, tag, hours)
    assert r.status_code == status
    assert r.headers["content-type"].startswith("text/html") and "<h1>dtlk</h1>" in r.text
    assert env.svc.db.conn.execute("SELECT COUNT(*) FROM uploads").fetchone()[0] == 0


def test_form_names():
    assert _basename(r"C:\Users\me\report.pdf") == "report.pdf"
    assert _basename("dir/a.txt") == "a.txt"
    used: set[str] = set()
    assert [_uniq(n, used) for n in ["a.txt", "a.txt", "a.txt", "b", "b", ".bashrc", ".bashrc"]] == [
        "a.txt", "a (2).txt", "a (3).txt", "b", "b (2)", ".bashrc", ".bashrc (2)"
    ]


# ---- upload.js's archives, run under node ----


def run_js(spec: dict) -> dict:
    proc = subprocess.run(
        ["node", str(SERVER / "tests" / "archive_harness.js")],
        input=json.dumps(spec).encode(),
        capture_output=True,
        timeout=120,
    )
    assert proc.returncode == 0, proc.stderr.decode()
    return json.loads(proc.stdout)


def pack(wasm: Path | None, roots: list[str], items: list[dict], now: float) -> tuple[dict, bytes]:
    spec_items = [
        {"path": it["path"], "dir": True}
        if it.get("dir")
        else {"path": it["path"], "data": base64.b64encode(it["data"]).decode(), "mtime": 1_700_000_000}
        for it in items
    ]
    out = run_js({"wasm": wasm and str(wasm), "part_size": CHUNK, "now": now, "roots": roots, "items": spec_items})
    parts = [base64.b64decode(p) for p in out["parts"]]
    assert all(len(p) == CHUNK for p in parts[:-1]) and 0 < len(parts[-1]) <= CHUNK
    data = b"".join(parts)
    assert len(data) <= out["estimate"]
    return out, data


@needs_node
@pytest.mark.parametrize("wasm", COMPRESSION)
def test_js_folder(env, wasm):
    text = b"".join(b"line %d of a log that compresses well\n" % i for i in range(80_000))
    rand = os.urandom(6 * MiB + 12345)
    long_name = "아주 긴 한글 파일 이름 " * 6 + ".txt"  # over 100 bytes: needs a PAX path
    items = [
        {"path": "test1", "dir": True},
        {"path": "test1/text.log", "data": text},
        {"path": "test1/sub", "dir": True},
        {"path": "test1/sub/rand.bin", "data": rand},
        {"path": f"test1/sub/{long_name}", "data": b"korean"},
        {"path": "test1/empty", "dir": True},
        {"path": "test1/zero.txt", "data": b""},
    ]
    out, data = pack(wasm, ["test1"], items, env.clock())
    assert out["name"] == "test1" and out["kind"] == "dir"
    if wasm:
        assert len(data) < out["tar_size"] - len(text) // 2
    else:
        assert len(data) == out["estimate"]

    tf = tarfile.open(fileobj=io.BytesIO(zstd.decompress(data)))
    members = {m.name: m for m in tf.getmembers()}
    assert list(members) == [it["path"] for it in items]
    for it in items:
        m = members[it["path"]]
        if it.get("dir"):
            assert m.isdir() and m.mode == 0o755
        else:
            assert m.isfile() and m.mode == 0o644 and m.mtime == 1_700_000_000
            assert tf.extractfile(m).read() == it["data"]

    code = upload(env.public, data, name=out["name"], kind=out["kind"])["code"]
    zf = zipfile.ZipFile(io.BytesIO(env.public.get(f"/{code}").content))
    assert zf.read("test1/sub/rand.bin") == rand and zf.read(f"test1/sub/{long_name}") == b"korean"
    assert "test1/empty/" in zf.namelist()


@needs_node
@pytest.mark.parametrize("wasm", COMPRESSION)
def test_js_single_file(env, wasm):
    content = b"hello web upload\n" * 1000
    out, data = pack(wasm, ["hello.txt"], [{"path": "hello.txt", "data": content}], env.clock())
    assert out["name"] == "hello.txt" and out["kind"] == "file"
    code = upload(env.public, data, name=out["name"], kind=out["kind"])["code"]
    assert env.public.get(f"/{code}").content == content


@needs_node
def test_js_items_are_bundled(env):
    items = [{"path": "a.txt", "data": b"a"}, {"path": "b", "dir": True}, {"path": "b/c.txt", "data": b"c"}]
    out, data = pack(None, ["a.txt", "b"], items, env.clock())
    top = out["name"]
    assert out["kind"] == "bundle" and re.fullmatch(r"dtlk-\d{6}-\d{6}", top)
    assert out["items"] == [{"name": "a.txt", "kind": "file"}, {"name": "b", "kind": "dir"}]
    names = [m.name for m in tarfile.open(fileobj=io.BytesIO(zstd.decompress(data)))]
    assert names == [top, f"{top}/a.txt", f"{top}/b", f"{top}/b/c.txt"]
    code = upload(env.public, data, name=top, kind=out["kind"], items=out["items"])["code"]
    assert env.public.get(f"/{code}/a.txt").content == b"a"


@needs_node
def test_js_uniq_matches_server():
    names = [["a.txt", False], ["a.txt", False], ["photos.v2", True], ["photos.v2", True], [".bashrc", False], [".bashrc", False]]
    assert run_js({"uniq": names}) == ["a.txt", "a (2).txt", "photos.v2", "photos.v2 (2)", ".bashrc", ".bashrc (2)"]
