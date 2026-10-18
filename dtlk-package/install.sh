#!/usr/bin/env bash
# dtlk installer
#   curl -fsSL https://dtlk.ga111o.com/install.sh | bash
# Non-interactive: DTLK_SERVER=<url> DTLK_INSTALL_DIR=<dir> bash install.sh
set -euo pipefail

# The server replaces this placeholder with its public URL when serving the script.
ORIGIN="__DTLK_ORIGIN__"
DEFAULT_SERVER="https://dtlk.ga111o.com"

say() { printf '%s\n' "$*" >&2; }
die() { say "오류: $*"; exit 1; }

if [ -r /dev/tty ] && [ -w /dev/tty ] && { : </dev/tty; } 2>/dev/null; then
  TTY=1
else
  TTY=0
fi

# ask PROMPT DEFAULT -> prints the answer (DEFAULT when empty or no terminal)
ask() {
  local reply=""
  if [ "$TTY" = 1 ]; then
    printf '%s' "$1" >/dev/tty
    IFS= read -r reply </dev/tty || reply=""
  fi
  reply="${reply#"${reply%%[![:space:]]*}"}"
  reply="${reply%"${reply##*[![:space:]]}"}"
  printf '%s' "${reply:-$2}"
}

normalize() {
  local s="${1%/}"
  case "$s" in
    http://*|https://*) ;;
    *) s="https://$s" ;;
  esac
  printf '%s' "$s"
}

healthy() { curl -fsS --max-time 10 "$1/api/health" >/dev/null 2>&1; }

[ "$(uname -s)" = "Linux" ] || die "Linux만 지원합니다."
command -v curl >/dev/null || die "curl이 필요합니다."
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "지원하지 않는 CPU입니다: $(uname -m) (amd64, arm64만 지원)" ;;
esac

# 1) server
if [ -n "${DTLK_SERVER:-}" ]; then
  SERVER="$(normalize "$DTLK_SERVER")"
  healthy "$SERVER" || die "서버에 연결할 수 없습니다: $SERVER"
else
  while :; do
    say ""
    say "연결할 dtlk 서버를 선택하세요."
    say "  1) dtlk.ga111o.com (기본)"
    say "  2) 직접 입력"
    case "$(ask "선택 [1]: " 1)" in
      1) SERVER="$DEFAULT_SERVER" ;;
      2)
        input="$(ask "서버 주소 (예: files.example.com): " "")"
        [ -n "$input" ] || { say "주소를 입력하세요."; continue; }
        SERVER="$(normalize "$input")"
        ;;
      *) say "1 또는 2를 입력하세요."; continue ;;
    esac
    if healthy "$SERVER"; then
      break
    fi
    say "서버에 연결할 수 없습니다: $SERVER"
    [ "$TTY" = 1 ] || die "서버 연결 실패"
  done
fi

# 2) install location
if [ -n "${DTLK_INSTALL_DIR:-}" ]; then
  DIR="$DTLK_INSTALL_DIR"
else
  while :; do
    say ""
    say "설치 위치를 선택하세요."
    say "  1) /usr/local/bin  (모든 사용자, sudo 필요)"
    say "  2) ~/.local/bin    (현재 사용자만)"
    case "$(ask "선택 [1]: " 1)" in
      1) DIR="/usr/local/bin"; break ;;
      2) DIR="$HOME/.local/bin"; break ;;
      *) say "1 또는 2를 입력하세요." ;;
    esac
  done
fi

# 3) download and verify (from the server that served this script)
if [ "${ORIGIN#__DTLK}" != "$ORIGIN" ]; then
  ORIGIN="$SERVER"
fi
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
BIN="dtlk-linux-$ARCH"
say ""
say "다운로드: $ORIGIN/dl/$BIN"
curl -fsSL "$ORIGIN/dl/$BIN" -o "$TMP/dtlk" || die "다운로드 실패"
curl -fsSL "$ORIGIN/dl/$BIN.sha256" -o "$TMP/dtlk.sha256" || die "체크섬 다운로드 실패"
expected="$(awk '{print $1}' "$TMP/dtlk.sha256")"
if command -v sha256sum >/dev/null; then
  actual="$(sha256sum "$TMP/dtlk" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "$TMP/dtlk" | awk '{print $1}')"
fi
[ -n "$expected" ] && [ "$expected" = "$actual" ] || die "체크섬이 맞지 않습니다."
chmod 755 "$TMP/dtlk"

# 4) install
if mkdir -p "$DIR" 2>/dev/null && [ -w "$DIR" ]; then
  install -m 755 "$TMP/dtlk" "$DIR/dtlk"
else
  command -v sudo >/dev/null || die "$DIR 에 쓸 권한이 없습니다 (sudo 없음)."
  say "$DIR 에 설치하려면 관리자 권한이 필요합니다."
  if [ "$TTY" = 1 ]; then
    sudo mkdir -p "$DIR" </dev/tty
    sudo install -m 755 "$TMP/dtlk" "$DIR/dtlk" </dev/tty
  else
    sudo -n mkdir -p "$DIR" && sudo -n install -m 755 "$TMP/dtlk" "$DIR/dtlk" || die "sudo 권한이 필요합니다."
  fi
fi

# 5) config
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/dtlk"
mkdir -p "$CONFIG_DIR"
printf 'server=%s\n' "$SERVER" >"$CONFIG_DIR/config"

say ""
say "설치 완료: $DIR/dtlk ($("$DIR/dtlk" version))"
say "서버: $SERVER  (설정: $CONFIG_DIR/config)"
case ":$PATH:" in
  *":$DIR:"*) ;;
  *) say "주의: $DIR 이(가) PATH에 없습니다. ~/.bashrc 등에 추가하세요:"
     say "  export PATH=\"$DIR:\$PATH\"" ;;
esac
say ""
say "사용법:"
say "  dtlk up <파일 또는 폴더> [--h 시간]"
say "  dtlk down <6자리 코드>"
