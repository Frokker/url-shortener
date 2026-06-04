# HTTP API

[← К оглавлению](./README.md) · Связанные: [architecture.md](./architecture.md) · [data-model.md](./data-model.md) · [configuration.md](./configuration.md)

Базовый префикс API: `/api/v1`. Редирект и health-чеки живут в корне.
Все тела запросов/ответов — `application/json; charset=utf-8`, кроме редиректа.

## Содержание

- [Сводка эндпоинтов](#сводка-эндпоинтов)
- [Формат ошибки](#формат-ошибки)
- [POST /api/v1/links — создать](#post-apiv1links--создать)
- [GET /{code} — редирект](#get-code--редирект)
- [GET /api/v1/links/{code} — метаданные](#get-apiv1linkscode--метаданные)
- [DELETE /api/v1/links/{code} — удалить](#delete-apiv1linkscode--удалить)
- [GET /api/v1/links/{code}/stats — аналитика (Этап 2)](#get-apiv1linkscodestats--аналитика-этап-2)
- [GET /healthz и /readyz](#get-healthz-и-readyz)
- [GET /metrics (Этап 2)](#get-metrics-этап-2)
- [Rate limiting (Этап 2)](#rate-limiting-этап-2)
- [Идемпотентность](#идемпотентность)

## Сводка эндпоинтов

| Метод | Путь | Этап | Назначение |
|---|---|---|---|
| POST | `/api/v1/links` | 1 | Создать короткую ссылку |
| GET | `/{code}` | 1 | Редирект (302) на оригинал, учёт клика |
| GET | `/api/v1/links/{code}` | 1 | Метаданные ссылки |
| DELETE | `/api/v1/links/{code}` | 1 | Удалить ссылку |
| GET | `/healthz` | 1 | Liveness |
| GET | `/readyz` | 1 | Readiness (пинг Postgres + Redis) |
| GET | `/api/v1/links/{code}/stats` | 2 | Расширенная аналитика по времени |
| GET | `/metrics` | 2 | Метрики Prometheus |

## Формат ошибки

Единый JSON для всех ошибок:

```json
{
  "error": {
    "code": "ALIAS_TAKEN",
    "message": "custom alias is already in use"
  }
}
```

| HTTP | `code` | Когда |
|---|---|---|
| 400 | `INVALID_REQUEST` | битый JSON, пустой/невалидный URL, плохой алиас, отрицательный ttl |
| 404 | `NOT_FOUND` | кода нет в БД |
| 409 | `ALIAS_TAKEN` | кастомный алиас занят |
| 410 | `GONE` | ссылка просрочена (`expires_at` в прошлом) |
| 429 | `RATE_LIMITED` | превышен лимит запросов (Этап 2) |
| 500 | `INTERNAL` | непредвиденная ошибка |
| 503 | `NOT_READY` | `/readyz`: Postgres или Redis недоступны |

Маппинг доменных ошибок (`ErrNotFound`, `ErrExpired`, `ErrAliasTaken`, ...) на эти коды
делает **только handler** через `errors.Is`.

## POST /api/v1/links — создать

Запрос:

```json
{
  "url": "https://example.com/very/long/path?a=1",
  "custom_alias": "promo",
  "ttl_seconds": 3600
}
```

| Поле | Тип | Обязательность | Правила |
|---|---|---|---|
| `url` | string | да | непустой, схема `http`/`https` |
| `custom_alias` | string | нет | `^[A-Za-z0-9_-]{3,32}$` |
| `ttl_seconds` | integer | нет | `>= 0`; `0`/отсутствует = бессрочно |

Ответ `201 Created` (новая ссылка):

```json
{
  "code": "promo",
  "short_url": "http://localhost:8080/promo",
  "expires_at": "2026-06-04T13:00:00Z"
}
```

Ответ `200 OK` (идемпотентный повтор того же `url` без алиаса — вернулась существующая):

```json
{
  "code": "aZ3xQ",
  "short_url": "http://localhost:8080/aZ3xQ",
  "expires_at": null
}
```

| Статус | Когда |
|---|---|
| 201 | создана новая ссылка |
| 200 | тот же URL уже существовал → возвращён существующий код (идемпотентность) |
| 400 | `INVALID_REQUEST` |
| 409 | `ALIAS_TAKEN` — кастомный алиас занят |
| 429 | `RATE_LIMITED` (Этап 2) |

curl:

```bash
curl -s -X POST http://localhost:8080/api/v1/links \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com","custom_alias":"promo","ttl_seconds":3600}'
```

## GET /{code} — редирект

Главный горячий путь. Возвращает **302 Found** (см. обоснование 301 vs 302 в
[architecture.md](./architecture.md#решение-301-vs-302)).

```
HTTP/1.1 302 Found
Location: https://example.com/very/long/path?a=1
```

| Статус | Когда |
|---|---|
| 302 | найдено и не просрочено → `Location: original_url` |
| 404 | `NOT_FOUND` — кода нет |
| 410 | `GONE` — ссылка просрочена |
| 429 | `RATE_LIMITED` (Этап 2) |

Поведение по этапам:
- **Этап 1**: перед ответом синхронно делается `UPDATE click_count + 1` (блокирует).
- **Этап 2**: событие клика кладётся в буферизованный канал, ответ отдаётся сразу; при
  полном буфере событие дропается (метрика), редирект всё равно мгновенный.

curl (не следовать редиректу, показать заголовки):

```bash
curl -s -i http://localhost:8080/promo
```

## GET /api/v1/links/{code} — метаданные

Ответ `200 OK`:

```json
{
  "code": "promo",
  "original_url": "https://example.com",
  "clicks": 142,
  "is_custom": true,
  "created_at": "2026-06-04T12:00:00Z",
  "expires_at": "2026-06-04T13:00:00Z"
}
```

| Статус | Когда |
|---|---|
| 200 | найдено (даже если просрочено — метаданные отдаём) |
| 404 | `NOT_FOUND` |

> `clicks` — это денормализованный `click_count`. На Этапе 2 он может отставать на величину
> до одного окна флаша (`FLUSH_INTERVAL_T`) — это ожидаемое следствие батчинга.

curl:

```bash
curl -s http://localhost:8080/api/v1/links/promo | jq
```

## DELETE /api/v1/links/{code} — удалить

| Статус | Когда |
|---|---|
| 204 | удалено |
| 404 | `NOT_FOUND` |

Удаление инвалидирует запись в Redis-кэше. На Этапе 2 `ON DELETE CASCADE` чистит и
`click_aggregates`.

curl:

```bash
curl -s -i -X DELETE http://localhost:8080/api/v1/links/promo
```

## GET /api/v1/links/{code}/stats — аналитика (Этап 2)

Агрегаты по времени из `click_aggregates` (питается Kafka-конвейером).
Параметры запроса: `from`, `to` (RFC3339), `bucket` (`hour`|`minute`, по умолчанию `hour`).

```bash
curl -s 'http://localhost:8080/api/v1/links/promo/stats?from=2026-06-04T00:00:00Z&to=2026-06-04T23:59:59Z&bucket=hour' | jq
```

Ответ `200 OK`:

```json
{
  "code": "promo",
  "total": 142,
  "bucket": "hour",
  "series": [
    { "ts": "2026-06-04T12:00:00Z", "clicks": 90 },
    { "ts": "2026-06-04T13:00:00Z", "clicks": 52 }
  ]
}
```

| Статус | Когда |
|---|---|
| 200 | найдено |
| 400 | `INVALID_REQUEST` — кривые `from`/`to`/`bucket` |
| 404 | `NOT_FOUND` |

## GET /healthz и /readyz

| Эндпоинт | Назначение | Ответ |
|---|---|---|
| `GET /healthz` | liveness — процесс жив | `200 {"status":"ok"}` всегда, без проверки зависимостей |
| `GET /readyz` | readiness — готов принимать трафик | `200` если Postgres **и** Redis отвечают на ping; иначе `503 NOT_READY` |

```json
// 200 /readyz
{ "status": "ready", "checks": { "postgres": "ok", "redis": "ok" } }
```

```json
// 503 /readyz
{ "status": "not_ready", "checks": { "postgres": "ok", "redis": "fail" } }
```

`/healthz` не должен дёргать БД — иначе оркестратор будет рестартить под при временной
недоступности зависимости. Зависимости проверяет только `/readyz`.

## GET /metrics (Этап 2)

Экспозиция Prometheus в text-формате. Список метрик — в [operations.md](./operations.md#метрики-prometheus).

```bash
curl -s http://localhost:8080/metrics | grep urlshort_
```

## Rate limiting (Этап 2)

Token-bucket middleware на **всём роутере** (отдельной ручки нет). Ключ — IP или API-ключ
(см. [configuration.md](./configuration.md)). При превышении — `429`.

Заголовки на каждом ответе:

| Заголовок | Пример | Смысл |
|---|---|---|
| `X-RateLimit-Limit` | `100` | размер бакета (запросов в окно) |
| `X-RateLimit-Remaining` | `87` | сколько токенов осталось |
| `X-RateLimit-Reset` | `1717502400` | unix-время пополнения |
| `Retry-After` | `1` | (только при 429) через сколько секунд повторить |

```json
// 429
{ "error": { "code": "RATE_LIMITED", "message": "too many requests" } }
```

## Идемпотентность

- **POST /api/v1/links без `custom_alias`**: повтор с тем же `url` возвращает **тот же `code`**
  (через `UNIQUE(url_hash)`), статус `200` вместо `201`. См.
  [data-model.md](./data-model.md#идемпотентность-создания).
- **POST с `custom_alias`**: повтор с занятым алиасом → `409 ALIAS_TAKEN` (это не
  идемпотентность, а конфликт уникального ключа).
- **DELETE**: повторное удаление отсутствующего кода → `404` (не `204`); операция не
  «идемпотентна» в смысле возврата того же кода, но безопасна для повторов.
- **GET /{code}**: на Этапе 1 каждый успешный редирект меняет состояние (счётчик), поэтому
  он не безопасен как «pure GET» — это часть учебной боли.
