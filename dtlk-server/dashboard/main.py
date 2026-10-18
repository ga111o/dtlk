"""dtlk admin dashboard. Reachable only from the private network; no login."""

import os
from datetime import datetime
from typing import Any
from zoneinfo import ZoneInfo

import httpx2
import streamlit as st

API = os.environ.get("DTLK_ADMIN_API", "http://api:8001").rstrip("/")
TZ = ZoneInfo(os.environ.get("DTLK_TZ", "Asia/Seoul"))
GiB = 1024**3

st.set_page_config(page_title="dtlk 관리", page_icon="📦", layout="wide")


def call(method: str, path: str, **kwargs: Any) -> Any:
    try:
        r = httpx2.request(method, API + path, timeout=15, **kwargs)
    except httpx2.HTTPError as exc:
        st.error(f"관리 API({API})에 연결할 수 없습니다: {exc}")
        st.stop()
    if r.status_code >= 400:
        try:
            detail = r.json().get("detail")
        except ValueError:
            detail = r.text
        st.error(f"요청 실패 ({r.status_code}): {detail}")
        st.stop()
    return r.json() if r.content else None


def human(n: float) -> str:
    for unit in ("B", "KB", "MB", "GB"):
        if n < 1024 or unit == "GB":
            return f"{n:.0f} {unit}" if unit == "B" else f"{n:.1f} {unit}"
        n /= 1024
    return f"{n} B"


def when(ts: float) -> str:
    return datetime.fromtimestamp(ts, TZ).strftime("%Y-%m-%d %H:%M")


def flash(msg: str) -> None:
    st.session_state["flash"] = msg
    st.rerun()


st.title("dtlk 관리")
if msg := st.session_state.pop("flash", None):
    st.success(msg)
if st.button("새로고침"):
    st.rerun()

ov = call("GET", "/admin/overview")
settings = ov["settings"]
tab_overview, tab_files, tab_settings, tab_blocks = st.tabs(["개요", "파일", "설정", "차단 IP"])

with tab_overview:
    used = ov["used_bytes"] + ov["reserved_bytes"]
    cap = settings["storage_cap_bytes"]
    c1, c2, c3, c4 = st.columns(4)
    c1.metric("사용 중", human(used), help="완료된 파일 + 진행 중인 업로드 예약분")
    c2.metric("용량 상한", human(cap))
    c3.metric("활성 파일", len(ov["files"]))
    c4.metric("진행 중 업로드", len(ov["uploads"]))
    st.progress(min(used / cap, 1.0), text=f"{used / cap:.1%} 사용 (파일 {human(ov['used_bytes'])}, 업로드 예약 {human(ov['reserved_bytes'])})")
    if disk := ov["disk"]:
        st.caption(f"RustFS 디스크: 전체 {human(disk['total'])}, 남은 공간 {human(disk['free'])}")
        if cap - used > disk["free"]:
            st.warning("남은 상한이 실제 디스크 남은 공간보다 큽니다. 설정 탭에서 상한을 낮추는 것을 고려하세요.")

with tab_files:
    st.subheader("활성 파일")
    files = ov["files"]
    if files:
        table = [
            {
                "코드": f["code"],
                "이름": f["name"] + ("/" if f["kind"] == "dir" else ""),
                "크기": human(f["size"]),
                "업로드 IP": f["ip"],
                "업로드": when(f["created_at"]),
                "만료": when(f["expires_at"]),
            }
            for f in files
        ]
        picked = st.dataframe(table, hide_index=True, on_select="rerun", selection_mode="multi-row", key="files")
        rows = picked.selection.rows
        if st.button(f"선택한 파일 {len(rows)}개 삭제", disabled=not rows, type="primary"):
            for i in rows:
                call("DELETE", f"/admin/files/{files[i]['code']}")
            flash(f"파일 {len(rows)}개를 삭제했습니다.")
    else:
        st.info("활성 파일이 없습니다.")

    st.subheader("진행 중 업로드")
    uploads = ov["uploads"]
    if uploads:
        table = [
            {
                "이름": u["name"] + ("/" if u["kind"] == "dir" else ""),
                "받은 크기": human(u["received"]),
                "예약": human(max(u["reserved"], u["received"])),
                "IP": u["ip"],
                "시작": when(u["created_at"]),
                "마지막 활동": when(u["updated_at"]),
                "상태": u["state"],
            }
            for u in uploads
        ]
        picked = st.dataframe(table, hide_index=True, on_select="rerun", selection_mode="multi-row", key="uploads")
        rows = picked.selection.rows
        if st.button(f"선택한 업로드 {len(rows)}개 중단", disabled=not rows, type="primary"):
            for i in rows:
                call("DELETE", f"/admin/uploads/{uploads[i]['id']}")
            flash(f"업로드 {len(rows)}개를 중단했습니다.")
    else:
        st.info("진행 중인 업로드가 없습니다.")

with tab_settings:
    with st.form("settings"):
        cap_gb = st.number_input("저장 용량 상한 (GB)", min_value=1, value=max(1, settings["storage_cap_bytes"] // GiB), step=1)
        st.markdown("**코드 조회 실패 제한** (잘못된 6자리를 반복 입력하는 IP 차단)")
        a, b, c = st.columns(3)
        fail_max = a.number_input("허용 실패 횟수", min_value=1, value=settings["lookup_fail_max"], step=1)
        fail_window = b.number_input("집계 시간 (분)", min_value=1, value=max(1, settings["lookup_fail_window_sec"] // 60), step=1)
        block_min = c.number_input("차단 시간 (분)", min_value=1, value=max(1, settings["lookup_block_sec"] // 60), step=1)
        st.markdown("**IP당 업로드 제한**")
        a, b = st.columns(2)
        concurrent = a.number_input("동시 업로드 수", min_value=1, value=settings["upload_concurrent_max"], step=1)
        hourly = b.number_input("시간당 업로드 수", min_value=1, value=settings["upload_hourly_max"], step=1)
        if st.form_submit_button("저장", type="primary"):
            call(
                "PUT",
                "/admin/settings",
                json={
                    "storage_cap_bytes": int(cap_gb) * GiB,
                    "lookup_fail_max": int(fail_max),
                    "lookup_fail_window_sec": int(fail_window) * 60,
                    "lookup_block_sec": int(block_min) * 60,
                    "upload_concurrent_max": int(concurrent),
                    "upload_hourly_max": int(hourly),
                },
            )
            flash("설정을 저장했습니다. 바로 적용됩니다.")

with tab_blocks:
    blocks = call("GET", "/admin/blocks")
    st.subheader("차단된 IP")
    if blocks["blocked"]:
        for blk in blocks["blocked"]:
            a, b, c = st.columns([3, 3, 1])
            a.write(f"`{blk['ip']}`")
            b.write(f"{when(blk['until'])}까지")
            if c.button("해제", key=f"unblock-{blk['ip']}"):
                call("DELETE", f"/admin/blocks/{blk['ip']}")
                flash(f"{blk['ip']} 차단을 해제했습니다.")
    else:
        st.info("차단된 IP가 없습니다.")

    st.subheader("최근 조회 실패")
    if blocks["failures"]:
        st.dataframe(
            [{"IP": f["ip"], "실패 횟수": f["count"], "마지막 실패": when(f["last"])} for f in blocks["failures"]],
            hide_index=True,
        )
    else:
        st.caption(f"최근 {settings['lookup_fail_window_sec'] // 60}분 동안 조회 실패가 없습니다.")
