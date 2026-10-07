# ── Stage 1: Go backend builder ──────────────────────────────────────
FROM golang:1.23-bookworm AS go-builder
ENV CGO_ENABLED=0
WORKDIR /src
# Module deps first for layer caching.
COPY go.mod go.sum ./
RUN go mod download
COPY backend/ ./backend/
RUN go build -trimpath -o /out/fixlab-server ./backend/cmd/server

# ── Stage 2: Next.js frontend builder ────────────────────────────────
FROM node:24-bookworm-slim AS node-builder
WORKDIR /fe
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
# The standalone server must call the backend over the browser's network,
# so NEXT_PUBLIC_FIXLAB_API_URL is baked in at build time. In compose the
# frontend is served to browsers that reach the backend at the same host:
# override via --build-arg NEXT_PUBLIC_FIXLAB_API_URL=... when needed.
ARG NEXT_PUBLIC_FIXLAB_API_URL=http://localhost:8080
ENV NEXT_PUBLIC_FIXLAB_API_URL=$NEXT_PUBLIC_FIXLAB_API_URL
RUN npm run build
# Standalone output never emits public/ when the dir is absent; create it so
# the runtime COPY below always has a source.
RUN mkdir -p public

# ── Stage 3: runtime ─────────────────────────────────────────────────
FROM node:24-bookworm-slim
WORKDIR /app
COPY --from=go-builder /out/fixlab-server /app/fixlab-server
COPY --from=node-builder /fe/.next/standalone /app/frontend/
COPY --from=node-builder /fe/.next/static /app/frontend/.next/static/
# `public/` may be empty in dev; tolerate a missing dir.
COPY --from=node-builder /fe/public /app/frontend/public
COPY docker/entrypoint /usr/local/bin/entrypoint
RUN chmod +x /usr/local/bin/entrypoint

EXPOSE 8080 3000
ENTRYPOINT ["entrypoint"]
CMD ["all"]
