---
name: test-for-change
description: Находит существующие Codeception-тесты для изменённого/нового PHP-кода в panels/ или sommerce/, запускает их и чинит регрессии, либо создаёт новый тест по конвенциям проекта, если тестов ещё нет. Используй сразу после написания или правки PHP-кода в этих проектах — это прямая реализация обязательных правил №1 и №2 из корневого CLAUDE.md.
---

# test-for-change

Реализует обязательные правила из корневого `CLAUDE.md`:
1. Новый код → обязательно новый Codeception-тест.
2. Изменённый код → найти покрывающие тесты, прогнать, починить регрессии.

Задача не считается выполненной, если тесты остались красными или для нового кода нет теста.

## Шаг 0 — определить проект и модуль

Проверь путь изменённого файла:

| Путь содержит | Проект | Правило из CLAUDE.md |
|---|---|---|
| `panels/components/api/actions/` | panels | User API → `panels/tests/unit/api/` |
| `panels/components/adminapi/actions/` | panels | Admin API v1 → `panels/tests/unit/adminapi/` |
| `panels/modules/adminRestApi/` | panels | Admin REST API → `panels/tests/unit/restapi/` |
| `panels/modules/adminRestApiClient/` | panels | Admin API v2 → `panels/tests/unit/adminapiv2/` |
| `panels/components/cron/` | panels | Cron → `panels/tests/unit/cron/` |
| `panels/helpers/` | panels | Хелпер → `panels/tests/unit/helpers/` |
| `panels/components/`, `panels/models/panel*/services/` | panels | Сервис/компонент → `panels/tests/unit/components/` или `panels/tests/unit/services/` |
| `sommerce/superadmin/modules/adminRestApi/` | sommerce | → `sommerce/superadmin/tests/unit/adminRestApi/` |
| `sommerce/my/...` | sommerce | → `sommerce/my/tests/unit/` |
| `sommerce/console/` cron-классы | sommerce | → `sommerce/superadmin/tests/unit/crons/` |

Если путь не подходит ни под одну строку — спроси пользователя или посмотри `{project}/CLAUDE.md` раздел "Тестирование".

## Шаг 1 — найти существующие тесты

```bash
grep -rl "ClassName\|methodName" tests/unit/          # panels
grep -rl "ClassName\|methodName" superadmin/tests/unit/ my/tests/unit/   # sommerce
```

Ищи по имени изменённого класса/метода и по именам моделей/хелперов, которые он использует.

## Шаг 2 — прогнать найденные тесты

**panels** (из `panels/`):
```bash
php vendor/bin/codecept run unit <suite>:<TestClass>          # один файл
php vendor/bin/codecept run unit <suite>:<TestClass>:testName # один метод
php vendor/bin/codecept run unit                               # весь unit-сьют (долго)
```

**sommerce** — 3 отдельных конфига (из `sommerce/`):
```bash
php vendor/bin/codecept run unit adminRestApi              # superadmin (по умолчанию)
php vendor/bin/codecept run -c my unit <suite>              # my
php vendor/bin/codecept run -c systemapi unit <suite>       # systemapi
```

В Docker (если локально нет PHP/codecept): оберни команду в
```bash
docker exec -i panels_php84 bash -c "cd /var/www/<panels|sommerce> && <команда>"
```

Если тестовые БД не инициализированы — сначала `sh tests/migrate.sh` (panels).

## Шаг 3 — если тестов нет, создать новый

Выбери базовый класс:

- `BasePanelUnitTest` (panels, `tests/_support/`) — даёт `getPanel()`/`getAdmin()`. Для action-классов, компонентов, хелперов.
- `AbstractRestapiBaseUnit` / `AbstractFormActionTest` / `AbstractListActionTest` (panels, `tests/unit/restapi/base/`) — для adminRestApi actions.
- Для sommerce — `Codeception\Test\Unit` напрямую + фабрики `superadmin/tests/helpers/*DataHelper.php` (`UserDataHelper::createCustomer()`, `PanelDataHelper::createPanel()` и т.п.) и `_before()`/`_after()` с очисткой созданных записей.

Тест должен покрывать: успешный сценарий, граничные условия, основные ошибки (invalid input, not found, access denied). Используй `#[Group('...')]` / `@group` и `@covers`, как в существующих тестах рядом.

**Важно:** не хардкодить локальные имена панельных БД в DSN/запросах — использовать константы тестового окружения `DB_TEST_PANEL1..3` / `DB_PANELS` (см. `tests/_bootstrap.php`).

## Шаг 4 — итог

Прогони изменённый/новый тест ещё раз и покажи результат. Если что-то красное — почини тест или код, вызвавший регрессию, а не удаляй/скипай тест.
