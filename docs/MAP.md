# Карта репозитория

## Найти нужное быстро

| Задача / ключевые слова | Начать здесь | Проверки рядом |
|---|---|---|
| Циклы, проходы, продолжение, prompt | [engine.go](../internal/app/engine.go), [quality.go](../internal/app/quality.go) | `app_test.go`, `iteration_test.go` |
| CLI, подписка, поток ответов, модели | [agents.go](../internal/app/agents.go), [models.go](../internal/app/models.go) | `app_test.go`, `process_test.go` |
| Лимит, вход, доступность, проверка Claude | [health.go](../internal/app/health.go), [health_cli.go](../internal/app/health_cli.go), [access_check.go](../internal/app/access_check.go) | `health_test.go`, `access_check_test.go` |
| Работа одним агентом, независимое ревью | [availability.go](../internal/app/availability.go), [independent_review.go](../internal/app/independent_review.go) | `availability_test.go` |
| Папки, перенос, копия, machine paths | [library.go](../internal/app/library.go), [project_transfer.go](../internal/app/project_transfer.go), [project_folder.go](../internal/app/project_folder.go) | `library_test.go` |
| БД, миграция, сохранение, перезапуск | [store.go](../internal/app/store.go), [recovery.go](../internal/app/recovery.go) | `recovery_test.go`, `library_test.go` |
| Потерянный HTTP-ответ, повтор запроса | [requests.go](../internal/app/requests.go), [client.ts](../web/src/client.ts) | `recovery_test.go`, `reliability.test.ts` |
| Материалы, загрузка MD/PDF/DOCX | [materials.go](../internal/app/materials.go), [server.go](../internal/app/server.go), [main.tsx](../web/src/main.tsx) | `app_test.go`, `library_test.go` |
| Редакции, утверждение, внешний Markdown | [revisions.go](../internal/app/revisions.go), [project_documents.go](../internal/app/project_documents.go) | `prototype_test.go`, `library_test.go` |
| Прототип, план, команды, Git checkpoint | [prototype.go](../internal/app/prototype.go), [prototype_workspace.go](../internal/app/prototype_workspace.go), [prototype_checks.go](../internal/app/prototype_checks.go) | `prototype_test.go` |
| UI папок / здоровья / разработки | [projects.tsx](../web/src/projects.tsx), [health.tsx](../web/src/health.tsx), [development.tsx](../web/src/development.tsx) | [SSR](../web/tests/health.test.tsx) |
| Черновики, localStorage, сравнение | [drafts.ts](../web/src/drafts.ts), [diff.ts](../web/src/diff.ts) | `reliability.test.ts`, `diff.test.ts` |
| Запуск, embedded UI, сборка | [main.go](../main.go), [scripts](../scripts), [package.json](../package.json) | TypeScript/Vite, Go build |

Искать сначала в выбранном модуле, например `rg -n 'transfer|relocate' internal/app/library*.go web/src/projects.tsx`. Для новых понятий расширять поиск на исходники, а не читать все журналы. Карта указывает точки входа, не заменяет проверку фактических вызовов.

## Папки и файлы

| Путь | Содержимое / переносимость |
|---|---|
| `AGENTS.md`, `CLAUDE.md` | Общие инструкции; Claude импортирует тот же текст. В Git. |
| `README.md` | Запуск и возможности для пользователя. |
| `docs/` | Состояние, карта, передача, архитектура, план и результаты проверок. |
| `docs/decisions/` | Причины существенных решений; новые решения могут заменять прежние с явной ссылкой. |
| `docs/worklog/` | Короткие записи по задачам/сессиям. Отдельные файлы уменьшают конфликты. |
| `.agents/skills/` | Исходники проектных skills Codex. |
| `.claude/skills/` | Генерируемые идентичные копии для Claude; тоже в Git, без symlink. |
| `.github/workflows/` | Проверки при push/PR; не содержат авторизацию CLI. |
| `internal/app/` | Сервер Go и его тесты `*_test.go`. |
| `web/src/` | React/TypeScript, стили и Node-тесты рядом с модулями. |
| `web/tests/` | Серверный рендеринг компонентов для проверки UI-состояний. |
| `web/public/` | Статические браузерные ресурсы. |
| `scripts/` | Команды сборки и проверок. Новые Node-скрипты работают без npm-зависимостей. |
| `examples/` | Демонстрационные материалы без пользовательских данных. |
| `go.mod`, `go.sum`, `package.json`, `package-lock.json` | Зависимости и команды. Lock-файлы сохраняются в Git. |
| `.git/` | История и локальные настройки Git; не редактировать вручную. |
| `.tandem/`, `.tandem-preview/` | Локальные пользовательские/тестовые базы; исключены из Git. |
| `.tools/`, `node_modules/` | Восстанавливаемые кеши и зависимости; исключены из Git. |
| `web/dist/`, `release/`, корневой `tandem` | Результаты сборки; исключены из Git, кроме `web/dist/placeholder.txt` для Go embed. |

Папка пользовательского проекта с `tandem.project.json`, `docs/`, `materials/`, `prototypes/`, `.tandem/` — отдельные данные приложения. Она не является этим репозиторием разработки.
