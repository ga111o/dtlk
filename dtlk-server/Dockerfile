# syntax=docker/dockerfile:1
# Build context is the repository root (see docker-compose.yml).

# ---- Go client: test, then build the binaries the server hands out ----
FROM golang:1.27.2-alpine AS client
WORKDIR /src
COPY dtlk-package/go.mod dtlk-package/go.sum ./
RUN go mod download
COPY dtlk-package/ ./
ARG DTLK_VERSION=dev
RUN go vet ./... && CGO_ENABLED=0 go test ./...
RUN mkdir /out && for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X main.version=${DTLK_VERSION}" -o /out/dtlk-linux-$arch . && \
      (cd /out && sha256sum dtlk-linux-$arch > dtlk-linux-$arch.sha256); \
    done

# ---- Python base ----
FROM python:3.14.8-slim AS base
COPY --from=ghcr.io/astral-sh/uv:0.13.1 /uv /usr/local/bin/uv
ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never \
    PYTHONUNBUFFERED=1 \
    PATH="/app/.venv/bin:$PATH"
WORKDIR /app
COPY dtlk-server/pyproject.toml dtlk-server/uv.lock dtlk-server/.python-version ./

# ---- tests (docker build --target test) ----
FROM base AS test
RUN uv sync --frozen
COPY --from=client /out /tmp/client-built
COPY dtlk-server/app ./app
COPY dtlk-server/tests ./tests
RUN uv run --frozen pytest -q

# ---- runtime ----
FROM base AS runtime
RUN uv sync --frozen --no-dev
COPY dtlk-server/app ./app
COPY dtlk-server/dashboard ./dashboard
COPY dtlk-package/install.sh ./static/install.sh
COPY --from=client /out ./static/dl
RUN useradd --system --uid 10002 --home-dir /app dtlk && mkdir -p /data && chown dtlk:dtlk /data
USER dtlk
EXPOSE 8000 8001 8501
CMD ["python", "-m", "app.serve"]
