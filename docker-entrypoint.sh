#!/bin/sh
# Контейнер стартует от root (см. Dockerfile), чтобы этот скрипт мог
# починить права на смонтированный сокет SSH-агента — Docker Desktop
# прокидывает его как root:root, а сервис работает от непривилегированного
# пользователя node и без этого не сможет подключиться к агенту для
# git fetch по SSH-remote'ам. Сам процесс сервиса дальше запускается
# от node, не от root.
set -e

if [ -S /ssh-agent ]; then
    chmod 666 /ssh-agent 2>/dev/null || true
fi

exec runuser -u node -- "$@"
