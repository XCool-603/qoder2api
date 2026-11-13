# =============================================================================
# Stage 1: Build stage (Go 1.23)
# =============================================================================
FROM golang:1.23-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/qoder2api ./cmd/server

# =============================================================================
# Stage 2: Runtime stage (Alpine 3.20)
# =============================================================================
FROM alpine:3.20

ARG UID=10001
ARG GID=10001

RUN apk add --no-cache wget ca-certificates tzdata bash nodejs npm su-exec \
 && addgroup -g "${GID}" -S app \
 && adduser -u "${UID}" -S -G app -h /home/app -s /sbin/nologin app \
 && install -d -o app -g app -m 755 /app /app/auths /app/data /home/app

WORKDIR /app
ENV HOME=/home/app \
    TZ=Asia/Shanghai

COPY --from=build --chown=app:app --chmod=755 /out/qoder2api /app/qoder2api
COPY --chown=app:app --chmod=755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY --chown=app:app config.example.json /app/config.json
COPY --chown=app:app config/ /app/config/

EXPOSE 7865
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7865/ui/public/status || exit 1

USER app

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["-headless", "-config", "/app/config.json"]
