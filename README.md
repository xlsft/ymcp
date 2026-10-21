# youtrack-mcp

Внешний MCP-сервер для self-hosted YouTrack на Go. Запускается локально, ходит в YouTrack
по REST API от имени пользователя (логин + пароль) и повторяет набор инструментов
встроенного MCP-сервера YouTrack: те же имена, параметры и лимиты пагинации.

Статус: **PoC**. Реализованы все 23 инструмента официального набора и проверен вход на живом инстансе.

## Инструменты

| Инструмент | Статус | Что делает |
|---|---|---|
| `search_issues` | ✅ | Поиск задач на языке запросов YouTrack, до 20 на страницу |
| `get_issue` | ✅ | Полная карточка задачи, последние комментарии, связи, вложения |
| `get_issue_comments` | ✅ | Комментарии задачи, до 10 на страницу |
| `add_issue_comment` | ✅ | Добавить комментарий (Markdown), можно с ограниченной видимостью |
| `get_issue_fields_schema` | ✅ | JSON Schema кастомных полей проекта с допустимыми значениями |
| `create_issue` | ✅ | Создать задачу; поля задаются по имени, `parentIssue` делает её подзадачей, `permittedUsers`/`permittedGroups` ограничивают видимость |
| `update_issue` | ✅ | Изменить summary, description, поля, звёздочку и голос |
| `change_issue_assignee` | ✅ | Сменить исполнителя, `null` снимает исполнителя |
| `find_projects` | ✅ | Найти проекты по подстроке имени или ключа |
| `get_project` | ✅ | Детали проекта, типы работ и настройки тайм-трекинга |
| `find_user` | ✅ | Найти пользователя по логину или email (`me` вернёт текущего) |
| `get_current_user` | ✅ | Текущий пользователь |
| `get_saved_issue_searches` | ✅ | Сохранённые поиски |
| `manage_issue_tags` | ✅ | Добавить или снять тег по ID или имени; если тег не найден, ошибка подскажет похожие |
| `link_issues` | ✅ | Связать две задачи (`subtask of`, `relates to`, `depends on`…), вернуть счётчики связей |
| `log_work` | ✅ | Списать время: минуты, дата, тип работы и атрибуты по имени |
| `create_draft_issue` | ✅ | Черновик задачи, виден только автору; дальше заполняется через `update_issue` по ID черновика |
| `find_user_groups` | ✅ | Найти группы и команды проектов по подстроке имени |
| `get_user_group_members` | ✅ | Участники группы или команды проекта (через Hub API) |
| `create_article` | ✅ | Статья базы знаний: родитель, ограниченная видимость по логинам и группам |
| `update_article` | ✅ | Изменить заголовок, текст (замена или дописывание в конец) или родителя |
| `get_article` | ✅ | Статья с подстатьями, тегами и видимостью; длинный текст читается кусками по строкам |
| `search_articles` | ✅ | Поиск по базе знаний со строками, где встретились слова запроса |

Кастомные поля передаются по отображаемому имени: `{"State": "Open", "Fix versions": ["1.0"]}`.
Сервер сам подбирает нужный `$type` (`StateIssueCustomField`, `MultiVersionIssueCustomField` и т. д.)
и проверяет значение по бандлу проекта. Даты принимаются как `yyyy-MM-dd` или RFC 3339,
а периоды — как `1w 2d 4h`.

## Сборка

Нужен Go 1.26+.

```bash
go build -o bin/youtrack-mcp ./cmd/youtrack-mcp
```

## Настройка

```bash
cp .env.example .env
```

| Переменная | Описание |
|---|---|
| `YOUTRACK_URL` | Адрес инстанса без `/api`, например `https://projectsystem.online` |
| `YOUTRACK_LOGIN` | Логин в YouTrack/Hub; LDAP-учётки тоже работают |
| `YOUTRACK_PASSWORD` | Пароль |
| `YOUTRACK_PROXY` | Пусто — брать `HTTP(S)_PROXY` из окружения, `none` — ходить напрямую, иначе URL прокси |

`.env` есть в `.gitignore`. Перед подключением к клиенту проверь, что вход работает:

```bash
./bin/youtrack-mcp -env .env -check
```

Если вход прошёл, команда выведет `ok: logged in to … as …`.

### Сеть: VPN, корпоративный DNS и прокси

YouTrack доступен только из внутренней сети, то есть через VPN или офисный Wi-Fi. Снаружи
`projectsystem.online` резолвится в публичную заглушку. Если в окружении задан
`HTTPS_PROXY`, который ведёт наружу, запросы уйдут мимо внутреннего DNS. В этом случае поставь
`YOUTRACK_PROXY=none`. Если вместо JSON пришла HTML-страница, сервер так и сообщит:
`unexpected "text/html" response … (check URL, VPN and proxy settings)`.

## Подключение к клиентам

**Claude Code:**

```bash
claude mcp add youtrack -- /abs/path/bin/youtrack-mcp -env /abs/path/.env
```

**Claude Desktop, Cursor и другие клиенты с JSON-конфигом:**

```json
{
  "mcpServers": {
    "youtrack": {
      "command": "/abs/path/bin/youtrack-mcp",
      "args": ["-env", "/abs/path/.env"]
    }
  }
}
```

**Streamable HTTP** (сервер слушает только localhost):

```bash
./bin/youtrack-mcp -env .env -transport http -addr 127.0.0.1:8090
```

Флаг `-v` включает подробные логи. Логи всегда пишутся в stderr, потому что stdout занят stdio-транспортом.

## Как устроена аутентификация

В Hub для password grant нужен secret сервиса, а у обычного пользователя его нет. Поэтому
сервер входит так же, как веб-интерфейс YouTrack:

1. Читает адрес Hub и ID сервиса YouTrack из публичного `GET /api/config`.
2. Отправляет логин и пароль в форму входа Hub (`/hub/api/rest/oauth2/interactive/login`) и получает cookie сессии Hub.
3. Проходит implicit OAuth flow веб-клиента YouTrack и забирает access token из фрагмента редиректа на `/oauth`.
4. Когда токен истекает (обычно через час), берёт новый по cookie сессии (`request_credentials=silent`) без повторной отправки пароля. Пароль отправляется заново только если сессия умерла. Если API ответил 401, сервер один раз входит заново.

Состав групп есть только в Hub API. Поэтому для `get_user_group_members` сервер получает в той же
сессии второй токен со scope Hub (`0-0-0-0-0`). Запрашивается он только при первом вызове, так что
если Hub его не выдаст, это затронет только этот инструмент.

Пароль хранится только в памяти процесса. Двухфакторная аутентификация (TOTP) пока
не поддерживается.

## Разработка

```bash
go test -race ./...
```

Тесты не ходят в сеть. `internal/youtrack/youtracktest` эмулирует YouTrack и Hub целиком: форму
входа, OAuth-редиректы, выдачу и отзыв токенов. Тесты инструментов вызывают их через
in-memory MCP-клиент и проверяют, какие REST-запросы в итоге уходят в YouTrack.

```
cmd/youtrack-mcp/          точка входа, флаги, загрузка .env
internal/youtrack/         REST-клиент и вход через Hub
internal/youtrack/youtracktest/  фейковый YouTrack + Hub для тестов
internal/tools/            MCP-инструменты
```
