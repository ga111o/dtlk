#!/bin/sh
# Builds zstd.wasm: the compression half of facebook/zstd for the web upload page.
# Runs in the Dockerfile's zstd-wasm stage (any glibc Linux with python3 works):
#   compile.sh <output dir>
set -eu

ZSTD_VERSION=1.5.7
ZSTD_SHA256=eb33e51f49a15e023950cd7825ca74a4a2b43db8354825ac24fc1b7ee09e6fa3
WASI_SDK_VERSION=34.0
case "$(uname -m)" in
  x86_64) SDK_ARCH=x86_64 SDK_SHA256=b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4 ;;
  aarch64 | arm64) SDK_ARCH=arm64 SDK_SHA256=f7e243dff54d60bcc576e94d6166b69f410f2500ae4a9ceef34315be10e77971 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

OUT=$(mkdir -p "$1" && cd "$1" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"

# fetch <url> <sha256>: download and verify, then unpack into the current directory
fetch() {
  python3 - "$1" "$2" <<'EOF'
import hashlib, sys, tarfile, urllib.request
url, want = sys.argv[1:]
path, _ = urllib.request.urlretrieve(url)
h = hashlib.sha256()
with open(path, "rb") as f:
    while chunk := f.read(1 << 20):
        h.update(chunk)
if h.hexdigest() != want:
    sys.exit(f"sha256 mismatch for {url}: {h.hexdigest()}")
with tarfile.open(path) as tf:
    tf.extractall(".", filter="tar")
EOF
}

fetch "https://github.com/facebook/zstd/releases/download/v$ZSTD_VERSION/zstd-$ZSTD_VERSION.tar.gz" "$ZSTD_SHA256"
fetch "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-${WASI_SDK_VERSION%.*}/wasi-sdk-$WASI_SDK_VERSION-$SDK_ARCH-linux.tar.gz" "$SDK_SHA256"

SDK=$WORK/wasi-sdk-$WASI_SDK_VERSION-$SDK_ARCH-linux
LIB=$WORK/zstd-$ZSTD_VERSION/lib
# A reactor module (no main): upload.js calls _initialize once, then the exports below.
# upload.js only uses level 3 (the dfast strategy), so the slower strategies are left out.
"$SDK/bin/clang" --target=wasm32-wasip1 --sysroot="$SDK/share/wasi-sysroot" -O2 \
  -mexec-model=reactor \
  -DZSTD_LEGACY_SUPPORT=0 -DZSTD_STRIP_ERROR_STRINGS -DDEBUGLEVEL=0 \
  -DZSTD_EXCLUDE_GREEDY_BLOCK_COMPRESSOR -DZSTD_EXCLUDE_LAZY_BLOCK_COMPRESSOR \
  -DZSTD_EXCLUDE_LAZY2_BLOCK_COMPRESSOR -DZSTD_EXCLUDE_BTLAZY2_BLOCK_COMPRESSOR \
  -DZSTD_EXCLUDE_BTOPT_BLOCK_COMPRESSOR -DZSTD_EXCLUDE_BTULTRA_BLOCK_COMPRESSOR \
  -I"$LIB" -I"$LIB/common" \
  "$LIB"/common/*.c "$LIB"/compress/*.c \
  -Wl,--export=ZSTD_createCCtx -Wl,--export=ZSTD_compressCCtx \
  -Wl,--export=ZSTD_compressBound -Wl,--export=ZSTD_isError \
  -Wl,--export=malloc -Wl,--export=free \
  -Wl,--strip-all \
  -o "$OUT/zstd.wasm"
ls -l "$OUT/zstd.wasm"
