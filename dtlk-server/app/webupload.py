"""The upload page at / and its fallback for browsers without JavaScript (POST /upload).

The page is plain HTML like the /files/<tag> list. Its script (upload.js) packs what is
dropped into the same tar.zst the CLI makes, compressing with zstd.wasm where the
browser can run it. Without the script the form posts the files here instead, which
nginx limits to 20 MB.
"""

import html
import io
import tarfile
import time
from datetime import datetime
from pathlib import Path
from typing import Any
from string import Template

from compression import zstd
from fastapi import Request
from fastapi.responses import HTMLResponse, Response
from starlette.concurrency import run_in_threadpool
from starlette.datastructures import UploadFile
from starlette.exceptions import HTTPException

from . import listing
from .config import Config, MiB
from .service import ApiError, Service, human, valid_name

FORM_MAX_BYTES = 20 * MiB  # nginx's client_max_body_size
SCRIPT = (Path(__file__).parent / "upload.js").read_text()

FORM = Template("""<h1>dtlk</h1>
<form id="form" method="post" action="/upload" enctype="multipart/form-data">
<p>Tag: <input type="text" name="tag" id="tag" maxlength="64"> (optional: letters, digits, - and _)</p>
<p>Hours: <input type="number" name="hours" id="hours" value="$hours" min="1" max="$max_hours"> (1-$max_hours)</p>
<p>Files: <input type="file" name="file" id="files" multiple><span id="folder"></span></p>
<p id="how"><input type="submit" value="Upload"> Without JavaScript, uploads are limited to 20 MB in total.</p>
</form>
<div id="out"></div>
<hr>
<p>Command line: <code>curl -fsSL $origin/install.sh | bash</code>, then <code>dtlk up &lt;path&gt;</code></p>
<script>
$script</script>""")


def render_page(cfg: Config) -> str:
    body = FORM.substitute(
        hours=cfg.default_hours, max_hours=cfg.max_hours, origin=html.escape(cfg.public_url), script=SCRIPT
    )
    return listing.PAGE.substitute(title="dtlk", body=body)


def _basename(filename: str) -> str:
    # Old IE sends the full client path.
    return filename.replace("\\", "/").rsplit("/", 1)[-1]


def _uniq(name: str, used: set[str]) -> str:
    """Same as uniq() in upload.js: the CLI refuses to unpack two "a.txt"s."""
    dot = name.rfind(".")
    if dot <= 0:
        dot = len(name)
    n, i = name, 1
    while n in used:
        i += 1
        n = f"{name[:dot]} ({i}){name[dot:]}"
    used.add(n)
    return n


def _archive(files: list[tuple[str, bytes]], now: float) -> tuple[str, str, bytes]:
    """name, kind and tar.zst of the posted files; several go into a dtlk-yymmdd-hhmmss folder."""
    bundle = len(files) > 1
    top = time.strftime("dtlk-%y%m%d-%H%M%S", time.localtime(now)) if bundle else files[0][0]
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.PAX_FORMAT) as tf:
        if bundle:
            info = tarfile.TarInfo(top)
            info.type, info.mode, info.mtime = tarfile.DIRTYPE, 0o755, int(now)
            tf.addfile(info)
        for name, data in files:
            info = tarfile.TarInfo(f"{top}/{name}" if bundle else name)
            info.size, info.mode, info.mtime = len(data), 0o644, int(now)
            tf.addfile(info, io.BytesIO(data))
    return top, "dir" if bundle else "file", zstd.compress(raw.getvalue())


def _store(svc: Service, ip: str, files: list[tuple[str, bytes]], hours: int, tag: str | None) -> dict[str, Any]:
    name, kind, data = _archive(files, svc.clock())
    up = svc.create_upload(ip, name, kind, len(data), hours, tag)
    try:
        step = up["chunk_size"]
        for i in range(0, len(data), step):
            svc.put_part(up["upload_id"], i // step + 1, data[i : i + step])
        res = svc.complete_upload(up["upload_id"])
    except BaseException:
        svc.abort_upload(up["upload_id"])
        raise
    return {**res, "name": name + ("/" if kind == "dir" else ""), "raw": sum(len(d) for _, d in files)}


def _result(cfg: Config, res: dict[str, Any], tag: str | None) -> Response:
    code = res["code"]
    rows = [
        ("Name", html.escape(res["name"])),
        ("Code", f"<b>{code}</b>"),
        ("Expires", listing.when(datetime.fromisoformat(res["expires_at"]).timestamp())),
        ("Size", f"{human(res['raw'])} &rarr; {human(res['size'])} (zstd)"),
        ("Web", f'<a href="/{code}">{html.escape(cfg.public_url)}/{code}</a>'),
        ("CLI", f"dtlk down {code}"),
    ]
    if tag:
        rows.append(("List", f'<a href="/files/{tag}">{html.escape(cfg.public_url)}/files/{tag}</a>'))
    trs = "\n".join(f'<tr><th align="left">{k}</th><td>{v}</td></tr>' for k, v in rows)
    body = (
        '<h1>Uploaded</h1>\n<table border="1" cellpadding="6" cellspacing="0">\n'
        f'{trs}\n</table>\n<p><a href="/">Upload more</a></p>'
    )
    return HTMLResponse(listing.PAGE.substitute(title=code, body=body), headers=listing.HEADERS)


async def handle_form(svc: Service, ip: str, request: Request) -> Response:
    cfg = svc.cfg
    try:
        length = request.headers.get("content-length", "")
        if not length.isdigit() or int(length) > FORM_MAX_BYTES:
            raise ApiError(413, "Without JavaScript, uploads are limited to 20 MB; enable JavaScript or use dtlk up")
        try:
            async with request.form(max_files=1000, max_fields=10) as form:
                posted = [f for f in form.getlist("file") if isinstance(f, UploadFile) and f.filename]
                used: set[str] = set()
                files = [(_uniq(_basename(f.filename or ""), used), await f.read()) for f in posted]
                tag_field, hours_field = form.get("tag"), form.get("hours")
        except HTTPException as exc:
            raise ApiError(400, f"Invalid form data: {exc.detail}")

        if not files:
            raise ApiError(400, "Choose at least one file")
        for name, _ in files:
            if not valid_name(name):
                raise ApiError(400, f"Invalid file name: {name}")
        hours = str(hours_field or "").strip()
        if not hours.isdigit():
            raise ApiError(400, f"Retention hours must be an integer between 1 and {cfg.max_hours}")
        tag = str(tag_field or "").strip().lower() or None
        res = await run_in_threadpool(_store, svc, ip, files, int(hours), tag)
    except ApiError as exc:
        return listing.render_error(exc, as_html=True)
    return _result(cfg, res, tag)
