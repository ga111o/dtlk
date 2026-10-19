"""The /files/<tag> page: active files uploaded with `dtlk up -t <tag>`.

Browsers (Accept: text/html) get an HTML table, everything else (curl, wget) plain text.
The HTML is deliberately plain (no CSS, no JS) so that any browser, however old, renders it.
"""

import html
import sqlite3
import time
from string import Template
from urllib.parse import quote

from fastapi.responses import HTMLResponse, PlainTextResponse, Response

from .service import ApiError

HEADERS = {"Cache-Control": "no-store", "X-Robots-Tag": "noindex"}

PAGE = Template("""<!DOCTYPE html>
<html>
<head>
<meta http-equiv="Content-Type" content="text/html; charset=utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>$title - dtlk</title>
</head>
<body>
$body
</body>
</html>
""")


def when(ts: float) -> str:
    # The api container's TZ is DTLK_TZ (default Asia/Seoul).
    return time.strftime("%Y-%m-%d %H:%M %Z", time.localtime(ts))


def _display_name(row: sqlite3.Row) -> str:
    return row["name"] + ("/" if row["kind"] != "file" else "")


def render(tag: str, rows: list[sqlite3.Row], as_html: bool) -> Response:
    if not as_html:
        text = "".join(f"{r['code']}  {when(r['expires_at'])}  {_display_name(r)}\n" for r in rows)
        return PlainTextResponse(text, headers=HEADERS)

    trs = "\n".join(
        f"<tr><td>{html.escape(_display_name(r))}</td>"
        f"<td nowrap>{when(r['expires_at'])}</td>"
        f'<td><a href="/{r["code"]}">{r["code"]}</a></td></tr>'
        for r in rows
    )
    count = f"{len(rows)} file{'s' if len(rows) != 1 else ''}"
    body = (
        f"<h1>{html.escape(tag)}</h1>\n"
        '<table border="1" cellpadding="6" cellspacing="0">\n'
        f"<tr><th>Name</th><th>Expires</th><th>Code</th></tr>\n{trs}\n</table>"
    )
    return HTMLResponse(PAGE.substitute(title=html.escape(tag), body=body), headers=HEADERS)


def render_bundle(code: str, name: str, items: list[dict[str, str]], expires_at: float, origin: str) -> Response:
    """The browser page of a bundle: a hidden frame per item starts its download, the links are a fallback."""
    trs: list[str] = []
    frames: list[str] = []
    for it in items:
        href = f"/{code}/{quote(it['name'], safe='')}"
        folder = it["kind"] == "dir"
        trs.append(
            f'<tr><td><a href="{href}">{html.escape(it["name"] + ("/" if folder else ""))}</a></td>'
            f"<td>{html.escape(it['name'] + ('.zip' if folder else ''))}</td></tr>"
        )
        frames.append(f'<iframe src="{href}" width="0" height="0" frameborder="0"></iframe>')
    body = (
        f"<h1>{html.escape(name)}/</h1>\n"
        f"<p>{len(items)} items, expires {when(expires_at)}. Each item downloads on its own: "
        "if the browser asks, allow multiple downloads, or click the links.</p>\n"
        '<table border="1" cellpadding="6" cellspacing="0">\n'
        "<tr><th>Item</th><th>Saved as</th></tr>\n" + "\n".join(trs) + "\n</table>\n"
        f"<p>CLI: <code>dtlk down {code}</code> or <code>curl -s {html.escape(origin)}/{code} | tar x</code></p>\n"
        + "\n".join(frames)
    )
    return HTMLResponse(PAGE.substitute(title=html.escape(name), body=body), headers=HEADERS)


def render_error(exc: ApiError, as_html: bool) -> Response:
    headers = dict(HEADERS)
    if exc.retry_after:
        headers["Retry-After"] = str(exc.retry_after)
    if not as_html:
        return PlainTextResponse(exc.detail + "\n", status_code=exc.status, headers=headers)
    body = f"<h1>dtlk</h1>\n<p>{html.escape(exc.detail)}</p>"
    return HTMLResponse(PAGE.substitute(title="dtlk", body=body), status_code=exc.status, headers=headers)
