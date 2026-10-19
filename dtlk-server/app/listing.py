"""The /files/<name> page: active files uploaded with `dtlk up -n <name>`.

Browsers (Accept: text/html) get an HTML table, everything else (curl, wget) plain text.
"""

import html
import sqlite3
import time
from string import Template

from fastapi.responses import HTMLResponse, PlainTextResponse, Response

from .service import ApiError

HEADERS = {"Cache-Control": "no-store", "X-Robots-Tag": "noindex"}

PAGE = Template("""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>$title · dtlk</title>
<style>
:root { color-scheme: light dark; --fg: #1f2328; --muted: #656d76; --line: #d0d7de; --bg: #ffffff; --link: #0969da; }
@media (prefers-color-scheme: dark) {
  :root { --fg: #e6edf3; --muted: #8d96a0; --line: #30363d; --bg: #0d1117; --link: #4493f8; }
}
body { margin: 0; padding: 32px 16px; background: var(--bg); color: var(--fg); font: 15px/1.5 system-ui, sans-serif; }
main { max-width: 760px; margin: 0 auto; }
h1 { font-size: 20px; margin: 0 0 4px; overflow-wrap: anywhere; }
p { color: var(--muted); margin: 0 0 20px; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 8px 12px 8px 0; border-bottom: 1px solid var(--line); vertical-align: top; }
th { color: var(--muted); font-weight: 500; font-size: 13px; }
td.name { overflow-wrap: anywhere; }
td.when { white-space: nowrap; color: var(--muted); }
td.code a { font: 600 15px ui-monospace, monospace; color: var(--link); text-decoration: none; }
td.code a:hover { text-decoration: underline; }
</style>
</head>
<body><main>
$body
</main></body>
</html>
""")


def _when(ts: float) -> str:
    # The api container's TZ is DTLK_TZ (default Asia/Seoul).
    return time.strftime("%Y-%m-%d %H:%M %Z", time.localtime(ts))


def _display_name(row: sqlite3.Row) -> str:
    return row["name"] + ("/" if row["kind"] == "dir" else "")


def render(tag: str, rows: list[sqlite3.Row], as_html: bool) -> Response:
    if not as_html:
        text = "".join(f"{r['code']}  {_when(r['expires_at'])}  {_display_name(r)}\n" for r in rows)
        return PlainTextResponse(text, headers=HEADERS)

    trs = "\n".join(
        f'<tr><td class="name">{html.escape(_display_name(r))}</td>'
        f'<td class="when">{_when(r["expires_at"])}</td>'
        f'<td class="code"><a href="/{r["code"]}">{r["code"]}</a></td></tr>'
        for r in rows
    )
    count = f"{len(rows)} file{'s' if len(rows) != 1 else ''}"
    body = (
        f"<h1>{html.escape(tag)}</h1>\n<p>{count} · click a code to download</p>\n"
        "<table>\n<thead><tr><th>Name</th><th>Expires</th><th>Code</th></tr></thead>\n"
        f"<tbody>\n{trs}\n</tbody>\n</table>"
    )
    return HTMLResponse(PAGE.substitute(title=html.escape(tag), body=body), headers=HEADERS)


def render_error(exc: ApiError, as_html: bool) -> Response:
    headers = dict(HEADERS)
    if exc.retry_after:
        headers["Retry-After"] = str(exc.retry_after)
    if not as_html:
        return PlainTextResponse(exc.detail + "\n", status_code=exc.status, headers=headers)
    body = f"<h1>dtlk</h1>\n<p>{html.escape(exc.detail)}</p>"
    return HTMLResponse(PAGE.substitute(title="dtlk", body=body), status_code=exc.status, headers=headers)
