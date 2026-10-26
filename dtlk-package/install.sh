#!/usr/bin/env bash
# dtlk installer
#   curl -fsSL https://dtlk.ga111o.com/install.sh | bash
# Non-interactive: DTLK_SERVER=<url> DTLK_INSTALL_DIR=<dir> bash install.sh
set -euo pipefail

# The server replaces this placeholder with its public URL when serving the script.
ORIGIN="__DTLK_ORIGIN__"
DEFAULT_SERVER="https://dtlk.ga111o.com"

say() { printf '%s\n' "$*" >&2; }
die() { say "Error: $*"; exit 1; }

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

# Give up on a connection that stays under 10 KB/s for 30 s and retry on a new one,
# instead of trickling forever with no output.
fetch() {
  curl -fL --connect-timeout 10 --speed-limit 10240 --speed-time 30 \
    --retry 3 --retry-delay 2 --retry-all-errors "$@"
}

[ "$(uname -s)" = "Linux" ] || die "Only Linux is supported."
command -v curl >/dev/null || die "curl is required."
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "Unsupported CPU architecture: $(uname -m) (only amd64 and arm64 are supported)" ;;
esac

# 1) server
if [ -n "${DTLK_SERVER:-}" ]; then
  SERVER="$(normalize "$DTLK_SERVER")"
  healthy "$SERVER" || die "Cannot connect to server: $SERVER"
else
  while :; do
    say ""
    say "Select dtlk server:"
    say "  1) dtlk.ga111o.com (default)"
    say "  2) Enter custom server URL"
    case "$(ask "Choice [1]: " 1)" in
      1) SERVER="$DEFAULT_SERVER" ;;
      2)
        input="$(ask "Server URL (e.g. files.example.com): " "")"
        [ -n "$input" ] || { say "Please enter a server URL."; continue; }
        SERVER="$(normalize "$input")"
        ;;
      *) say "Please enter 1 or 2."; continue ;;
    esac
    if healthy "$SERVER"; then
      break
    fi
    say "Cannot connect to server: $SERVER"
    [ "$TTY" = 1 ] || die "Failed to connect to server"
  done
fi

# 2) install location
if [ -n "${DTLK_INSTALL_DIR:-}" ]; then
  DIR="$DTLK_INSTALL_DIR"
else
  while :; do
    say ""
    say "Select install location:"
    say "  1) /usr/local/bin  (all users, requires sudo)"
    say "  2) ~/.local/bin    (current user only)"
    case "$(ask "Choice [1]: " 1)" in
      1) DIR="/usr/local/bin"; break ;;
      2) DIR="$HOME/.local/bin"; break ;;
      *) say "Please enter 1 or 2." ;;
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
say "Downloading: $ORIGIN/dl/$BIN"
fetch --progress-bar "$ORIGIN/dl/$BIN" -o "$TMP/dtlk" || die "Download failed (connection too slow or interrupted; please try again)"
fetch -sS "$ORIGIN/dl/$BIN.sha256" -o "$TMP/dtlk.sha256" || die "Failed to download checksum"
expected="$(awk '{print $1}' "$TMP/dtlk.sha256")"
if command -v sha256sum >/dev/null; then
  actual="$(sha256sum "$TMP/dtlk" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "$TMP/dtlk" | awk '{print $1}')"
fi
[ -n "$expected" ] && [ "$expected" = "$actual" ] || die "Checksum verification failed."
chmod 755 "$TMP/dtlk"

# 4) install
if mkdir -p "$DIR" 2>/dev/null && [ -w "$DIR" ]; then
  install -m 755 "$TMP/dtlk" "$DIR/dtlk"
else
  command -v sudo >/dev/null || die "No write permission for $DIR (and sudo is not available)."
  say "Installing to $DIR requires sudo privileges."
  if [ "$TTY" = 1 ]; then
    sudo mkdir -p "$DIR" </dev/tty
    sudo install -m 755 "$TMP/dtlk" "$DIR/dtlk" </dev/tty
  else
    sudo -n mkdir -p "$DIR" && sudo -n install -m 755 "$TMP/dtlk" "$DIR/dtlk" || die "sudo privileges are required."
  fi
fi

# 5) config: replace server=, keep everything else (e.g. tag/hours set with `dtlk config`)
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/dtlk"
CONFIG="$CONFIG_DIR/config"
mkdir -p "$CONFIG_DIR"
rest=""
if [ -f "$CONFIG" ]; then
  rest="$(grep -v '^[[:space:]]*server[[:space:]]*=' "$CONFIG" || true)"
fi
{
  printf 'server=%s\n' "$SERVER"
  [ -z "$rest" ] || printf '%s\n' "$rest"
} >"$CONFIG"

say ""
say "Installed: $DIR/dtlk ($("$DIR/dtlk" version))"
say "Server: $SERVER  (config: $CONFIG)"
case ":$PATH:" in
  *":$DIR:"*) ;;
  *) say "Note: $DIR is not in PATH. Add it to your shell profile (e.g. ~/.bashrc):"
     say "  export PATH=\"$DIR:\$PATH\"" ;;
esac
say ""
say "Usage:"
say "  dtlk up <file-or-dir> [-h hours] [-t tag]"
say "  dtlk up -m \"text\" [-h hours] [-t tag]"
say "  dtlk down [server/]<6-digit-code>"
say "  dtlk files <tag>          (list files uploaded with -t <tag>)"
say "  dtlk config tag <tag>     (apply -t <tag> to every upload)"
