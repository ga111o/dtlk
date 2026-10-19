#!/bin/sh
# Builds dtlk-server/build/zstd.wasm for local runs and tests (Docker builds make their own).
set -eu
cd "$(dirname "$0")/../.."
docker build -f dtlk-server/Dockerfile --target zstd-wasm-export --output dtlk-server/build .
ls -l dtlk-server/build/zstd.wasm
