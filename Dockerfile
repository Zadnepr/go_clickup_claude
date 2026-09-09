# syntax=docker/dockerfile:1

# ---- сборка статического бинарника ------------------------------------
FROM golang:1.23 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/reviewer ./cmd/reviewer

# ---- финальный образ ----------------------------------------------------
# Node.js нужен для claude (@anthropic-ai/claude-code) и cup (@krodak/clickup-cli).
FROM node:22-bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates openssh-client \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @anthropic-ai/claude-code @krodak/clickup-cli \
    && npm cache clean --force

COPY --from=build /out/reviewer /usr/local/bin/reviewer
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# node:22-slim уже содержит непривилегированного пользователя "node" (uid 1000).
# HOME задаём явно: claude пишет состояние в ~/.claude, и в контейнере без
# этого переменная бывает пустой.
ENV HOME=/home/node
RUN mkdir -p /data /repo "$HOME" \
    && chown -R node:node /data /repo "$HOME"

ENV DB_PATH=/data/state.db \
    REPO_PATH=/repo \
    PORT=8080

# Смонтированный ~/.ssh/config хоста может содержать macOS-специфичные
# директивы (например, UseKeychain из форка OpenSSH в Apple), которые
# линуксовый ssh здесь не понимает и без этого падает на парсинге всего
# файла. IgnoreUnknown просит молча пропустить только их, конфиг реального
# ~/.ssh/config при этом не трогаем и не копируем.
ENV GIT_SSH_COMMAND="ssh -o IgnoreUnknown=UseKeychain"

VOLUME ["/data", "/repo"]
EXPOSE 8080

WORKDIR /repo

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["node", "-e", "require('http').get('http://127.0.0.1:8080/healthz',r=>process.exit(r.statusCode===200?0:1)).on('error',()=>process.exit(1))"]

# Контейнер стартует от root — entrypoint чинит права на проброшенный сокет
# SSH-агента (см. docker-entrypoint.sh) и сам запускает сервис от node,
# root-процессов после старта не остаётся.
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/usr/local/bin/reviewer"]
