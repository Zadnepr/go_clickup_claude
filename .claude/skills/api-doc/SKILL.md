---
name: api-doc
description: Создаёт или обновляет OpenAPI 3.0 документацию в panels/web/openapi/ для action-класса в adminRestApi, admin_mobile, user_mobile, adminRestApiClient или system_api. Используй сразу после написания нового endpoint'а или изменения существующего (новые параметры, схема ответа, коды ошибок) в одном из этих 5 модулей.
---

# api-doc

`panels/web/openapi/` — единственный источник правды для OpenAPI-документации. Тонкие прокси `panels/system_api/openapi/` и `panels/user_mobile_api/openapi/` содержат только `index.php`, который отдаёт файлы из `web/openapi/` (подменяя часть пути `system_api`/`user_mobile_api` на `web`) — их редактировать не нужно.

## Шаг 0 — определить модуль и его docs-root

| Модуль (namespace / директория с action-классами) | Docs-root | Корневой файл | Где перечислены paths |
|---|---|---|---|
| `adminRestApi\` (`panels/modules/adminRestApi/controllers/actions/`) | `web/openapi/adminrest/v1/` | `openapi.yaml` | `paths.yaml` (список `$ref`) |
| `admin_mobile\` (`panels/modules/admin_mobile/controllers/actions/`) | `web/openapi/admin_mobile/v1/` | `openapi.yaml` | `paths.yaml` |
| `user_mobile\` (`panels/modules/user_mobile/controllers/actions/`) | `web/openapi/user_app/v1/` | `index.yaml` | пути перечислены прямо в `index.yaml` (отдельного `paths.yaml` нет) |
| `adminRestApiClient\` (`panels/modules/adminRestApiClient/controllers/actions/`) | `web/openapi/adminapi/v2/` | **два файла**: правишь `src/index.yaml` + `src/<resource>/*.yaml`, реально отдаётся монолитный `index.yaml` (~3900 строк) | пути в `src/index.yaml` |
| `system_api\` (`panels/modules/system_api/controllers/actions/`) | `web/openapi/system/v1/` | `index.yaml` | пути перечислены прямо в `index.yaml` |

Общие компоненты (schemas/responses/requestBodies) лежат рядом с корневым файлом каждого модуля: `schemas.yaml`, `responses.yaml`, `requestBodies.yaml` — не у всех модулей есть все три (см. таблицу ниже).

| Модуль | schemas.yaml | responses.yaml | requestBodies.yaml |
|---|---|---|---|
| adminrest/v1 | ✅ | ✅ | ✅ |
| admin_mobile/v1 | ✅ | ✅ | — (тело инлайново) |
| user_app/v1 | — (схемы инлайновые в каждом файле) | ✅ | — |
| adminapi/v2 (src/) | ✅ | ✅ | — |
| system/v1 | — (схемы инлайновые) | ✅ | — |

## Шаг 1 — найти route и понять контракт

1. Найди регистрацию action в контроллере (`actions()` в соответствующем `Controller.php`) и маршрут в `config/routes.php` — так узнаёшь HTTP-метод и URL.
2. Прочитай action-класс и его Form-модель (если есть) — `rules()` дают параметры запроса и их типы/ограничения, возвращаемый массив — форму успешного ответа.
3. Найди 2-3 похожих существующих yaml-файла рядом (тот же контроллер/ресурс) и скопируй их структуру: `tags`, стиль `summary`/`description`, `security` (в adminrest/v1 это устойчиво `security: [{key: [], admin_id: []}]` — повторяй существующий паттерн модуля, не выдумывай новый), общие `$ref` на `responses.yaml`/`schemas.yaml` вместо дублирования инлайн-схем.

## Шаг 2 — создать/обновить yaml и подключить в root

- Новый endpoint → отдельный `.yaml`-файл в подпапке, соответствующей ресурсу/контроллеру (например `orders/bulk-status.yaml`), плюс строка `$ref` в `paths.yaml` (adminrest, admin_mobile) или прямо в `index.yaml` (user_app, system, adminapi/v2 src).
- Изменение существующего endpoint'а → правь его файл на месте: добавь новые параметры/поля ответа, обнови `description`, добавь недостающие коды ошибок в `responses`.
- Переиспользуемые модели данных — в `schemas.yaml` модуля (если он есть), не дублируй inline.

### adminRestApiClient (`adminapi/v2`) — особый случай

Редактируй только `src/index.yaml` + `src/<resource>/*.yaml` (`src/schemas.yaml`, `src/responses.yaml`, `src/parameters.yaml` — общие). Затем собери в монолитный `index.yaml`, который реально отдаётся:

```bash
cd panels/web/openapi/adminapi/v2
npx --yes @redocly/cli bundle src/index.yaml -o index.yaml
```

**Важно:** в репозитории нет закоммиченного скрипта/CI-шага для этого бандла — команда выше не подтверждена как официальный процесс проекта. Перед тем как перезаписывать `index.yaml`, посмотри `git diff` — если в нём есть правки, внесённые напрямую в монолитный файл без соответствующих изменений в `src/`, бандл их затрёт. В таком случае перенеси эти правки в `src/` вручную перед бандлом.

## Шаг 3 — валидация

```bash
npx --yes @redocly/cli lint <путь-к-корневому-yaml-модуля>
```

Проверяет синтаксис и битые `$ref`. Прогоняй на файле, который реально отдаётся по HTTP (для adminapi/v2 — на `index.yaml`, а не на `src/index.yaml`).

## Итог

Не считай задачу выполненной, если `redocly lint` падает с ошибкой, или если для нового action-класса из перечисленных 5 модулей нет соответствующего yaml-файла.
