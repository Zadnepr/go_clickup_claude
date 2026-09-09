---
name: load-project-context
description: Определяет, какой саб-проект (panels/, sommerce/, editor/, PaymentsMethods/, docker/, migrations/, automigrations/) затрагивает задача, и подключает его CLAUDE.md как контекст. Используй в начале задачи, если из запроса пользователя неочевидно, к какому проекту он относится, или если задача затрагивает несколько проектов сразу.
user-invocable: false
---

# load-project-context

Реализует правило из корневого `CLAUDE.md`: "если задача затрагивает конкретный проект — изучи его CLAUDE.md; если задача проекта не касается — подключать не нужно".

## Карта проектов → CLAUDE.md

| Директория | CLAUDE.md | Когда подключать |
|---|---|---|
| `panels/` | `panels/CLAUDE.md` | Действия User/Admin API v1/v2, adminRestApi, action-классы, cron, RabbitMQ/Kafka panels, PanelAdmin/editor фронтенд |
| `sommerce/` | `sommerce/CLAUDE.md` | Биллинг, инвойсы, домены, superadmin/my/systemapi, cron-my, платежи, продукты (panel/store/gate/gateway) |
| `docker/` | `docker/CLAUDE.md` | Локальная разработка, docker-compose, контейнеры |
| `migrations/` | `migrations/CLAUDE.md` | SQL/PHP миграции структуры БД |
| `automigrations/` | `automigrations/CLAUDE.md` | Автоматизированная система миграций |
| `PaymentsMethods/` | `PaymentsMethods/CLAUDE.md` | Платёжные интеграции, базовые классы платёжек |

`editor/`, `PanelAdmin/`, `SuperTasks/` пока не имеют своего `CLAUDE.md` — ориентируйся по коду и README, если есть.

## Алгоритм

1. Посмотри, какие файлы/пути упомянуты в задаче или уже открыты.
2. Сопоставь с таблицей выше. Если совпадает несколько директорий (например, правка одновременно в `panels/` и `sommerce/`) — подключи CLAUDE.md каждой из них.
3. Прочитай найденный(е) `CLAUDE.md` целиком, прежде чем писать код — там задокументированы неймспейсы, обязательные паттерны (`ActivityLog::log`, `OrderHelper::create`, запрет `DbHelper`) и структура тестов.
4. Если задача не привязана ни к одной директории из таблицы (например, чисто инфраструктурный вопрос про `devops_deploy/`) — не подключай ничего лишнего, работай по корневому `CLAUDE.md`.
